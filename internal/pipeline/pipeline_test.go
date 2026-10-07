package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/luiul/orchard/internal/bedrock"
	"github.com/luiul/orchard/internal/catalog"
	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/probe"
	"github.com/luiul/orchard/internal/router"
)

// swapSeams installs small synthetic fakes and restores the real seams
// afterwards.
func swapSeams(t *testing.T) {
	t.Helper()
	old := []any{loadCatalog, checkSSO, fetchListings, fetchRouter, loadRegistry, readPatterns, probeAll}

	oldScope := resolveScope
	resolveScope = func(_ context.Context, entries []model.CatalogEntry, patterns []string, _ string) (catalog.Scope, error) {
		scope := catalog.Scope{Selected: map[string]bool{}, Matches: map[string][]string{}}
		for _, entry := range entries {
			key := entry.Provider + "\x00" + entry.ID
			if len(patterns) == 0 {
				scope.Selected[key] = true
			}
			for _, pattern := range patterns {
				if strings.HasPrefix(entry.ID, strings.TrimSuffix(pattern, "*")) {
					scope.Selected[key] = true
					scope.Matches[pattern] = append(scope.Matches[pattern], key)
				}
			}
		}
		if len(scope.Selected) == 0 {
			for _, entry := range entries {
				scope.Selected[entry.Provider+"\x00"+entry.ID] = true
			}
		}
		return scope, nil
	}
	t.Cleanup(func() { resolveScope = oldScope })
	oldRead := readModelConfig
	readModelConfig = func(string) ([]byte, error) {
		return []byte(`{"providers":{"ai-model-router":{"baseUrl":"https://test.invalid/v1","api":"openai-completions","apiKey":"$AI_MODEL_ROUTER_API_KEY","models":[]}}}`), nil
	}
	t.Cleanup(func() { readModelConfig = oldRead })
	loadCatalog = func(context.Context) ([]model.CatalogEntry, error) {
		return []model.CatalogEntry{
			{Provider: model.Bedrock, ID: "b1", Context: "200K", MaxOut: "64K", Thinking: "yes", Images: "yes"},
			{Provider: model.Bedrock, ID: "b2", Context: "200K", MaxOut: "64K", Thinking: "no", Images: "no"},
			{Provider: model.Bedrock, ID: "b3", Context: "200K", MaxOut: "64K", Thinking: "no", Images: "no"},
			{Provider: model.Router, ID: "r1", Context: "1M", MaxOut: "128K", Thinking: "yes", Images: "yes"},
			{Provider: model.Router, ID: "r2", Context: "200K", MaxOut: "64K", Thinking: "yes", Images: "no"},
		}, nil
	}
	checkSSO = func(context.Context, paths.Config) error { return nil }
	fetchListings = func(context.Context, paths.Config) bedrock.Listings {
		return bedrock.Listings{
			ByRegion: map[string]map[string]bool{
				"eu-west-1": {"b1": true, "b2": true},
				"us-east-1": {"b1": true},
			},
			Degraded: map[string]string{},
		}
	}
	fetchRouter = func(context.Context, paths.Config) (router.Deployment, error) {
		return router.Deployment{
			Reachable: true,
			Deployed:  []string{"r1", "r3-unknown"},
			Entries: map[string]router.ModelEntry{
				"r1":         {Entry: router.Entry{ID: "r1", ContextWindow: 200_000, MaxTokens: 64_000}},
				"r3-unknown": {Entry: router.Entry{ID: "r3-unknown", ContextWindow: 200_000, MaxTokens: 64_000}},
			},
		}, nil
	}
	loadRegistry = func(string) (router.Registry, error) {
		return router.Registry{
			ProbeRegionOverrides: map[string]string{},
			RouterModelOverrides: map[string]map[string]any{},
		}, nil
	}
	readPatterns = func(string) ([]string, error) { return []string{"b1*", "r1*"}, nil }
	probeAll = func(_ context.Context, targets []probe.Target, _ paths.Config) probe.Batch {
		outcomes := make([]probe.Outcome, 0, len(targets))
		for _, target := range targets {
			status := probe.OK
			reason := ""
			if target.ID == "b2" {
				status = probe.FAIL
				reason = "(AccessDeniedException: no marketplace subscription)"
			}
			outcomes = append(outcomes, probe.Outcome{
				ID: target.ID, Provider: target.Provider, Region: target.Region, Status: status, Reason: reason,
			})
		}
		return probe.Batch{Outcomes: outcomes}
	}
	t.Cleanup(func() {
		loadCatalog = old[0].(func(context.Context) ([]model.CatalogEntry, error))
		checkSSO = old[1].(func(context.Context, paths.Config) error)
		fetchListings = old[2].(func(context.Context, paths.Config) bedrock.Listings)
		fetchRouter = old[3].(func(context.Context, paths.Config) (router.Deployment, error))
		loadRegistry = old[4].(func(string) (router.Registry, error))
		readPatterns = old[5].(func(string) ([]string, error))
		probeAll = old[6].(func(context.Context, []probe.Target, paths.Config) probe.Batch)
	})
}

func testConfig() paths.Config {
	return paths.Config{
		DefaultRegion: "eu-west-1",
		ExtraRegions:  []string{"us-east-1"},
		AWSProfile:    "sso-bedrock",
	}
}

func rowByID(rows []model.Row, provider, id string) model.Row {
	for _, r := range rows {
		if r.Provider == provider && r.ID == id {
			return r
		}
	}
	return model.Row{}
}

func TestRunProbedPipelineRowsAndDrift(t *testing.T) {
	swapSeams(t)
	p, err := Run(context.Background(), testConfig(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Probed() {
		t.Fatal("expected probes")
	}
	rows := p.Rows()

	b1 := rowByID(rows, model.Bedrock, "b1")
	if b1.ListedDeployed != model.Yes || b1.Candidate != model.Yes || b1.Invocable != model.Yes {
		t.Errorf("b1 states: %+v", b1)
	}
	if b1.Scoped != model.Yes || b1.Region != "eu-west-1" {
		t.Errorf("b1: %+v", b1)
	}
	if b1.Stats.Context != 200_000 || !b1.Stats.Reasoning || !b1.Stats.Vision {
		t.Errorf("b1 stats: %+v", b1.Stats)
	}

	b2 := rowByID(rows, model.Bedrock, "b2")
	if b2.Invocable != model.No {
		t.Errorf("b2 invocable: %v", b2.Invocable)
	}
	if b2.Note == "" {
		t.Error("b2 note empty")
	}

	b3 := rowByID(rows, model.Bedrock, "b3")
	if b3.ListedDeployed != model.No || b3.Candidate != model.No || b3.Invocable != model.Unknown {
		t.Errorf("b3 states: %+v", b3)
	}

	r1 := rowByID(rows, model.Router, "r1")
	if r1.ListedDeployed != model.Yes || r1.Candidate != model.Yes || r1.Invocable != model.Yes {
		t.Errorf("r1 states: %+v", r1)
	}

	r2 := rowByID(rows, model.Router, "r2")
	if r2.ListedDeployed != model.No || r2.Note != "no longer deployed" {
		t.Errorf("r2: %+v", r2)
	}

	r3 := rowByID(rows, model.Router, "r3-unknown")
	if r3.Catalog != model.No || r3.ListedDeployed != model.Yes || r3.Note != "deployed but unknown to pi" {
		t.Errorf("r3: %+v", r3)
	}

	drift := p.Drift()
	if len(drift.DeployedUnknownToPi) != 1 || drift.DeployedUnknownToPi[0] != "r3-unknown" {
		t.Errorf("deployedUnknownToPi: %v", drift.DeployedUnknownToPi)
	}
	if len(drift.ListedButUndeployed) != 1 || drift.ListedButUndeployed[0] != "r2" {
		t.Errorf("listedButUndeployed: %v", drift.ListedButUndeployed)
	}
	if len(drift.UnmatchedScopePatterns) != 0 || len(drift.InvocableUncovered) != 1 || drift.InvocableUncovered[0] != "ai-model-router/r3-unknown" {
		t.Errorf("patterns drift: %v / %v", drift.UnmatchedScopePatterns, drift.InvocableUncovered)
	}
}

func TestRunNoProbeCandidatesStandIn(t *testing.T) {
	swapSeams(t)
	p, err := Run(context.Background(), testConfig(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Probed() {
		t.Fatal("expected no probes")
	}
	rows := p.Rows()
	if r := rowByID(rows, model.Bedrock, "b1"); r.Invocable != model.Unknown {
		t.Errorf("b1 invocable: %v", r.Invocable)
	}
	// Without probes, the report must not call any candidate invocable or
	// decide which patterns are unmatched.
	drift := p.Drift()
	if len(drift.InvocableUncovered) != 0 || len(drift.UnmatchedScopePatterns) != 0 {
		t.Errorf("unverified drift: %+v", drift)
	}
}

func TestRunStrictFailsOnUnreachableRouter(t *testing.T) {
	swapSeams(t)
	old := fetchRouter
	fetchRouter = func(context.Context, paths.Config) (router.Deployment, error) {
		return router.Deployment{Reachable: false, Err: "connection refused"}, nil
	}
	t.Cleanup(func() { fetchRouter = old })

	if _, err := Run(context.Background(), testConfig(), false, false); err != nil {
		t.Fatalf("non-strict must not fail: %v", err)
	}
	_, err := Run(context.Background(), testConfig(), false, true)
	var unreachable *RouterUnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("got %v, want RouterUnreachableError", err)
	}
}
