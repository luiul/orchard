package report

import (
	"strings"
	"testing"

	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/pipeline"
	"github.com/luiul/orchard/internal/router"
)

func tinyPipeline() *pipeline.Pipeline {
	return &pipeline.Pipeline{
		Cfg: paths.Config{
			DefaultRegion: "eu-west-1",
			ExtraRegions:  []string{"us-east-1"},
		},
		Router: router.Deployment{Reachable: true},
	}
}

func TestJSONModelStatus(t *testing.T) {
	p := tinyPipeline()
	rows := []model.Row{{
		Provider:       model.Bedrock,
		ID:             "b1",
		Catalog:        model.Yes,
		ListedDeployed: model.Yes,
		Candidate:      model.Yes,
		Invocable:      model.Unknown,
		Scoped:         model.Yes,
		Region:         "eu-west-1",
	}}
	drift := model.Drift{
		DeployedUnknownToPi:    []string{},
		ListedButUndeployed:    []string{},
		UnmatchedScopePatterns: []string{},
		InvocableUncovered:     []string{},
	}
	got, err := JSON(p, rows, drift, "2026-10-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "drift": {
    "deployedUnknownToPi": [],
    "invocableUncovered": [],
    "listedButUndeployed": [],
    "unmatchedScopePatterns": []
  },
  "generatedAt": "2026-10-01T00:00:00Z",
  "meta": {
    "circuitTripped": false,
    "defaultRegion": "eu-west-1",
    "degradedRegions": {},
    "probed": false,
    "regions": [
      "eu-west-1",
      "us-east-1"
    ],
    "routerError": null,
    "routerReachable": true
  },
  "rows": [
    {
      "candidate": "yes",
      "catalog": "yes",
      "id": "b1",
      "invocable": "unknown",
      "listedDeployed": "yes",
      "note": "",
      "provider": "amazon-bedrock",
      "region": "eu-west-1",
      "scoped": "yes",
      "stats": {
        "context": 0,
        "costCacheRead": null,
        "costIn": null,
        "costOut": null,
        "maxOut": 0,
        "reasoning": false,
        "vision": false
      }
    }
  ]
}
`
	if got != want {
		t.Errorf("JSON mismatch.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestJSONRouterErrorCarried(t *testing.T) {
	p := tinyPipeline()
	p.Router = router.Deployment{Reachable: false, Err: "connection refused"}
	got, err := JSON(p, nil, model.Drift{}, "2026-10-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"routerError": "connection refused"`) {
		t.Errorf("router error missing:\n%s", got)
	}
	if !strings.Contains(got, `"routerReachable": false`) {
		t.Errorf("reachability missing:\n%s", got)
	}
	// Empty row and drift lists render as [], never null.
	if !strings.Contains(got, `"rows": []`) {
		t.Errorf("rows not an empty array:\n%s", got)
	}
}

func TestJSONStatsAndCosts(t *testing.T) {
	p := tinyPipeline()
	in, out, cacheRead := 2.0, 10.0, 0.2
	rows := []model.Row{{
		Provider: model.Router,
		ID:       "claude-sonnet-5",
		Stats: model.Stats{
			Context:       1_000_000,
			MaxOut:        128_000,
			CostIn:        &in,
			CostOut:       &out,
			CostCacheRead: &cacheRead,
			Reasoning:     true,
			Vision:        true,
		},
	}}
	got, err := JSON(p, rows, model.Drift{}, "2026-10-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	want := `"stats": {
        "context": 1000000,
        "costCacheRead": 0.2,
        "costIn": 2,
        "costOut": 10,
        "maxOut": 128000,
        "reasoning": true,
        "vision": true
      }`
	if !strings.Contains(got, want) {
		t.Errorf("stats block mismatch, got:\n%s", got)
	}
}

func TestTableRendersStatesAndNotes(t *testing.T) {
	rows := []model.Row{{
		Provider:       model.Bedrock,
		ID:             "b1",
		Catalog:        model.Yes,
		ListedDeployed: model.Yes,
		Candidate:      model.Yes,
		Invocable:      model.Unknown,
		Scoped:         model.No,
		Region:         "eu-west-1",
		Note:           "(some reason)",
	}}
	table := Table(rows)
	for _, want := range []string{"Model status (scope is independent of usability)", "provider", "listed/deployed", "bedrock", "b1", "yes", "?", "eu-west-1 (some reason)"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}
}

func TestDriftSectionsOnlyNonEmpty(t *testing.T) {
	if sections := DriftSections(model.Drift{}); len(sections) != 0 {
		t.Errorf("empty drift: %v", sections)
	}
	sections := DriftSections(model.Drift{UnmatchedScopePatterns: []string{"x*"}, ListedButUndeployed: []string{"r2"}})
	if len(sections) != 2 {
		t.Fatalf("sections: %v", sections)
	}
	if sections[0].Title != "Listed in pi but no longer deployed" {
		t.Errorf("sections[0]: %v", sections[0].Title)
	}
	if sections[1].Title != "Curated scope patterns with no verified match" {
		t.Errorf("sections[1]: %v", sections[1].Title)
	}
}

func TestSummarize(t *testing.T) {
	p := tinyPipeline()
	p.Catalog = []model.CatalogEntry{
		{Provider: model.Bedrock, ID: "b1"},
		{Provider: model.Router, ID: "r1"},
	}
	p.BedrockCandidates = map[string]string{"b1": "eu-west-1"}
	p.RouterCandidates = []string{"r1"}
	p.Router = router.Deployment{Reachable: true, Deployed: []string{"r1"}}
	out := Summarize(p)
	for _, want := range []string{
		"regions scanned: eu-west-1, us-east-1 (default: eu-west-1)",
		"bedrock: 1 catalog, 1 candidates, 0 invocable (not probed; candidates are unverified)",
		"router: 1 catalog, 1 deployed, 0 invocable (not probed; candidates are unverified)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

func TestSummarizeUnreachableRouter(t *testing.T) {
	p := tinyPipeline()
	p.Router = router.Deployment{Reachable: false, Err: "no route to host"}
	out := Summarize(p)
	if !strings.Contains(out, "router: unreachable (no route to host), router status unknown") {
		t.Errorf("summary:\n%s", out)
	}
}
