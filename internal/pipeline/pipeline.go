// Package pipeline discovers models in Pi, AWS, and the router, then probes
// candidates before claiming that they can be invoked. Fetch and report never
// change Pi's configuration; sync writes artifacts only after a complete run.
package pipeline

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/luiul/orchard/internal/artifacts"
	"github.com/luiul/orchard/internal/bedrock"
	"github.com/luiul/orchard/internal/catalog"
	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/probe"
	"github.com/luiul/orchard/internal/progress"
	"github.com/luiul/orchard/internal/router"
)

// RouterUnreachableError means --strict was given and the router is
// unreachable.
type RouterUnreachableError struct{ Detail string }

func (e *RouterUnreachableError) Error() string {
	return "router unreachable and --strict given: " + e.Detail
}

// External seams, swapped in tests (the convention for every external call
// in this repo's family).
var (
	loadCatalog     = catalog.Load
	checkSSO        = bedrock.CheckSSO
	fetchListings   = bedrock.FetchListings
	fetchRouter     = router.Fetch
	loadRegistry    = router.LoadRegistry
	readPatterns    = artifacts.ReadPatterns
	resolveScope    = catalog.ResolveScope
	probeAll        = probe.All
	readModelConfig = os.ReadFile
)

// Pipeline holds the source data and model status from one run.
type Pipeline struct {
	Cfg               paths.Config
	Catalog           []model.CatalogEntry
	Listings          bedrock.Listings
	Router            router.Deployment
	Patterns          []string
	Scope             catalog.Scope
	BedrockCandidates map[string]string   // id -> first probe region
	BedrockRegions    map[string][]string // id -> regions worth trying, in order
	RouterCandidates  []string
	Probes            *probe.Batch // nil if probing was skipped
	RouterConfig      string       // full models.json snapshot used to stage router probes
}

// Probed reports whether live probes ran.
func (p *Pipeline) Probed() bool { return p.Probes != nil }

// CatalogIDs returns the catalog ids for one provider.
func (p *Pipeline) CatalogIDs(provider string) map[string]bool {
	ids := map[string]bool{}
	for _, e := range p.Catalog {
		if e.Provider == provider {
			ids[e.ID] = true
		}
	}
	return ids
}

// InvocableBedrock maps each successful Bedrock probe to its working region.
// A candidate without a probe is never counted as invocable.
func (p *Pipeline) InvocableBedrock() map[string]string {
	if p.Probes == nil {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, o := range p.Probes.Outcomes {
		if o.Provider == model.Bedrock && o.Status == probe.OK {
			out[o.ID] = o.Region
		}
	}
	return out
}

// InvocableRouter lists router IDs with successful probes, sorted.
func (p *Pipeline) InvocableRouter() []string {
	if p.Probes == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, o := range p.Probes.Outcomes {
		if o.Provider == model.Router && o.Status == probe.OK && !seen[o.ID] {
			out = append(out, o.ID)
			seen[o.ID] = true
		}
	}
	sort.Strings(out)
	return out
}

// Rows builds one row per provider and model with independent status flags.
func (p *Pipeline) Rows() []model.Row {
	probeByKey := map[string]probe.Outcome{}
	systemic := map[string]bool{}
	bedrockSuccess := map[string]bool{}
	if p.Probes != nil {
		for _, o := range p.Probes.Outcomes {
			key := o.Provider + "\x00" + o.ID
			if o.Provider == model.Bedrock {
				if o.Status == probe.OK {
					bedrockSuccess[o.ID] = true
				}
			}
			if o.Status == probe.SKIP || o.Systemic {
				systemic[key] = true
			}
			if current, ok := probeByKey[key]; !ok || current.Status != probe.OK {
				probeByKey[key] = o
			}
		}
	}

	catalogByID := map[string]model.CatalogEntry{}
	for _, e := range p.Catalog {
		catalogByID[e.Provider+"\x00"+e.ID] = e
	}

	scoped := func(provider, id string) model.State {
		if p.Scope.Selected[provider+"\x00"+id] {
			return model.Yes
		}
		return model.No
	}

	rows := []model.Row{}
	workingRegions := p.InvocableBedrock()
	listedAll := p.Listings.AllIDs()
	degraded := len(p.Listings.Degraded) > 0

	for _, modelID := range sortedKeys(p.CatalogIDs(model.Bedrock)) {
		listed := model.No
		if listedAll[modelID] {
			listed = model.Yes
		} else if degraded {
			listed = model.Unknown
		}
		_, isCandidate := p.BedrockCandidates[modelID]
		candidate := model.No
		if isCandidate {
			candidate = model.Yes
		} else if degraded {
			candidate = model.Unknown
		}
		invocable := model.Unknown
		note := ""
		if p.Probes != nil {
			if outcome, ok := probeByKey[model.Bedrock+"\x00"+modelID]; ok {
				if bedrockSuccess[modelID] {
					invocable = model.Yes
				} else if outcome.Status == probe.FAIL && !p.Probes.Tripped && !systemic[model.Bedrock+"\x00"+modelID] && len(p.Listings.Degraded) == 0 {
					invocable = model.No
				}
				note = outcome.Reason
			}
		}
		stats := catalogStats(catalogByID[model.Bedrock+"\x00"+modelID])
		region := p.BedrockCandidates[modelID]
		if successful, ok := workingRegions[modelID]; ok {
			region = successful
		}
		rows = append(rows, model.Row{
			Provider:       model.Bedrock,
			ID:             modelID,
			Catalog:        model.Yes,
			ListedDeployed: listed,
			Candidate:      candidate,
			Invocable:      invocable,
			Scoped:         scoped(model.Bedrock, modelID),
			Region:         region,
			Note:           note,
			Stats:          stats,
		})
	}

	routerCatalog := p.CatalogIDs(model.Router)
	deployed := map[string]bool{}
	for _, id := range p.Router.Deployed {
		deployed[id] = true
	}
	union := map[string]bool{}
	for id := range routerCatalog {
		union[id] = true
	}
	for id := range deployed {
		union[id] = true
	}
	for _, modelID := range sortedKeys(union) {
		inCatalog := routerCatalog[modelID]
		isDeployed := deployed[modelID]
		deployedFlag, candidate, invocable := model.Unknown, model.Unknown, model.Unknown
		if p.Router.Reachable {
			deployedFlag = model.No
			if isDeployed {
				deployedFlag = model.Yes
			}
			candidate = model.No
			if isDeployed {
				candidate = model.Yes
			}
			if p.Probes != nil {
				if outcome, ok := probeByKey[model.Router+"\x00"+modelID]; ok {
					if outcome.Status == probe.OK {
						invocable = model.Yes
					} else if outcome.Status == probe.FAIL && !systemic[model.Router+"\x00"+modelID] && !p.Probes.Tripped {
						invocable = model.No
					}
				}
			}
		}
		note := ""
		switch {
		case !p.Router.Reachable:
			note = "router unreachable"
		case isDeployed && !inCatalog:
			note = "deployed but unknown to pi"
		case inCatalog && !isDeployed:
			note = "no longer deployed"
		}
		if outcome, ok := probeByKey[model.Router+"\x00"+modelID]; ok && outcome.Reason != "" {
			if note == "" {
				note = outcome.Reason
			} else {
				note += "; " + outcome.Reason
			}
		}
		stats := routerStats(p.Router.Entries[modelID], catalogByID[model.Router+"\x00"+modelID])
		rows = append(rows, model.Row{
			Provider:       model.Router,
			ID:             modelID,
			Catalog:        boolState(inCatalog),
			ListedDeployed: deployedFlag,
			Candidate:      candidate,
			Invocable:      invocable,
			Scoped:         scoped(model.Router, modelID),
			Note:           note,
			Stats:          stats,
		})
	}
	return rows
}

// Drift reports router catalog differences and curated scope gaps.
func (p *Pipeline) Drift() model.Drift {
	var unmatched, uncovered []string
	if p.Probed() && len(p.Listings.Degraded) == 0 && p.Router.Reachable && !p.Probes.Tripped {
		complete := true
		for _, row := range p.Rows() {
			if row.Candidate == model.Yes && row.Invocable == model.Unknown {
				complete = false
				break
			}
		}
		if complete {
			verified := map[string]bool{}
			for _, row := range p.Rows() {
				if row.Invocable == model.Yes {
					key := row.Provider + "\x00" + row.ID
					verified[key] = true
					if !p.Scope.Selected[key] {
						uncovered = append(uncovered, row.Provider+"/"+row.ID)
					}
				}
			}
			for _, pattern := range p.Patterns {
				matched := false
				for _, key := range p.Scope.Matches[pattern] {
					matched = matched || verified[key]
				}
				if !matched {
					unmatched = append(unmatched, pattern)
				}
			}
			sort.Strings(uncovered)
		}
	}
	drift := model.Drift{
		DeployedUnknownToPi:    []string{},
		ListedButUndeployed:    []string{},
		UnmatchedScopePatterns: unmatched,
		InvocableUncovered:     uncovered,
	}
	if p.Router.Reachable {
		deployed := map[string]bool{}
		for _, id := range p.Router.Deployed {
			deployed[id] = true
		}
		catalogRouter := p.CatalogIDs(model.Router)
		for id := range deployed {
			if !catalogRouter[id] {
				drift.DeployedUnknownToPi = append(drift.DeployedUnknownToPi, id)
			}
		}
		for id := range catalogRouter {
			if !deployed[id] {
				drift.ListedButUndeployed = append(drift.ListedButUndeployed, id)
			}
		}
		sort.Strings(drift.DeployedUnknownToPi)
		sort.Strings(drift.ListedButUndeployed)
	}
	return drift
}

// catalogStats derives stats from the catalog row's strings (used for
// Bedrock models, which have no router metadata).
func catalogStats(entry model.CatalogEntry) model.Stats {
	ctxTokens, _ := model.ParseTokenCount(entry.Context)
	maxOut, _ := model.ParseTokenCount(entry.MaxOut)
	return model.Stats{
		Context:   ctxTokens,
		MaxOut:    maxOut,
		Reasoning: entry.Thinking == "yes",
		Vision:    entry.Images == "yes",
	}
}

// routerStats derives stats from the generated router entry, falling back
// to the catalog row when the entry is absent (deployed-but-unknown models
// have an entry; catalog-only models might not).
func routerStats(entry router.ModelEntry, catalogEntry model.CatalogEntry) model.Stats {
	stats := catalogStats(catalogEntry)
	if entry.Entry.ID == "" {
		return stats
	}
	stats.Context = int(entry.Entry.ContextWindow)
	stats.MaxOut = int(entry.Entry.MaxTokens)
	stats.Reasoning = entry.Entry.Reasoning
	stats.Vision = false
	for _, modality := range entry.Entry.Input {
		if modality == "image" {
			stats.Vision = true
		}
	}
	in, out, cacheRead := entry.Entry.Cost.Input, entry.Entry.Cost.Output, entry.Entry.Cost.CacheRead
	stats.CostIn = &in
	stats.CostOut = &out
	stats.CostCacheRead = &cacheRead
	return stats
}

// Run fetches every source, computes candidates, and (unless doProbe is
// false) probes.
func Run(ctx context.Context, cfg paths.Config, doProbe, strict bool) (*Pipeline, error) {
	progress.Report(ctx, "loading Pi model catalog")
	catalogEntries, err := loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	progress.Report(ctx, "loaded %d Pi catalog models", len(catalogEntries))
	progress.Report(ctx, "checking AWS credentials for profile %s", cfg.AWSProfile)
	if err := checkSSO(ctx, cfg); err != nil { // exactly once per run; never logs in
		return nil, err
	}
	progress.Report(ctx, "AWS credentials ready")
	listings := fetchListings(ctx, cfg)
	progress.Report(ctx, "fetching router deployment and metadata")
	routerDeployment, err := fetchRouter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if routerDeployment.Reachable {
		progress.Report(ctx, "router: %d deployed models", len(routerDeployment.Deployed))
	} else {
		progress.Report(ctx, "router unavailable: %s", routerDeployment.Err)
	}
	if strict && !routerDeployment.Reachable {
		return nil, &RouterUnreachableError{Detail: routerDeployment.Err}
	}

	registry, err := loadRegistry(cfg.ModelRegistry)
	if err != nil {
		return nil, fmt.Errorf("model registry: %w", err)
	}
	bedrockCatalog := map[string]bool{}
	for _, e := range catalogEntries {
		if e.Provider == model.Bedrock {
			bedrockCatalog[e.ID] = true
		}
	}
	progress.Report(ctx, "resolving Pi model scope")
	patterns, err := readPatterns(cfg.PISettings)
	if err != nil {
		return nil, fmt.Errorf("read settings snapshot: %w", err)
	}
	scope, err := resolveScope(ctx, catalogEntries, patterns, cfg.ModelsJSON)
	if err != nil {
		return nil, err
	}
	probeScope := scope
	if cfg.LiveSettings != "" && cfg.LiveSettings != cfg.PISettings {
		live, err := readPatterns(cfg.LiveSettings)
		if err != nil {
			return nil, fmt.Errorf("read live settings: %w", err)
		}
		liveScope, err := resolveScope(ctx, catalogEntries, live, cfg.ModelsJSON)
		if err != nil {
			return nil, err
		}
		// Resolve separately: an empty live scope means all models, even
		// when the snapshot still contains a restricted set of patterns.
		probeScope.Selected = make(map[string]bool, len(scope.Selected)+len(liveScope.Selected))
		for key := range scope.Selected {
			probeScope.Selected[key] = true
		}
		for key := range liveScope.Selected {
			probeScope.Selected[key] = true
		}
	}
	selected := map[string]bool{}
	for _, entry := range catalogEntries {
		if entry.Provider == model.Bedrock && probeScope.Selected[entry.Provider+"\x00"+entry.ID] {
			selected[entry.ID] = true
		}
	}
	bedrockRegions, err := bedrockProbeRegions(cfg, listings, bedrockCatalog, selected, registry.ProbeRegionOverrides)
	if err != nil {
		return nil, err
	}
	bedrockCandidates := make(map[string]string, len(bedrockRegions))
	for id, regions := range bedrockRegions {
		bedrockCandidates[id] = regions[0]
	}
	var routerCandidates []string
	if routerDeployment.Reachable {
		routerCandidates = append(routerCandidates, routerDeployment.Deployed...)
		sort.Strings(routerCandidates)
	}
	p := &Pipeline{
		Cfg:               cfg,
		Catalog:           catalogEntries,
		Listings:          listings,
		Router:            routerDeployment,
		Patterns:          patterns,
		Scope:             scope,
		BedrockCandidates: bedrockCandidates,
		BedrockRegions:    bedrockRegions,
		RouterCandidates:  routerCandidates,
	}
	progress.Report(ctx, "candidates: %d Bedrock models, %d router models", len(bedrockCandidates), len(routerCandidates))
	if doProbe {
		if err := p.runProbes(ctx); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		progress.Report(ctx, "probes finished: %d Bedrock models and %d router models verified", len(p.InvocableBedrock()), len(p.InvocableRouter()))
	}
	return p, nil
}

func sortedKeys[V any](set map[string]V) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func boolState(b bool) model.State {
	if b {
		return model.Yes
	}
	return model.No
}
