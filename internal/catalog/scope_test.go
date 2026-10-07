package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/luiul/orchard/internal/model"
)

func TestResolveScopePreservesPiProviderQualifiedMatches(t *testing.T) {
	old := runScope
	t.Cleanup(func() { runScope = old })
	runScope = func(_ context.Context, payload []byte) ([]byte, error) {
		var input struct {
			Patterns   []string `json:"patterns"`
			ModelsPath string   `json:"modelsPath"`
		}
		if err := json.Unmarshal(payload, &input); err != nil {
			t.Fatal(err)
		}
		if input.Patterns[0] != "amazon-bedrock/JP.*:high" || input.ModelsPath != "/test/models.json" {
			t.Fatalf("resolver input: %+v", input)
		}
		return []byte(`{"amazon-bedrock/JP.*:high":["amazon-bedrock\u0000jp.anthropic.x"],"missing":[]}`), nil
	}
	result, err := ResolveScope(context.Background(), []model.CatalogEntry{{Provider: model.Bedrock, ID: "jp.anthropic.x"}}, []string{"amazon-bedrock/JP.*:high", "missing"}, "/test/models.json")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Selected[model.Bedrock+"\x00jp.anthropic.x"] || len(result.Matches["missing"]) != 0 {
		t.Fatalf("scope: %+v", result)
	}
}

func TestAbsentAndEmptyScopesIncludeAllModels(t *testing.T) {
	models := []model.CatalogEntry{{Provider: model.Bedrock, ID: "au.anthropic.x"}}
	all, err := ResolveScope(context.Background(), models, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := ResolveScope(context.Background(), models, []string{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Selected) != 1 || len(empty.Selected) != 1 {
		t.Fatalf("all=%v empty=%v", all, empty)
	}
}

func TestUnmatchedScopeFallsBackToAllModels(t *testing.T) {
	old := runScope
	t.Cleanup(func() { runScope = old })
	runScope = func(context.Context, []byte) ([]byte, error) {
		return []byte(`{"retired-*":[]}`), nil
	}
	models := []model.CatalogEntry{{Provider: model.Bedrock, ID: "jp.anthropic.x"}}
	result, err := ResolveScope(context.Background(), models, []string{"retired-*"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Selected) != 1 || len(result.Matches["retired-*"]) != 0 {
		t.Fatalf("Pi fallback lost or unmatched pattern hidden: %+v", result)
	}
}

func TestPiScopeIntegration(t *testing.T) {
	if os.Getenv("ORCHARD_PI_INTEGRATION") != "1" {
		t.Skip("set ORCHARD_PI_INTEGRATION=1 to test the installed Pi resolver")
	}
	path := filepath.Join(t.TempDir(), "models.json")
	text := `{"providers":{"fixture-router":{"baseUrl":"https://test.invalid/v1","api":"openai-completions","apiKey":"local-test","models":[{"id":"same","name":"Fixture Friendly Model","contextWindow":1000,"maxTokens":100}]}}}`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	models := []model.CatalogEntry{
		{Provider: model.Bedrock, ID: "jp.anthropic.x"},
		{Provider: model.Bedrock, ID: "same"},
		{Provider: "fixture-router", ID: "same"},
	}
	patterns := []string{"amazon-bedrock/JP.*:high", "fixture-router/same", "Friendly"}
	result, err := ResolveScope(context.Background(), models, patterns, path)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Selected[model.Bedrock+"\x00jp.anthropic.x"] || !result.Selected["fixture-router\x00same"] || result.Selected[model.Bedrock+"\x00same"] {
		t.Fatalf("provider-qualified scope mismatch: %+v", result)
	}
	if len(result.Matches["Friendly"]) != 1 || result.Matches["Friendly"][0] != "fixture-router\x00same" {
		t.Fatalf("Pi model name matching lost: %+v", result.Matches)
	}
}

func TestScopeResolverFailureIsNotEmptyScope(t *testing.T) {
	old := runScope
	t.Cleanup(func() { runScope = old })
	runScope = func(context.Context, []byte) ([]byte, error) { return nil, errors.New("Pi unavailable") }
	if _, err := ResolveScope(context.Background(), nil, []string{"*"}, ""); err == nil {
		t.Fatal("scope failure ignored")
	}
}
