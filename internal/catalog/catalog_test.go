package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/luiul/orchard/internal/model"
)

func catalogText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "pi-list-models.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFailedListCannotSupplyPartialCatalog(t *testing.T) {
	old := runListModels
	t.Cleanup(func() { runListModels = old })
	runListModels = func(context.Context) (string, string, error) {
		return "ai-model-router m1 200K 64K yes no\n", "partial failure", errors.New("exit 1")
	}
	if _, err := Load(context.Background()); err == nil {
		t.Fatal("accepted output from failed catalog command")
	}
}

func TestParsesCapturedFixture(t *testing.T) {
	entries := Parse(catalogText(t))
	bedrock, router := 0, 0
	for _, e := range entries {
		switch e.Provider {
		case model.Bedrock:
			bedrock++
		case model.Router:
			router++
		}
	}
	if bedrock != 180 {
		t.Errorf("bedrock rows: got %d, want 180", bedrock)
	}
	if router != 11 {
		t.Errorf("router rows: got %d, want 11", router)
	}
}

func TestFieldsAreSplit(t *testing.T) {
	byID := map[string]model.CatalogEntry{}
	for _, e := range Parse(catalogText(t)) {
		byID[e.ID] = e
	}
	kimi, ok := byID["moonshotai/Kimi-K3"]
	if !ok {
		t.Fatal("moonshotai/Kimi-K3 missing")
	}
	if kimi.Provider != model.Router || kimi.Thinking != "yes" || kimi.Images != "yes" {
		t.Errorf("kimi: %+v", kimi)
	}
	haiku, ok := byID["eu.anthropic.claude-haiku-4-5-20251001-v1:0"]
	if !ok {
		t.Fatal("bedrock haiku missing")
	}
	if haiku.Provider != model.Bedrock || haiku.Context != "200K" {
		t.Errorf("haiku: %+v", haiku)
	}
}

func TestHeaderAndBlankLinesAreSkipped(t *testing.T) {
	text := "provider  model  context  max-out  thinking  images\n\nai-model-router  m1  200K  64K  yes  no\n"
	entries := Parse(text)
	if len(entries) != 1 || entries[0].ID != "m1" {
		t.Errorf("got %+v", entries)
	}
}

func TestMalformedLinesAreSkipped(t *testing.T) {
	entries := Parse("provider  model  context  max-out  thinking  images\nonly three fields here\n")
	if len(entries) != 0 {
		t.Errorf("got %+v", entries)
	}
}
