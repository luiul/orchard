package artifacts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luiul/orchard/internal/router"
)

func TestRenderBedrockModelsIsKeySortedAndStable(t *testing.T) {
	text, err := RenderBedrockModels(map[string]string{"b": "us-east-1", "a": "eu-west-1"}, "eu-west-1", "2026-10-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "defaultRegion": "eu-west-1",
  "generatedAt": "2026-10-01T00:00:00Z",
  "models": {
    "a": "eu-west-1",
    "b": "us-east-1"
  }
}
`
	if text != want {
		t.Errorf("got:\n%s\nwant:\n%s", text, want)
	}
	// Unchanged inputs produce byte-identical output.
	again, err := RenderBedrockModels(map[string]string{"a": "eu-west-1", "b": "us-east-1"}, "eu-west-1", "2026-10-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if again != text {
		t.Error("not byte-identical across input orders")
	}
}

func TestAtomicWritePreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, "new"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("permissions: %o, want 640", got)
	}
}

func TestWriteBedrockModelsIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bedrock-models.json")
	if err := WriteBedrockModels(path, map[string]string{"m": "eu-west-1"}, "eu-west-1", "2026-10-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteBedrockModels(path, map[string]string{"m": "eu-west-1"}, "eu-west-1", "2026-10-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Error("not byte-identical")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// sampleModelsJSON mirrors the Python test fixture, as json.dumps(indent=2)
// would render it.
func sampleModelsJSON() string {
	return `{
  "providers": {
    "amazon-bedrock": {
      "modelOverrides": {
        "x.y": {
          "cost": {
            "input": 1
          }
        }
      }
    },
    "ai-model-router": {
      "baseUrl": "https://router.test/v1",
      "api": "openai-completions",
      "apiKey": "$AI_MODEL_ROUTER_API_KEY",
      "compat": {
        "supportsReasoningEffort": true
      },
      "models": [
        {
          "id": "old-model",
          "name": "Old",
          "reasoning": false,
          "input": [
            "text"
          ],
          "cost": {
            "input": 1,
            "output": 2,
            "cacheRead": 0,
            "cacheWrite": 0
          },
          "contextWindow": 100,
          "maxTokens": 50
        }
      ]
    }
  }
}
`
}

func sampleEntry(id string) router.Entry {
	return router.Entry{
		ID:            id,
		Name:          strings.ToUpper(id[:1]) + id[1:],
		Reasoning:     true,
		Input:         []string{"text", "image"},
		Cost:          router.Cost{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.2},
		ContextWindow: 1000,
		MaxTokens:     100,
	}
}

func splicedModels(t *testing.T, text string) []router.Entry {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc["providers"].(map[string]any)["ai-model-router"].(map[string]any)["models"])
	if err != nil {
		t.Fatal(err)
	}
	var models []router.Entry
	if err := json.Unmarshal(raw, &models); err != nil {
		t.Fatal(err)
	}
	return models
}

func TestSplicePreservesEverythingElseByteForByte(t *testing.T) {
	original := sampleModelsJSON()
	spliced, err := SpliceRouterModels(original, []router.Entry{sampleEntry("new-model")})
	if err != nil {
		t.Fatal(err)
	}
	models := splicedModels(t, spliced)
	if len(models) != 1 || models[0].ID != "new-model" {
		t.Errorf("models: %v", models)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(spliced), &doc); err != nil {
		t.Fatal(err)
	}
	routerSection := doc["providers"].(map[string]any)["ai-model-router"].(map[string]any)
	if routerSection["baseUrl"] != "https://router.test/v1" {
		t.Errorf("baseUrl: %v", routerSection["baseUrl"])
	}
	if routerSection["apiKey"] != "$AI_MODEL_ROUTER_API_KEY" {
		t.Errorf("apiKey: %v", routerSection["apiKey"])
	}
	// Everything outside the models array is byte-identical.
	before := original[:strings.Index(original, `"models"`)]
	if !strings.HasPrefix(spliced, before) {
		t.Errorf("spliced text diverges before the models array:\n%s", spliced[:min(len(spliced), len(before)+40)])
	}
}

func TestSpliceRemovesExactlyTheUndeployedEntry(t *testing.T) {
	spliced, err := SpliceRouterModels(sampleModelsJSON(), nil) // gateway stopped deploying everything
	if err != nil {
		t.Fatal(err)
	}
	if models := splicedModels(t, spliced); len(models) != 0 {
		t.Errorf("models: %v", models)
	}
}

func TestSpliceInputArraysStayInline(t *testing.T) {
	spliced, err := SpliceRouterModels(sampleModelsJSON(), []router.Entry{sampleEntry("m")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spliced, `"input": ["text", "image"]`) {
		t.Errorf("input array not inline:\n%s", spliced)
	}
}

func TestSpliceRoundTripIsStable(t *testing.T) {
	once, err := SpliceRouterModels(sampleModelsJSON(), []router.Entry{sampleEntry("b"), sampleEntry("a")})
	if err != nil {
		t.Fatal(err)
	}
	twice, err := SpliceRouterModels(once, []router.Entry{sampleEntry("b"), sampleEntry("a")})
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Error("round trip not byte-stable")
	}
}

func TestWriteModelsJSONSortsByID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(sampleModelsJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteModelsJSON(path, []router.Entry{sampleEntry("zeta"), sampleEntry("alpha")}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	models := splicedModels(t, string(data))
	if len(models) != 2 || models[0].ID != "alpha" || models[1].ID != "zeta" {
		t.Errorf("ids: %v", models)
	}
}

func TestSpliceRealModelsJSONPreservesOtherSections(t *testing.T) {
	path := os.ExpandEnv("$HOME/dotfiles/pi/.pi/agent/models.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skip("dotfiles checkout not present")
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	providers := doc["providers"].(map[string]any)
	raw, err := json.Marshal(providers["ai-model-router"].(map[string]any)["models"])
	if err != nil {
		t.Fatal(err)
	}
	var current []router.Entry
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatal(err)
	}
	spliced, err := SpliceRouterModels(string(data), current)
	if err != nil {
		t.Fatal(err)
	}
	var check map[string]any
	if err := json.Unmarshal([]byte(spliced), &check); err != nil {
		t.Fatal(err)
	}
	checkProviders := check["providers"].(map[string]any)
	if !jsonEqual(checkProviders["amazon-bedrock"], providers["amazon-bedrock"]) {
		t.Error("amazon-bedrock section changed")
	}
	routerBefore := providers["ai-model-router"].(map[string]any)
	routerAfter := checkProviders["ai-model-router"].(map[string]any)
	for _, key := range []string{"baseUrl", "compat"} {
		if !jsonEqual(routerAfter[key], routerBefore[key]) {
			t.Errorf("%s changed", key)
		}
	}
}

func jsonEqual(a, b any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

func TestSettingsDrift(t *testing.T) {
	tmp := t.TempDir()
	dotfiles := filepath.Join(tmp, "dot.json")
	live := filepath.Join(tmp, "live.json")
	if err := os.WriteFile(dotfiles, []byte(`{"enabledModels": ["a", "b"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"enabledModels": ["a", "b"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if warning := SettingsDrift(dotfiles, live); warning != "" {
		t.Errorf("unexpected drift: %s", warning)
	}
	if err := os.WriteFile(live, []byte(`{"enabledModels": ["a"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if warning := SettingsDrift(dotfiles, live); warning == "" {
		t.Error("expected drift warning")
	}
}

func TestReadPatternsRejectsInvalidEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"enabledModels": ["a*", 42, "b*"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPatterns(path); err == nil {
		t.Fatal("accepted invalid enabledModels entry")
	}
}
