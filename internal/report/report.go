// Package report shows model discovery, probe results, and Pi scope coverage.
// It never changes settings. JSON output can be used by other tools.
package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/pipeline"
)

// Short provider labels keep the table readable; the JSON report carries
// full names.
var providerLabels = map[string]string{
	model.Bedrock: "bedrock",
	model.Router:  "router",
}

var stateMarks = map[model.State]string{
	model.Yes:     "yes",
	model.No:      "no",
	model.Unknown: "?",
}

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	green       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	red         = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	yellow      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

func stateCell(state model.State) string {
	mark := stateMarks[state]
	switch state {
	case model.Yes:
		return green.Render(mark)
	case model.No:
		return red.Render(mark)
	default:
		return yellow.Render(mark)
	}
}

// Table shows one row per model with independent status flags.
func Table(rows []model.Row) string {
	headers := []string{"provider", "model", "catalog", "listed/deployed", "candidate", "invocable", "scoped", "region / note"}
	centered := map[int]bool{2: true, 3: true, 4: true, 5: true, 6: true}

	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		note := strings.Join(strings.Fields(row.Region+" "+row.Note), " ")
		provider := row.Provider
		if label, ok := providerLabels[provider]; ok {
			provider = label
		}
		cells = append(cells, []string{
			provider, row.ID,
			stateMarks[row.Catalog], stateMarks[row.ListedDeployed], stateMarks[row.Candidate],
			stateMarks[row.Invocable], stateMarks[row.Scoped],
			note,
		})
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range cells {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}

	pad := func(s string, i int) string {
		gap := widths[i] - len(s)
		if gap <= 0 {
			return s
		}
		if centered[i] {
			left := gap / 2
			return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
		}
		return s + strings.Repeat(" ", gap)
	}

	var b strings.Builder
	b.WriteString(headerStyle.Render("Model status (scope is independent of usability)") + "\n")
	for i, h := range headers {
		if i > 0 {
			b.WriteString("  ")
		}
		b.WriteString(headerStyle.Render(pad(h, i)))
	}
	for ri, r := range cells {
		b.WriteString("\n")
		for i, c := range r {
			if i > 0 {
				b.WriteString("  ")
			}
			if i >= 2 && i <= 6 {
				// Color the centered state cells after padding.
				b.WriteString(colorState(rows[ri], i, pad(c, i), c))
			} else {
				b.WriteString(pad(c, i))
			}
		}
	}
	return b.String()
}

// colorState re-renders a padded state cell with the state's color applied to
// the mark only (padding stays unstyled).
func colorState(row model.Row, col int, padded, mark string) string {
	state := []model.State{row.Catalog, row.ListedDeployed, row.Candidate, row.Invocable, row.Scoped}[col-2]
	return strings.Replace(padded, mark, stateCell(state), 1)
}

// Section is one non-empty drift finding: title, items, and the fix hint.
type Section struct {
	Title string
	Items []string
	Hint  string
}

// DriftSections returns one section per non-empty drift finding.
func DriftSections(drift model.Drift) []Section {
	var sections []Section
	if len(drift.DeployedUnknownToPi) > 0 {
		sections = append(sections, Section{
			Title: "Deployed but unknown to pi",
			Items: drift.DeployedUnknownToPi,
			Hint:  "run `orchard sync` to regenerate models.json",
		})
	}
	if len(drift.ListedButUndeployed) > 0 {
		sections = append(sections, Section{
			Title: "Listed in pi but no longer deployed",
			Items: drift.ListedButUndeployed,
			Hint:  "run `orchard sync` to regenerate models.json",
		})
	}
	if len(drift.UnmatchedScopePatterns) > 0 {
		sections = append(sections, Section{
			Title: "Curated scope patterns with no verified match",
			Items: drift.UnmatchedScopePatterns,
			Hint:  "save the intended scope in Pi, then refresh the dotfiles settings snapshot",
		})
	}
	if len(drift.InvocableUncovered) > 0 {
		sections = append(sections, Section{
			Title: "Verified models outside the curated scope",
			Items: drift.InvocableUncovered,
			Hint:  "add a pattern to enabledModels to include them in Pi's scoped list",
		})
	}
	return sections
}

// JSON renders one report with fixed row and key order for scripting.
func JSON(p *pipeline.Pipeline, rows []model.Row, drift model.Drift, generatedAt string) (string, error) {
	var routerError any
	if p.Router.Err != "" {
		routerError = p.Router.Err
	}
	degraded := p.Listings.Degraded
	if degraded == nil {
		degraded = map[string]string{}
	}
	rowDocs := make([]any, 0, len(rows))
	for _, row := range rows {
		rowDocs = append(rowDocs, map[string]any{
			"provider":       row.Provider,
			"id":             row.ID,
			"catalog":        string(row.Catalog),
			"listedDeployed": string(row.ListedDeployed),
			"candidate":      string(row.Candidate),
			"invocable":      string(row.Invocable),
			"scoped":         string(row.Scoped),
			"region":         row.Region,
			"note":           row.Note,
			"stats":          statsDoc(row.Stats),
		})
	}
	nonNil := func(items []string) []string {
		if items == nil {
			return []string{}
		}
		return items
	}
	doc := map[string]any{
		"generatedAt": generatedAt,
		"meta": map[string]any{
			"regions":         p.Cfg.Regions(),
			"defaultRegion":   p.Cfg.DefaultRegion,
			"probed":          p.Probed(),
			"circuitTripped":  p.Probes != nil && p.Probes.Tripped,
			"routerReachable": p.Router.Reachable,
			"routerError":     routerError,
			"degradedRegions": degraded,
		},
		"rows": rowDocs,
		"drift": map[string]any{
			"deployedUnknownToPi":    nonNil(drift.DeployedUnknownToPi),
			"listedButUndeployed":    nonNil(drift.ListedButUndeployed),
			"unmatchedScopePatterns": nonNil(drift.UnmatchedScopePatterns),
			"invocableUncovered":     nonNil(drift.InvocableUncovered),
		},
	}
	text, err := model.MarshalIndent(doc)
	if err != nil {
		return "", err
	}
	return text + "\n", nil
}

// statsDoc renders basic catalog and router metadata, not benchmark scores.
func statsDoc(s model.Stats) map[string]any {
	var costIn, costOut, costCacheRead any
	if s.CostIn != nil {
		costIn = *s.CostIn
	}
	if s.CostOut != nil {
		costOut = *s.CostOut
	}
	if s.CostCacheRead != nil {
		costCacheRead = *s.CostCacheRead
	}
	return map[string]any{
		"context":       s.Context,
		"maxOut":        s.MaxOut,
		"costIn":        costIn,
		"costOut":       costOut,
		"costCacheRead": costCacheRead,
		"reasoning":     s.Reasoning,
		"vision":        s.Vision,
	}
}

// Summarize renders the one-line counts per provider for the report
// header.
func Summarize(p *pipeline.Pipeline) string {
	invocableBedrock := p.InvocableBedrock()
	lines := []string{
		fmt.Sprintf("regions scanned: %s (default: %s)", strings.Join(p.Cfg.Regions(), ", "), p.Cfg.DefaultRegion),
		fmt.Sprintf("bedrock: %d catalog, %d candidates, %d invocable%s",
			len(p.CatalogIDs(model.Bedrock)), len(p.BedrockCandidates), len(invocableBedrock),
			probeNote(p.Probed())),
	}
	if p.Router.Reachable {
		lines = append(lines, fmt.Sprintf("router: %d catalog, %d deployed, %d invocable%s",
			len(p.CatalogIDs(model.Router)), len(p.Router.Deployed), len(p.InvocableRouter()),
			probeNote(p.Probed())))
	} else {
		lines = append(lines, fmt.Sprintf("router: unreachable (%s), router status unknown", p.Router.Err))
	}
	regions := make([]string, 0, len(p.Listings.Degraded))
	for region := range p.Listings.Degraded {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	for _, region := range regions {
		lines = append(lines, fmt.Sprintf("degraded region %s: %s", region, p.Listings.Degraded[region]))
	}
	return strings.Join(lines, "\n")
}

func probeNote(probed bool) string {
	if probed {
		return ""
	}
	return " (not probed; candidates are unverified)"
}
