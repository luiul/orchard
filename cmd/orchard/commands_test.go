package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luiul/orchard/internal/bedrock"
	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/pipeline"
	"github.com/luiul/orchard/internal/probe"
	"github.com/luiul/orchard/internal/progress"
	"github.com/luiul/orchard/internal/router"
)

func syncFixture(t *testing.T) paths.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := paths.Config{PISettings: filepath.Join(dir, "snapshot.json"), LiveSettings: filepath.Join(dir, "live.json"), ModelsJSON: filepath.Join(dir, "models.json"), BedrockModelsJSON: filepath.Join(dir, "bedrock.json"), DefaultRegion: "eu-west-1"}
	files := map[string]string{
		cfg.PISettings:        `{"enabledModels":["*"]}`,
		cfg.LiveSettings:      `{"enabledModels":["*"]}`,
		cfg.BedrockModelsJSON: `{"models":{"b1":"eu-west-1"}}`,
		cfg.ModelsJSON:        `{"providers":{"ai-model-router":{"baseUrl":"https://test.invalid/v1","api":"openai-completions","apiKey":"$AI_MODEL_ROUTER_API_KEY","models":[{"id":"old"}]}}}`,
	}
	for path, text := range files {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldConfig, oldPipeline := resolveConfig, executePipeline
	t.Cleanup(func() { resolveConfig, executePipeline = oldConfig, oldPipeline })
	resolveConfig = func() (paths.Config, error) { return cfg, nil }
	executePipeline = func(_ context.Context, _ paths.Config, doProbe, _ bool) (*pipeline.Pipeline, error) {
		if !doProbe {
			t.Fatal("sync did not request probes")
		}
		return &pipeline.Pipeline{
			Cfg: cfg, Patterns: []string{"*"}, Catalog: []model.CatalogEntry{{Provider: model.Bedrock, ID: "b1"}},
			RouterConfig:      files[cfg.ModelsJSON],
			Listings:          bedrock.Listings{ByRegion: map[string]map[string]bool{"eu-west-1": {"b1": true}}},
			BedrockCandidates: map[string]string{"b1": "eu-west-1"}, RouterCandidates: []string{"new"},
			Router: router.Deployment{Reachable: true, Deployed: []string{"new"}, Entries: map[string]router.ModelEntry{"new": {Entry: router.Entry{ID: "new", Name: "New", Input: []string{"text"}, ContextWindow: 200000, MaxTokens: 64000}}}},
			Probes: &probe.Batch{Outcomes: []probe.Outcome{{Provider: model.Bedrock, ID: "b1", Region: "eu-west-1", Status: probe.OK}, {Provider: model.Router, ID: "new", Status: probe.OK}}},
		}, nil
	}
	return cfg
}

func TestSyncDryRunRendersWithoutWriting(t *testing.T) {
	cfg := syncFixture(t)
	before, _ := os.ReadFile(cfg.ModelsJSON)
	var out, errs bytes.Buffer
	if code := commandSync([]string{"--dry-run"}, &out, &errs); code != 0 {
		t.Fatalf("code %d: %s", code, &errs)
	}
	after, _ := os.ReadFile(cfg.ModelsJSON)
	if !bytes.Equal(before, after) || !strings.Contains(out.String(), "+ new") || !strings.Contains(out.String(), "- old") {
		t.Fatalf("bad preview: %s", &out)
	}
}

func TestSyncWritesVerifiedDefinitions(t *testing.T) {
	cfg := syncFixture(t)
	var out, errs bytes.Buffer
	if code := commandSync(nil, &out, &errs); code != 0 {
		t.Fatalf("code %d: %s", code, &errs)
	}
	data, err := os.ReadFile(cfg.ModelsJSON)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"id": "new"`) || strings.Contains(string(data), `"id":"old"`) {
		t.Fatalf("wrong models: %s", data)
	}
}

func TestSyncRejectsConfigChangedDuringProbes(t *testing.T) {
	cfg := syncFixture(t)
	old := executePipeline
	executePipeline = func(ctx context.Context, cfg paths.Config, probes, strict bool) (*pipeline.Pipeline, error) {
		p, err := old(ctx, cfg, probes, strict)
		if writeErr := os.WriteFile(cfg.ModelsJSON, []byte(p.RouterConfig+"\n"), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		return p, err
	}
	before, _ := os.ReadFile(cfg.BedrockModelsJSON)
	var out, errs bytes.Buffer
	if commandSync(nil, &out, &errs) != 1 || !strings.Contains(errs.String(), "models.json changed") {
		t.Fatalf("unsafe config write: %s", &errs)
	}
	after, _ := os.ReadFile(cfg.BedrockModelsJSON)
	if !bytes.Equal(before, after) {
		t.Fatal("changed Bedrock map after failed preflight")
	}
}

func TestSyncRejectsPartialScanAndFailedRouter(t *testing.T) {
	for _, scenario := range []string{"region failed", "router failed"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := syncFixture(t)
			old := executePipeline
			executePipeline = func(ctx context.Context, cfg paths.Config, probes, strict bool) (*pipeline.Pipeline, error) {
				p, err := old(ctx, cfg, probes, strict)
				if scenario == "region failed" {
					p.Listings.Degraded = map[string]string{"ap-northeast-1": "timeout"}
				} else {
					p.Probes.Outcomes[1].Status = probe.FAIL
				}
				return p, err
			}
			before, _ := os.ReadFile(cfg.ModelsJSON)
			var out, errs bytes.Buffer
			if commandSync(nil, &out, &errs) != 1 {
				t.Fatal("unsafe scan accepted")
			}
			after, _ := os.ReadFile(cfg.ModelsJSON)
			if !bytes.Equal(before, after) {
				t.Fatal("unsafe scan wrote models")
			}
		})
	}
}

func TestReportJSONKeepsProgressOnStderr(t *testing.T) {
	syncFixture(t)
	old := executePipeline
	executePipeline = func(ctx context.Context, cfg paths.Config, probes, strict bool) (*pipeline.Pipeline, error) {
		progress.Report(ctx, "loading catalog")
		progress.Report(ctx, "probe 1/1 OK: model")
		return old(ctx, cfg, probes, strict)
	}
	var out, errs bytes.Buffer
	if code := commandReport([]string{"--json"}, &out, &errs); code != 0 {
		t.Fatalf("report code %d: %s", code, &errs)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not clean JSON: %v", err)
	}
	if !strings.Contains(errs.String(), "orchard: loading catalog") || !strings.Contains(errs.String(), "probe 1/1 OK") {
		t.Fatalf("missing progress: %s", &errs)
	}
}

func TestCircuitFailureStillEmitsJSONAndReasons(t *testing.T) {
	syncFixture(t)
	old := executePipeline
	executePipeline = func(ctx context.Context, cfg paths.Config, probes, strict bool) (*pipeline.Pipeline, error) {
		p, err := old(ctx, cfg, probes, strict)
		p.Probes.Tripped = true
		p.Probes.Outcomes[0].Status = probe.FAIL
		p.Probes.Outcomes[0].Systemic = true
		p.Probes.Outcomes[0].Reason = "(invalid Pi assistant response)"
		return p, err
	}
	var out, errs bytes.Buffer
	if code := commandReport([]string{"--json"}, &out, &errs); code != 1 {
		t.Fatalf("report code %d", code)
	}
	var doc struct {
		Meta struct {
			CircuitTripped bool `json:"circuitTripped"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || !doc.Meta.CircuitTripped {
		t.Fatalf("partial report missing: %v", err)
	}
	if !strings.Contains(errs.String(), "invalid Pi assistant response") {
		t.Fatalf("failure reason hidden: %s", &errs)
	}
}

func TestProbeRejectsInvalidTuningBeforeFetch(t *testing.T) {
	for _, args := range [][]string{{"--timeout", "0"}, {"--concurrency", "-1"}, {"--fail-circuit", "0"}} {
		var out, errs bytes.Buffer
		if code := commandProbe(args, &out, &errs); code != 2 {
			t.Fatalf("%v accepted: %d", args, code)
		}
	}
}
