package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luiul/orchard/internal/paths"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testCfg(t *testing.T) paths.Config {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("PI_PROVIDER_DATA_DIR", filepath.Join(tmp, "no-such-dir")) // no bundled data
	return paths.Config{
		Dotfiles:      tmp,
		ModelRegistry: filepath.Join(tmp, "model-registry.json"),
		RouterAPIKey:  "test-key",
	}
}

// fixtureServer serves the captured gateway responses.
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write(fixture(t, "router-models.json"))
		case "/v1/model/info":
			_, _ = w.Write(fixture(t, "router-model-info.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestLiveFixtureShape(t *testing.T) {
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fixture(t, "router-models.json"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Data) != 13 {
		t.Errorf("got %d deployed ids, want 13", len(doc.Data))
	}
	found := false
	for _, item := range doc.Data {
		if item.ID == "claude-opus-5-5" {
			found = true
		}
	}
	if !found {
		t.Error("claude-opus-5-5 missing (deployed but unknown to pi when captured)")
	}
}

func TestFetchDeployedIDsAndEntries(t *testing.T) {
	server := fixtureServer(t)
	defer server.Close()
	cfg := testCfg(t)
	cfg.RouterBaseURL = server.URL + "/v1"

	deployment, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !deployment.Reachable {
		t.Fatalf("unreachable: %s", deployment.Err)
	}
	if len(deployment.Deployed) != 13 {
		t.Errorf("got %d deployed, want 13", len(deployment.Deployed))
	}
	entry := deployment.Entries["claude-sonnet-5"].Entry
	if entry.ID != "claude-sonnet-5" {
		t.Errorf("id: %q", entry.ID)
	}
	wantCost := Cost{Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5}
	if entry.Cost != wantCost {
		t.Errorf("cost: got %+v, want %+v", entry.Cost, wantCost)
	}
	if entry.ContextWindow != 1_000_000 {
		t.Errorf("contextWindow: %d", entry.ContextWindow)
	}
	if entry.MaxTokens != 128_000 {
		t.Errorf("maxTokens: %d", entry.MaxTokens)
	}
	if !entry.Reasoning {
		t.Error("reasoning: got false")
	}
	if strings.Join(entry.Input, ",") != "text,image" {
		t.Errorf("input: %v", entry.Input)
	}
	if src := deployment.Entries["claude-sonnet-5"].Sources["cost"]; src != "router" {
		t.Errorf("cost source: %q", src)
	}
}

func TestFetchGatewayResponseValidation(t *testing.T) {
	const models = `{"data":[{"id":"model-one","object":"model"}]}`
	const info = `{"data":[{"model_name":"model-one","model_info":{"max_input_tokens":4096},"litellm_params":{}}]}`
	tests := []struct {
		name, models, info, errorPath string
	}{
		{name: "healthy", models: models, info: info},
		{name: "models missing data", models: `{}`, info: info, errorPath: "/models"},
		{name: "models null data", models: `{"data":null}`, info: info, errorPath: "/models"},
		{name: "models wrong data shape", models: `{"data":{}}`, info: info, errorPath: "/models"},
		{name: "models empty data", models: `{"data":[]}`, info: info, errorPath: "/models"},
		{name: "models invalid items", models: `{"data":[null,42]}`, info: info, errorPath: "/models"},
		{name: "models missing ids", models: `{"data":[{"object":"model"}]}`, info: info, errorPath: "/models"},
		{name: "models invalid id type", models: `{"data":[{"id":42}]}`, info: info, errorPath: "/models"},
		{name: "models blank id", models: `{"data":[{"id":"  "}]}`, info: info, errorPath: "/models"},
		{name: "models mixed valid and invalid items", models: `{"data":[{"id":"model-one"},{}]}`, info: info, errorPath: "/models"},
		{name: "info missing data", models: models, info: `{}`, errorPath: "/model/info"},
		{name: "info wrong data shape", models: models, info: `{"data":{}}`, errorPath: "/model/info"},
		{name: "info empty data is healthy", models: models, info: `{"data":[]}`},
		{name: "info invalid item", models: models, info: `{"data":[false]}`, errorPath: "/model/info"},
		{name: "info missing model name", models: models, info: `{"data":[{}]}`, errorPath: "/model/info"},
		{name: "info invalid metadata", models: models, info: `{"data":[{"model_name":"model-one","model_info":[]}]}`, errorPath: "/model/info"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/models":
					_, _ = w.Write([]byte(tt.models))
				case "/v1/model/info":
					_, _ = w.Write([]byte(tt.info))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			cfg := testCfg(t)
			cfg.RouterBaseURL = server.URL + "/v1"
			deployment, err := Fetch(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Fetch returned hard error: %v", err)
			}
			if tt.errorPath != "" {
				if deployment.Reachable || !strings.Contains(deployment.Err, tt.errorPath) {
					t.Fatalf("malformed response: got %+v, want unreachable with error from %s", deployment, tt.errorPath)
				}
				if len(deployment.Deployed) != 0 || len(deployment.Entries) != 0 {
					t.Fatalf("malformed response produced deployed models: %+v", deployment)
				}
				return
			}
			if !deployment.Reachable || deployment.Err != "" {
				t.Fatalf("healthy response: got %+v", deployment)
			}
			if len(deployment.Deployed) != 1 || deployment.Deployed[0] != "model-one" {
				t.Fatalf("deployed: %v", deployment.Deployed)
			}
			if tt.name == "healthy" {
				if got := deployment.Entries["model-one"].Entry.ContextWindow; got != 4096 {
					t.Errorf("context window: got %d, want 4096", got)
				}
			}
		})
	}
}

func TestUnreachableGatewayDegradesWithoutRaising(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	server.Close() // nothing is listening: every request fails
	cfg := testCfg(t)
	cfg.RouterBaseURL = server.URL + "/v1"
	deployment, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Reachable {
		t.Fatal("expected unreachable")
	}
	if deployment.Err == "" {
		t.Error("error is empty")
	}
	if len(deployment.Deployed) != 0 {
		t.Errorf("deployed: %v", deployment.Deployed)
	}
}

func TestMissingAPIKeyIsUnreachable(t *testing.T) {
	cfg := testCfg(t)
	cfg.RouterAPIKey = ""
	deployment, err := Fetch(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Reachable {
		t.Fatal("expected unreachable")
	}
	if !strings.Contains(deployment.Err, "AI_MODEL_ROUTER_API_KEY") {
		t.Errorf("error: %q", deployment.Err)
	}
}

func fp(v float64) *float64 { return &v }
func sp(v string) *string   { return &v }
func bp(v bool) *bool       { return &v }

func TestFallbackChainNullMetadata(t *testing.T) {
	bundled := map[string]bundledModel{
		"gpt-6-luna": {
			Name:      sp("GPT-6 Luna"),
			Reasoning: bp(true),
			Input:     []string{"text", "image"},
			Cost: &struct {
				Input      *float64 `json:"input"`
				Output     *float64 `json:"output"`
				CacheRead  *float64 `json:"cacheRead"`
				CacheWrite *float64 `json:"cacheWrite"`
			}{Input: fp(0.1), Output: fp(0.2), CacheRead: fp(0.02), CacheWrite: fp(0.25)},
			ContextWindow: fp(1_000_000),
			MaxTokens:     fp(128_000),
		},
	}
	entry := buildEntry("gpt-6-luna", nil, nil, bundled, nil)
	if entry.Entry.Name != "GPT-6 Luna" {
		t.Errorf("name: %q", entry.Entry.Name)
	}
	if entry.Entry.Cost.Input != 0.1 {
		t.Errorf("cost.input: %v", entry.Entry.Cost.Input)
	}
	if !entry.Entry.Reasoning {
		t.Error("reasoning: got false")
	}
	for field, source := range entry.Sources {
		if source != "bundled" {
			t.Errorf("sources[%s] = %q, want bundled", field, source)
		}
	}

	overrides := map[string]map[string]any{
		"gpt-6-luna": {"name": "Luna Six", "cost": map[string]any{"input": 9.0}},
	}
	entry = buildEntry("gpt-6-luna", nil, nil, nil, overrides)
	if entry.Entry.Name != "Luna Six" || entry.Sources["name"] != "override" {
		t.Errorf("name: %q (%s)", entry.Entry.Name, entry.Sources["name"])
	}
	if entry.Entry.Cost.Input != 9 || entry.Sources["cost"] != "override" {
		t.Errorf("cost.input: %v (%s)", entry.Entry.Cost.Input, entry.Sources["cost"])
	}
	if entry.Entry.Cost.Output != 0 {
		t.Errorf("cost.output: %v, want default 0", entry.Entry.Cost.Output)
	}

	entry = buildEntry("mystery-model", nil, nil, nil, nil)
	if strings.Join(entry.Entry.Input, ",") != "text" {
		t.Errorf("input: %v", entry.Entry.Input)
	}
	if entry.Entry.Reasoning {
		t.Error("reasoning: got true")
	}
	for field, source := range entry.Sources {
		if source != "default" {
			t.Errorf("sources[%s] = %q, want default", field, source)
		}
	}
}

func TestOverrideWinsOverPresentButWrongRouterData(t *testing.T) {
	// The registry is human authority: it corrects live values that break
	// the backend, not just nulls (the DeepSeek maxTokens 384000 -> 262144
	// case).
	infoItem := map[string]any{
		"model_name": "deepseek-ai/DeepSeek-V4-Flash-0731",
		"model_info": map[string]any{
			"max_output_tokens":  384000.0,
			"max_input_tokens":   1048576.0,
			"supports_reasoning": true,
		},
		"litellm_params": map[string]any{},
	}
	overrides := map[string]map[string]any{
		"deepseek-ai/DeepSeek-V4-Flash-0731": {"maxTokens": 262144.0},
	}
	entry := buildEntry("deepseek-ai/DeepSeek-V4-Flash-0731", nil, infoItem, nil, overrides)
	if entry.Entry.MaxTokens != 262144 || entry.Sources["maxTokens"] != "override" {
		t.Errorf("maxTokens: %d (%s)", entry.Entry.MaxTokens, entry.Sources["maxTokens"])
	}
	if entry.Entry.ContextWindow != 1048576 || entry.Sources["contextWindow"] != "router" {
		t.Errorf("contextWindow: %d (%s)", entry.Entry.ContextWindow, entry.Sources["contextWindow"])
	}
}

func TestRegistryOverridesMatchNormalizedIDs(t *testing.T) {
	overrides := map[string]map[string]any{"glm-5.3-flash": {"name": "GLM 5.3 Flash"}}
	entry := buildEntry("zai-org/GLM-5.3-Flash", nil, nil, nil, overrides)
	if entry.Entry.Name != "GLM 5.3 Flash" || entry.Sources["name"] != "override" {
		t.Errorf("name: %q (%s)", entry.Entry.Name, entry.Sources["name"])
	}
}

func TestPerMillionConversion(t *testing.T) {
	if got := perMillion(fp(0.000002)); got == nil || *got != 2 {
		t.Errorf("2e-6: %v", got)
	}
	if got := perMillion(fp(2e-7)); got == nil || *got != 0.2 {
		t.Errorf("2e-7: %v", got)
	}
	if got := perMillion(nil); got != nil {
		t.Errorf("nil: %v", got)
	}
}

func TestNormalizeModelID(t *testing.T) {
	if got := NormalizeModelID("zai-org/GLM-5.3-Flash"); got != "glm-5.3-flash" {
		t.Errorf("got %q", got)
	}
	if got := NormalizeModelID("claude-opus-5-5"); got != "claude-opus-5-5" {
		t.Errorf("got %q", got)
	}
}

func TestHumanize(t *testing.T) {
	if got := humanize("deepseek-ai/DeepSeek-V4.1-Flash"); got != "Deepseek V4 1 Flash" {
		t.Errorf("got %q", got)
	}
}

func TestLoadRegistryMissingFile(t *testing.T) {
	registry, err := LoadRegistry(filepath.Join(t.TempDir(), "model-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.ProbeRegionOverrides) != 0 || len(registry.RouterModelOverrides) != 0 {
		t.Errorf("got %+v", registry)
	}
}

func TestLoadRegistryReadsOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-registry.json")
	doc := `{"probeRegionOverrides": {"a": "b"}, "routerModelOverrides": {"m": {"name": "M"}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if registry.ProbeRegionOverrides["a"] != "b" {
		t.Errorf("probeRegionOverrides: %v", registry.ProbeRegionOverrides)
	}
	if registry.RouterModelOverrides["m"]["name"] != "M" {
		t.Errorf("routerModelOverrides: %v", registry.RouterModelOverrides)
	}
}

func TestLoadRegistryInvalidJSONIsAHardError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-registry.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRegistry(path); err == nil {
		t.Fatal("expected an error")
	}
}
