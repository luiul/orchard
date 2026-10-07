package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luiul/orchard/internal/artifacts"
	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/probe"
	"github.com/luiul/orchard/internal/progress"
	"github.com/luiul/orchard/internal/router"
)

// runProbes tries further discovered regions when the first one fails. All
// router IDs are probed using the exact generated definitions sync would
// write, in an isolated Pi agent directory.
func (p *Pipeline) runProbes(ctx context.Context) error {
	cfg := p.Cfg
	var agentDir string
	if p.Router.Reachable {
		progress.Report(ctx, "staging %d router definitions for Pi probes", len(p.Router.Deployed))
		for _, id := range p.Router.Deployed {
			entry, ok := p.Router.Entries[id]
			if !ok || entry.Entry.ID != id {
				return fmt.Errorf("router deployment has no definition for %q", id)
			}
		}
		data, err := readModelConfig(cfg.ModelsJSON)
		if err != nil {
			return fmt.Errorf("read router config: %w", err)
		}
		p.RouterConfig = string(data)
		if cfg.RouterBaseURL != "" {
			var source struct {
				Providers map[string]struct {
					BaseURL string `json:"baseUrl"`
				} `json:"providers"`
			}
			if err := json.Unmarshal(data, &source); err != nil {
				return fmt.Errorf("read router config: %w", err)
			}
			if strings.TrimRight(source.Providers[model.Router].BaseURL, "/") != strings.TrimRight(cfg.RouterBaseURL, "/") {
				return fmt.Errorf("router discovery endpoint differs from models.json; configure both to the same endpoint")
			}
		}
		agentDir, err = stageRouterText(p.RouterConfig, p.Router.Entries)
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(agentDir) }()
	}
	bedrockIDs := sortedKeys(p.BedrockRegions)
	remaining := make(map[string][]string, len(bedrockIDs))
	for _, id := range bedrockIDs {
		remaining[id] = p.BedrockRegions[id]
	}
	outcomes := make([]probe.Outcome, 0)
	tripped := false
	round := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var targets []probe.Target
		for _, id := range bedrockIDs {
			if len(remaining[id]) > 0 {
				targets = append(targets, probe.Target{Provider: model.Bedrock, ID: id, Region: remaining[id][0]})
				remaining[id] = remaining[id][1:]
			}
		}
		if len(outcomes) == 0 && p.Router.Reachable {
			for _, id := range p.RouterCandidates {
				targets = append(targets, probe.Target{Provider: model.Router, ID: id, AgentDir: agentDir})
			}
		}
		if len(targets) == 0 || tripped {
			break
		}
		round++
		progress.Report(ctx, "starting probe round %d: %d calls (%d workers, %ds timeout)", round, len(targets), cfg.ProbeConcurrency, cfg.ProbeTimeout)
		batch := probeAll(ctx, targets, cfg)
		if len(batch.Outcomes) != len(targets) {
			return fmt.Errorf("probe returned %d results for %d targets", len(batch.Outcomes), len(targets))
		}
		for i, result := range batch.Outcomes {
			if result.Provider != targets[i].Provider || result.ID != targets[i].ID || result.Region != targets[i].Region {
				return fmt.Errorf("probe returned a result for the wrong target: %s", targets[i].ID)
			}
			if result.Provider == model.Bedrock && result.Status == probe.OK {
				remaining[result.ID] = nil
			}
			if result.Status != probe.OK && result.Status != probe.FAIL && result.Status != probe.SKIP {
				return fmt.Errorf("invalid probe result for %s", targets[i].ID)
			}
			outcomes = append(outcomes, result)
		}
		tripped = batch.Tripped
	}
	p.Probes = &probe.Batch{Outcomes: outcomes, Tripped: tripped}
	return nil
}

// stageRouterConfig contains only the generated models and the provider's
// endpoint/auth settings. No extensions, credentials, or project settings are
// copied. The caller removes the temporary directory after probing.
func stageRouterConfig(path string, entries map[string]router.ModelEntry) (string, error) {
	data, err := readModelConfig(path)
	if err != nil {
		return "", fmt.Errorf("read router config: %w", err)
	}
	return stageRouterText(string(data), entries)
}

func stageRouterText(source string, entries map[string]router.ModelEntry) (string, error) {
	for id, entry := range entries {
		if id == "" || entry.Entry.ID != id {
			return "", fmt.Errorf("router definition missing or mismatched for %q", id)
		}
		if entry.Entry.ContextWindow <= 0 || entry.Entry.MaxTokens <= 0 {
			return "", fmt.Errorf("router definition for %q has no usable token limits", id)
		}
	}
	models := make([]router.Entry, 0, len(entries))
	for _, entry := range entries {
		models = append(models, entry.Entry)
	}
	text, err := artifacts.RenderModelsJSON(source, models)
	if err != nil {
		return "", fmt.Errorf("stage router config: %w", err)
	}
	// A probe must not load unrelated user extensions or credentials. Keep
	// only the staged provider definition with its environment key reference.
	var doc struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	provider, ok := doc.Providers[model.Router]
	if !ok {
		return "", fmt.Errorf("router provider missing from staged config")
	}
	var config struct {
		BaseURL string `json:"baseUrl"`
		API     string `json:"api"`
		APIKey  string `json:"apiKey"`
	}
	if err := json.Unmarshal(provider, &config); err != nil {
		return "", err
	}
	if config.BaseURL == "" || config.API != "openai-completions" || config.APIKey != "$AI_MODEL_ROUTER_API_KEY" {
		return "", fmt.Errorf("staged router provider has unexpected endpoint or authentication")
	}
	text = fmt.Sprintf(`{"providers":{"%s":%s}}`, model.Router, provider)
	dir, err := os.MkdirTemp("", "orchard-router-probe-*")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(text), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
