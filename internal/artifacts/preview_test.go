package artifacts

import (
	"strings"
	"testing"
)

func TestPreviewShowsAddRemoveAndRegionMove(t *testing.T) {
	before := []byte(`{"models":{"a":"eu-west-1","b":"us-east-1","c":"ap-northeast-1"}}`)
	preview, err := PreviewBedrock(before, map[string]string{"a": "ap-southeast-2", "b": "us-east-1", "d": "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"+ d (eu-west-1)", "- c (ap-northeast-1)", "~ a (eu-west-1 -> ap-southeast-2)"} {
		if !strings.Contains(preview, item) {
			t.Errorf("missing %s in %s", item, preview)
		}
	}
}

func TestPreviewRouterShowsIDChanges(t *testing.T) {
	before := `{"providers":{"ai-model-router":{"models":[{"id":"a"},{"id":"b"}]}}}`
	after := `{"providers":{"ai-model-router":{"models":[{"id":"b"},{"id":"c"}]}}}`
	preview, err := PreviewRouter(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if preview != "+ c\n- a" {
		t.Errorf("router preview: %q", preview)
	}
}

func TestBedrockMapRejectsMissingModels(t *testing.T) {
	if _, err := BedrockMap([]byte(`{"generatedAt":"today"}`)); err == nil {
		t.Fatal("accepted invalid existing region map")
	}
}
