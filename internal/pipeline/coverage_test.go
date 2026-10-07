package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luiul/orchard/internal/bedrock"
	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/probe"
	"github.com/luiul/orchard/internal/router"
)

func TestBedrockRegionsIncludeAllPrefixesAndScopedMisses(t *testing.T) {
	cfg := paths.Config{DefaultRegion: "eu-west-1", ExtraRegions: []string{"us-east-1", "ap-northeast-1", "ap-southeast-2"}}
	listings := bedrock.Listings{ByRegion: map[string]map[string]bool{
		"eu-west-1":      {"global.anthropic.x": true},
		"us-east-1":      {"us.anthropic.x": true},
		"ap-northeast-1": {"jp.anthropic.x": true, "apac.anthropic.x": true},
		"ap-southeast-2": {"au.anthropic.x": true},
	}}
	catalog := map[string]bool{}
	for _, ids := range listings.ByRegion {
		for id := range ids {
			catalog[id] = true
		}
	}
	catalog["scoped.notlisted"] = true
	catalog["ignored.notlisted"] = true
	regions, err := bedrockProbeRegions(cfg, listings, catalog, map[string]bool{"scoped.notlisted": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"global.anthropic.x", "us.anthropic.x", "jp.anthropic.x", "apac.anthropic.x", "au.anthropic.x"} {
		if len(regions[id]) != 1 {
			t.Errorf("%s: %v", id, regions[id])
		}
	}
	if len(regions["scoped.notlisted"]) != 4 || len(regions["ignored.notlisted"]) != 0 {
		t.Errorf("unexpected scoped and unscoped misses: %+v", regions)
	}
}

func TestBedrockRegionsUseSavedMapAsHintAndRejectBadOverride(t *testing.T) {
	cfg := paths.Config{DefaultRegion: "eu-west-1", ExtraRegions: []string{"ap-southeast-2"}, BedrockModelsJSON: filepath.Join(t.TempDir(), "map.json")}
	if err := os.WriteFile(cfg.BedrockModelsJSON, []byte(`{"models":{"au.anthropic.x":"ap-southeast-2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	regions, err := bedrockProbeRegions(cfg, bedrock.Listings{}, map[string]bool{"au.anthropic.x": true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(regions["au.anthropic.x"], []string{"ap-southeast-2", "eu-west-1"}) {
		t.Errorf("saved region used as more than a hint: %v", regions)
	}
	if _, err := bedrockProbeRegions(cfg, bedrock.Listings{}, nil, nil, map[string]string{"x": "somewhere-else"}); err == nil {
		t.Fatal("accepted an override outside the scanned regions")
	}
	listings := bedrock.Listings{ByRegion: map[string]map[string]bool{
		"eu-west-1": {"known": true}, "ap-southeast-2": {"known": true},
	}}
	regions, err = bedrockProbeRegions(cfg, listings, map[string]bool{"known": true}, nil, map[string]string{"known": "ap-southeast-2"})
	if err != nil || len(regions["known"]) == 0 || regions["known"][0] != "ap-southeast-2" {
		t.Errorf("valid region override not first: %v, %v", regions, err)
	}
}

func TestProbeCoverageIncludesLiveScopeMissingFromSnapshot(t *testing.T) {
	swapSeams(t)
	cfg := testConfig()
	cfg.PISettings, cfg.LiveSettings = "snapshot", "live"
	readPatterns = func(path string) ([]string, error) {
		if path == "live" {
			return []string{"b3*"}, nil
		}
		return []string{"b1*", "r1*"}, nil
	}
	p, err := Run(context.Background(), cfg, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.InvocableBedrock()["b3"] == "" {
		t.Fatal("live scope model was not probed")
	}
	if row := rowByID(p.Rows(), model.Bedrock, "b3"); row.Scoped != model.No || row.Invocable != model.Yes {
		t.Fatalf("scope membership and probe coverage mixed up: %+v", row)
	}
}

func TestProbeCoverageIncludesEmptyLiveScope(t *testing.T) {
	swapSeams(t)
	cfg := testConfig()
	cfg.PISettings, cfg.LiveSettings = "snapshot", "live"
	readPatterns = func(path string) ([]string, error) {
		if path == "live" {
			return []string{}, nil
		}
		return []string{"b1*", "r1*"}, nil
	}
	p, err := Run(context.Background(), cfg, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.InvocableBedrock()["b3"] == "" {
		t.Fatal("empty live scope excluded an unlisted catalog model")
	}
	if row := rowByID(p.Rows(), model.Bedrock, "b3"); row.Scoped != model.No {
		t.Fatal("live coverage changed snapshot scope membership")
	}
}

func TestRouterProbeRejectsDifferentDiscoveryEndpoint(t *testing.T) {
	swapSeams(t)
	cfg := testConfig()
	cfg.RouterBaseURL = "https://different.invalid/v1"
	if _, err := Run(context.Background(), cfg, true, false); err == nil {
		t.Fatal("probed one gateway using discovery from another")
	}
}

func TestProbeFallbackKeepsWorkingRegion(t *testing.T) {
	swapSeams(t)
	oldFetch, oldProbe := fetchListings, probeAll
	fetchListings = func(context.Context, paths.Config) bedrock.Listings {
		return bedrock.Listings{ByRegion: map[string]map[string]bool{
			"eu-west-1": {"b1": true}, "us-east-1": {"b1": true},
		}, Degraded: map[string]string{}}
	}
	var attempted []string
	probeAll = func(_ context.Context, targets []probe.Target, _ paths.Config) probe.Batch {
		results := make([]probe.Outcome, 0, len(targets))
		for _, target := range targets {
			result := probe.Outcome{ID: target.ID, Provider: target.Provider, Region: target.Region, Status: probe.OK}
			if target.ID == "b1" {
				attempted = append(attempted, target.Region)
				if target.Region == "eu-west-1" {
					result.Status = probe.FAIL
					result.Reason = "(regional failure)"
				}
			}
			results = append(results, result)
		}
		return probe.Batch{Outcomes: results}
	}
	t.Cleanup(func() { fetchListings, probeAll = oldFetch, oldProbe })
	p, err := Run(context.Background(), testConfig(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(attempted, []string{"eu-west-1", "us-east-1"}) {
		t.Errorf("probe regions: %v", attempted)
	}
	if p.InvocableBedrock()["b1"] != "us-east-1" || rowByID(p.Rows(), model.Bedrock, "b1").Invocable != model.Yes {
		t.Errorf("working region missing from report: %+v", p.Rows())
	}
}

func TestRouterProbeSkipsIncompletelyStagedModel(t *testing.T) {
	swapSeams(t)
	oldEntries := fetchRouter
	fetchRouter = func(context.Context, paths.Config) (router.Deployment, error) {
		return router.Deployment{Reachable: true, Deployed: []string{"r1", "missing"}, Entries: map[string]router.ModelEntry{"r1": {Entry: router.Entry{ID: "r1"}}}}, nil
	}
	t.Cleanup(func() { fetchRouter = oldEntries })
	if _, err := Run(context.Background(), testConfig(), true, false); err == nil {
		t.Fatal("accepted a gateway result missing a staged model definition")
	}
}

func TestNewRouterModelUsesIsolatedPiConfig(t *testing.T) {
	swapSeams(t)
	oldProbe := probeAll
	probeAll = func(_ context.Context, targets []probe.Target, _ paths.Config) probe.Batch {
		results := make([]probe.Outcome, 0, len(targets))
		for _, target := range targets {
			if target.Provider == model.Router {
				if target.AgentDir == "" {
					t.Fatal("router target did not use the staged Pi config")
				}
				data, err := os.ReadFile(filepath.Join(target.AgentDir, "models.json"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), `"id": "r3-unknown"`) || !strings.Contains(string(data), `"id": "r1"`) {
					t.Fatal("staged provider missing an existing or new router model")
				}
				if strings.Contains(string(data), "amazon-bedrock") {
					t.Fatal("staged config copied unrelated provider")
				}
			}
			results = append(results, probe.Outcome{ID: target.ID, Provider: target.Provider, Region: target.Region, Status: probe.OK})
		}
		return probe.Batch{Outcomes: results}
	}
	t.Cleanup(func() { probeAll = oldProbe })
	p, err := Run(context.Background(), testConfig(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.InvocableRouter(), "r3-unknown") {
		t.Fatalf("new router model was not probed: %v", p.InvocableRouter())
	}
	row := rowByID(p.Rows(), model.Router, "r3-unknown")
	if row.Catalog != model.No || row.Invocable != model.Yes {
		t.Errorf("discovered router ID status: %+v", row)
	}
}

func TestSyncReadyRejectsUnverifiedPreviousAndDegradedSources(t *testing.T) {
	p := &Pipeline{
		Cfg:               paths.Config{DefaultRegion: "eu-west-1"},
		Catalog:           []model.CatalogEntry{{Provider: model.Bedrock, ID: "b1"}},
		Listings:          bedrock.Listings{ByRegion: map[string]map[string]bool{"eu-west-1": {"b1": true}}},
		Router:            router.Deployment{Reachable: true, Deployed: []string{"r1"}},
		BedrockCandidates: map[string]string{"b1": "eu-west-1"},
		RouterCandidates:  []string{"r1"},
		Probes: &probe.Batch{Outcomes: []probe.Outcome{
			{Provider: model.Bedrock, ID: "b1", Region: "eu-west-1", Status: probe.OK},
			{Provider: model.Router, ID: "r1", Status: probe.OK},
		}},
	}
	if err := p.SyncReady(map[string]string{"b1": "eu-west-1"}); err != nil {
		t.Fatalf("complete scan refused: %v", err)
	}
	p.Probes.Outcomes[1].Status = probe.FAIL
	p.Probes.Outcomes[1].Reason = "(AccessDeniedException)"
	if err := p.SyncReady(nil); err == nil {
		t.Error("accepted a failed staged router probe")
	}
	p.Probes.Outcomes[1].Status = probe.OK
	p.Probes.Outcomes[1].Reason = ""
	p.RouterCandidates = nil
	if err := p.SyncReady(nil); err == nil {
		t.Error("accepted a router deployment with no probe list")
	}
	p.RouterCandidates = []string{"r1"}
	delete(p.Listings.ByRegion, "eu-west-1")
	if err := p.SyncReady(nil); err == nil {
		t.Error("accepted a missing Bedrock region without a degraded flag")
	}
	p.Listings.ByRegion["eu-west-1"] = map[string]bool{"b1": true}
	if err := p.SyncReady(map[string]string{"previous": "ap-northeast-1"}); err == nil {
		t.Error("removed a previously mapped model")
	}
	p.Listings.Degraded = map[string]string{"ap-northeast-1": "timeout"}
	if err := p.SyncReady(nil); err == nil {
		t.Error("accepted an incomplete region scan")
	}
	p.Listings.Degraded = nil
	p.Probes.Outcomes[0].Status = probe.FAIL
	if err := p.SyncReady(nil); err == nil {
		t.Error("accepted a run with no verified Bedrock model")
	}
}

func TestSyncReadyRejectsUncertainProbe(t *testing.T) {
	p := &Pipeline{
		Cfg:               paths.Config{DefaultRegion: "eu-west-1"},
		Catalog:           []model.CatalogEntry{{Provider: model.Bedrock, ID: "b1"}},
		Listings:          bedrock.Listings{ByRegion: map[string]map[string]bool{"eu-west-1": {"b1": true}}},
		Router:            router.Deployment{Reachable: true, Deployed: []string{"r1"}},
		BedrockCandidates: map[string]string{"b1": "eu-west-1"},
		RouterCandidates:  []string{"r1"},
		Probes: &probe.Batch{Outcomes: []probe.Outcome{
			{Provider: model.Bedrock, ID: "b1", Region: "eu-west-1", Status: probe.FAIL, Systemic: true},
			{Provider: model.Router, ID: "r1", Status: probe.OK},
		}},
	}
	if err := p.SyncReady(nil); err == nil {
		t.Error("systemic probe failure allowed sync")
	}
}

func TestStageRouterConfigRejectsInvalidTokenLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"providers":{"ai-model-router":{"baseUrl":"https://test.invalid/v1","api":"openai-completions","apiKey":"$AI_MODEL_ROUTER_API_KEY","models":[]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]router.ModelEntry{"r1": {Entry: router.Entry{ID: "r1", ContextWindow: 0, MaxTokens: 100}}}
	if _, err := stageRouterConfig(path, entries); err == nil {
		t.Fatal("accepted a router model with zero context window")
	}
	entries["r1"] = router.ModelEntry{Entry: router.Entry{ID: "r1", ContextWindow: 1000, MaxTokens: 0}}
	if _, err := stageRouterConfig(path, entries); err == nil {
		t.Fatal("accepted a router model with zero output limit")
	}
}

func TestStageRouterConfigRequiresValidModelsFile(t *testing.T) {
	if _, err := stageRouterConfig(filepath.Join(t.TempDir(), "missing"), map[string]router.ModelEntry{}); err == nil {
		t.Fatal("staged a missing router config")
	}
}
