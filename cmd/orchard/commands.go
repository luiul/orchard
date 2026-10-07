package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/luiul/orchard/internal/artifacts"
	"github.com/luiul/orchard/internal/catalog"
	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/pipeline"
	"github.com/luiul/orchard/internal/probe"
	"github.com/luiul/orchard/internal/progress"
	"github.com/luiul/orchard/internal/report"
	"github.com/luiul/orchard/internal/router"
)

// dispatch runs a validated subcommand. It returns the process exit code.
func dispatch(cfg config, stdout, stderr io.Writer) int {
	switch cfg.command {
	case "fetch":
		return commandFetch(cfg.args, stdout, stderr)
	case "probe":
		return commandProbe(cfg.args, stdout, stderr)
	case "report":
		return commandReport(cfg.args, stdout, stderr)
	case "sync":
		return commandSync(cfg.args, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "orchard: unknown command %q\n", cfg.command)
	return 2
}

var (
	resolveConfig   = paths.Load
	executePipeline = pipeline.Run
)

// loadConfig reports invalid configuration with exit code 2.
func loadConfig(stderr io.Writer) (paths.Config, int) {
	cfg, err := resolveConfig()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return paths.Config{}, 2
	}
	return cfg, 0
}

// runPipeline cancels provider calls and Pi probes when the command stops.
func runPipeline(cfg paths.Config, doProbe, strict bool, stderr io.Writer) (*pipeline.Pipeline, int) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx = progress.WithReporter(ctx, func(message string) {
		_, _ = fmt.Fprintln(stderr, "orchard: "+message)
	})
	p, err := executePipeline(ctx, cfg, doProbe, strict)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return nil, 1
	}
	return p, 0
}

// checkCircuit enforces the circuit-breaker contract: a tripped breaker
// means nothing was written and the run is NOT auto-retried.
func checkCircuit(p *pipeline.Pipeline, stderr io.Writer) int {
	if p.Probes != nil && p.Probes.Tripped {
		_, _ = fmt.Fprintln(stderr, "circuit breaker tripped: too many consecutive systemic probe failures; nothing was written")
		shown := 0
		for _, outcome := range p.Probes.Outcomes {
			if outcome.Status == probe.FAIL && outcome.Systemic {
				_, _ = fmt.Fprintf(stderr, "  %s/%s: %s\n", outcome.Provider, outcome.ID, outcome.Reason)
				shown++
				if shown == p.Cfg.ProbeFailCircuit {
					break
				}
			}
		}
		_, _ = fmt.Fprintln(stderr, "fix the reported failures before rerunning; Orchard did not run aws sso login")
		return 1
	}
	return 0
}

func generatedAt() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func commandFetch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	catalogOnly := fs.Bool("catalog-only", false, "Only parse the pi catalog, skip AWS and the router.")
	strict := fs.Bool("strict", false, "Fail when the router is unreachable instead of degrading.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *catalogOnly {
		_, _ = fmt.Fprintln(stderr, "orchard: loading Pi model catalog")
		entries, err := catalog.Load(context.Background())
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "%-16s  %-52s  %-8s  %-7s  %-8s  %s\n", "provider", "model", "context", "max-out", "thinking", "images")
		for _, e := range entries {
			_, _ = fmt.Fprintf(stdout, "%-16s  %-52s  %-8s  %-7s  %-8s  %s\n", e.Provider, e.ID, e.Context, e.MaxOut, e.Thinking, e.Images)
		}
		_, _ = fmt.Fprintf(stderr, "orchard: loaded %d Pi catalog models\n", len(entries))
		_, _ = fmt.Fprintf(stdout, "%d catalog row(s)\n", len(entries))
		return 0
	}

	cfg, code := loadConfig(stderr)
	if code != 0 {
		return code
	}
	p, code := runPipeline(cfg, false, *strict, stderr)
	if code != 0 {
		return code
	}
	_, _ = fmt.Fprintln(stdout, report.Summarize(p))
	for _, region := range cfg.Regions() {
		listed, ok := p.Listings.ByRegion[region]
		if !ok {
			continue
		}
		nCand := 0
		for _, r := range p.BedrockCandidates {
			if r == region {
				nCand++
			}
		}
		_, _ = fmt.Fprintf(stdout, "  %s: %d listed, %d candidate(s) assigned to probe from here\n", region, len(listed), nCand)
	}
	if p.Router.Reachable {
		_, _ = fmt.Fprintf(stdout, "  router: %d deployed: %s\n", len(p.Router.Deployed), strings.Join(p.Router.Deployed, ", "))
	}
	return 0
}

func commandProbe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Int("timeout", 0, "Seconds before a probe is killed and counted systemic. (env PROBE_TIMEOUT)")
	concurrency := fs.Int("concurrency", 0, "How many probes run at once. (env PROBE_CONCURRENCY)")
	failCircuit := fs.Int("fail-circuit", 0, "Consecutive systemic failures that trip the circuit breaker. (env PROBE_FAIL_CIRCUIT)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var invalidFlag string
	values := map[string]int{"timeout": *timeout, "concurrency": *concurrency, "fail-circuit": *failCircuit}
	fs.Visit(func(f *flag.Flag) {
		if value, ok := values[f.Name]; ok && value <= 0 {
			invalidFlag = f.Name
		}
	})
	if invalidFlag != "" {
		_, _ = fmt.Fprintf(stderr, "--%s must be positive\n", invalidFlag)
		return 2
	}
	cfg, code := loadConfig(stderr)
	if code != 0 {
		return code
	}
	// Command-line tuning wins over environment defaults.
	if *timeout > 0 {
		cfg.ProbeTimeout = *timeout
	}
	if *concurrency > 0 {
		cfg.ProbeConcurrency = *concurrency
	}
	if *failCircuit > 0 {
		cfg.ProbeFailCircuit = *failCircuit
	}
	p, code := runPipeline(cfg, true, false, stderr)
	if code != 0 {
		return code
	}
	ok := 0
	for _, outcome := range p.Probes.Outcomes {
		if outcome.Status == probe.OK {
			ok++
		}
	}
	_, _ = fmt.Fprintf(stdout, "%d/%d probes OK\n", ok, len(p.Probes.Outcomes))
	return checkCircuit(p, stderr)
}

func commandReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Emit the report as JSON for scripting.")
	noProbe := fs.Bool("no-probe", false, "Discover candidates only; their probe status stays unknown.")
	strict := fs.Bool("strict", false, "Fail when the router is unreachable instead of degrading.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, code := loadConfig(stderr)
	if code != 0 {
		return code
	}
	p, code := runPipeline(cfg, !*noProbe, *strict, stderr)
	if code != 0 {
		return code
	}
	rows := p.Rows()
	drift := p.Drift()
	if *asJSON {
		text, err := report.JSON(p, rows, drift, generatedAt())
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		_, _ = fmt.Fprint(stdout, text)
		return checkCircuit(p, stderr)
	}
	printReport(stdout, p, rows, drift)
	return checkCircuit(p, stderr)
}

// printReport renders the human report: summary, table, drift sections.
func printReport(stdout io.Writer, p *pipeline.Pipeline, rows []model.Row, drift model.Drift) {
	_, _ = fmt.Fprintln(stdout, report.Summarize(p))
	_, _ = fmt.Fprintln(stdout, report.Table(rows))
	for _, section := range report.DriftSections(drift) {
		_, _ = fmt.Fprintf(stdout, "\n%s (%d) -- %s:\n", section.Title, len(section.Items), section.Hint)
		for _, item := range section.Items {
			_, _ = fmt.Fprintf(stdout, "  %s\n", item)
		}
	}
}

func commandSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "Probe and preview changes, but do not write files.")
	strict := fs.Bool("strict", false, "Fail when the router is unreachable.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, code := loadConfig(stderr)
	if code != 0 {
		return code
	}
	if warning := artifacts.SettingsDrift(cfg.PISettings, cfg.LiveSettings); warning != "" {
		_, _ = fmt.Fprintf(stderr, "settings drift: %s\n", warning)
		return 1
	}
	p, code := runPipeline(cfg, true, *strict, stderr)
	if code != 0 {
		return code
	}
	if code := checkCircuit(p, stderr); code != 0 {
		return code
	}
	rows := p.Rows()
	drift := p.Drift()
	printReport(stdout, p, rows, drift)

	if warning := artifacts.SettingsDrift(cfg.PISettings, cfg.LiveSettings); warning != "" {
		_, _ = fmt.Fprintf(stderr, "settings drift: %s\n", warning)
		return 1
	}
	patterns, err := artifacts.ReadPatterns(cfg.PISettings)
	if err != nil || !reflect.DeepEqual(patterns, p.Patterns) {
		_, _ = fmt.Fprintln(stderr, "sync refused: model scope changed while probing; files unchanged")
		return 1
	}
	invocable := p.InvocableBedrock()
	currentBedrock, err := os.ReadFile(cfg.BedrockModelsJSON)
	if err != nil && !os.IsNotExist(err) {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	previous, err := artifacts.BedrockMap(currentBedrock)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := p.SyncReady(previous); err != nil {
		_, _ = fmt.Fprintf(stderr, "sync refused: %v; files unchanged\n", err)
		return 1
	}
	bedrockChanges, err := artifacts.PreviewBedrock(currentBedrock, invocable)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	var routerChanges string
	data, err := os.ReadFile(cfg.ModelsJSON)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read %s: %v\n", cfg.ModelsJSON, err)
		return 1
	}
	if string(data) != p.RouterConfig {
		_, _ = fmt.Fprintln(stderr, "sync refused: models.json changed after probes started; files unchanged")
		return 1
	}
	newModels := make([]router.Entry, 0, len(p.Router.Deployed))
	for _, id := range p.Router.Deployed {
		newModels = append(newModels, p.Router.Entries[id].Entry)
	}
	newModelsText, err := artifacts.RenderModelsJSON(string(data), newModels)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	routerChanges, err = artifacts.PreviewRouter(string(data), newModelsText)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "\nProposed Bedrock region map changes:")
	printChanges(stdout, bedrockChanges)
	_, _ = fmt.Fprintln(stdout, "Proposed router model changes:")
	printChanges(stdout, routerChanges)
	if *dryRun {
		_, _ = fmt.Fprintln(stdout, "Preview only. No files changed.")
		return 0
	}

	// Each file replacement is atomic; the two files are not one transaction.
	stamp := generatedAt()
	newBedrock, err := artifacts.RenderBedrockModels(invocable, cfg.DefaultRegion, stamp)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := artifacts.AtomicWrite(cfg.BedrockModelsJSON, newBedrock); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "\nWrote %d verified models to %s\n", len(invocable), cfg.BedrockModelsJSON)
	if err := artifacts.AtomicWrite(cfg.ModelsJSON, newModelsText); err != nil {
		_, _ = fmt.Fprintf(stderr, "router write failed after the Bedrock map changed: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "Updated %d deployed router models in %s\n", len(newModels), cfg.ModelsJSON)
	_, _ = fmt.Fprintf(stdout, "  metadata sources per field: %s\n", metadataSourcesSummary(p.Router.Entries))
	_, _ = fmt.Fprintf(stdout, "enabledModels: %d curated pattern(s) read, not modified\n", len(p.Patterns))
	return 0
}

func printChanges(out io.Writer, changes string) {
	if changes == "" {
		_, _ = fmt.Fprintln(out, "  (no ID or region changes)")
		return
	}
	_, _ = fmt.Fprintln(out, changes)
}

// metadataSourcesSummary renders the per-field provenance counts:
// "contextWindow: router x13; cost: override x1, router x12; ...".
func metadataSourcesSummary(entries map[string]router.ModelEntry) string {
	quality := map[string]map[string]int{}
	for _, entry := range entries {
		for field, source := range entry.Sources {
			if quality[field] == nil {
				quality[field] = map[string]int{}
			}
			quality[field][source]++
		}
	}
	fields := make([]string, 0, len(quality))
	for field := range quality {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		sources := make([]string, 0, len(quality[field]))
		for source := range quality[field] {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		counts := make([]string, 0, len(sources))
		for _, source := range sources {
			counts = append(counts, fmt.Sprintf("%s x%d", source, quality[field][source]))
		}
		parts = append(parts, field+": "+strings.Join(counts, ", "))
	}
	return strings.Join(parts, "; ")
}
