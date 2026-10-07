package artifacts

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// BedrockMap parses the saved map so sync can protect previously verified
// models before replacing the file. A missing file is an empty map.
func BedrockMap(before []byte) (map[string]string, error) {
	if len(before) == 0 {
		return map[string]string{}, nil
	}
	var current struct {
		Models map[string]string `json:"models"`
	}
	if err := json.Unmarshal(before, &current); err != nil {
		return nil, fmt.Errorf("parse current Bedrock map: %w", err)
	}
	if current.Models == nil {
		return nil, fmt.Errorf("current Bedrock map has no models object")
	}
	return current.Models, nil
}

// PreviewBedrock describes the model IDs and regions a sync would change.
func PreviewBedrock(before []byte, after map[string]string) (string, error) {
	previous, err := BedrockMap(before)
	if err != nil {
		return "", err
	}
	var changes []string
	for id, region := range after {
		old, ok := previous[id]
		if !ok {
			changes = append(changes, "+ "+id+" ("+region+")")
		} else if old != region {
			changes = append(changes, "~ "+id+" ("+old+" -> "+region+")")
		}
	}
	for id, region := range previous {
		if _, ok := after[id]; !ok {
			changes = append(changes, "- "+id+" ("+region+")")
		}
	}
	sort.Strings(changes)
	return strings.Join(changes, "\n"), nil
}

// PreviewRouter shows which deployed IDs would be added or removed from Pi.
func PreviewRouter(before, after string) (string, error) {
	ids := func(text string) (map[string]bool, error) {
		var doc struct {
			Providers map[string]struct {
				Models []struct {
					ID string `json:"id"`
				} `json:"models"`
			} `json:"providers"`
		}
		if err := json.Unmarshal([]byte(text), &doc); err != nil {
			return nil, err
		}
		if len(doc.Providers) == 0 {
			return nil, fmt.Errorf("models.json has no providers")
		}
		provider, ok := doc.Providers["ai-model-router"]
		if !ok {
			return nil, fmt.Errorf("models.json has no ai-model-router provider")
		}
		out := map[string]bool{}
		for _, model := range provider.Models {
			if model.ID == "" || out[model.ID] {
				return nil, fmt.Errorf("models.json has an empty or duplicate router model ID")
			}
			out[model.ID] = true
		}
		return out, nil
	}
	old, err := ids(before)
	if err != nil {
		return "", err
	}
	newIDs, err := ids(after)
	if err != nil {
		return "", err
	}
	var changes []string
	for id := range newIDs {
		if !old[id] {
			changes = append(changes, "+ "+id)
		}
	}
	for id := range old {
		if !newIDs[id] {
			changes = append(changes, "- "+id)
		}
	}
	sort.Strings(changes)
	return strings.Join(changes, "\n"), nil
}
