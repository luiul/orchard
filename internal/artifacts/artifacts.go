// Package artifacts reads Pi's curated model scope and renders the two files
// Orchard may sync: the verified Bedrock region map and the deployed router
// model definitions. It never changes Pi's enabledModels or default model.
package artifacts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/router"
)

// ReadPatterns reads the optional curated Pi model scope. Missing means Pi
// has no scope restriction; invalid entries are errors, never ignored.
func ReadPatterns(settingsPath string) ([]string, error) {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		EnabledModels json.RawMessage `json:"enabledModels"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.EnabledModels) == 0 {
		return nil, nil
	}
	var patterns []string
	if err := json.Unmarshal(doc.EnabledModels, &patterns); err != nil {
		return nil, fmt.Errorf("enabledModels must be a list of patterns: %w", err)
	}
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("enabledModels has an empty pattern")
		}
	}
	return patterns, nil
}

// SettingsDrift warns when the live (not stowed, pi-rewritten) settings
// disagree with the dotfiles copy on enabledModels. "" means in sync.
func SettingsDrift(dotfilesSettings, liveSettings string) string {
	dot, errDot := ReadPatterns(dotfilesSettings)
	live, errLive := ReadPatterns(liveSettings)
	if errDot != nil || errLive != nil {
		err := errDot
		if err == nil {
			err = errLive
		}
		return fmt.Sprintf("could not compare settings files: %v", err)
	}
	if reflect.DeepEqual(dot, live) {
		return ""
	}
	return fmt.Sprintf(
		"live %s enabledModels differs from the dotfiles copy (%d vs %d entries). "+
			"Save your intended scope in Pi, then refresh the dotfiles snapshot.",
		liveSettings, len(live), len(dot))
}

// AtomicWrite replaces one file through a same-directory temporary file.
// It preserves the old permissions; separate calls are not one transaction.
func AtomicWrite(path, text string) error {
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// marshalIndent keeps artifact renderers using one JSON formatting helper.
var marshalIndent = model.MarshalIndent

// RenderBedrockModels renders sorted JSON for the verified Bedrock map.
func RenderBedrockModels(mapping map[string]string, defaultRegion, generatedAt string) (string, error) {
	if mapping == nil {
		mapping = map[string]string{}
	}
	doc := map[string]any{
		"generatedAt":   generatedAt,
		"defaultRegion": defaultRegion,
		"models":        mapping,
	}
	text, err := marshalIndent(doc)
	if err != nil {
		return "", err
	}
	return text + "\n", nil
}

// WriteBedrockModels renders and atomically writes bedrock-models.json.
func WriteBedrockModels(path string, mapping map[string]string, defaultRegion, generatedAt string) error {
	text, err := RenderBedrockModels(mapping, defaultRegion, generatedAt)
	if err != nil {
		return err
	}
	return AtomicWrite(path, text)
}

// findValueSpan does a string-aware bracket match: given the byte index of
// an opening '[' or '{', it returns the (start, end) span of the whole
// JSON value.
func findValueSpan(text string, start int) (int, int, error) {
	openToClose := map[byte]byte{'[': ']', '{': '}'}
	stack := []byte{openToClose[text[start]]}
	inString, escaped := false, false
	i := start + 1
	for i < len(text) && len(stack) > 0 {
		ch := text[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case ch == '"':
			inString = true
		case ch == '[' || ch == '{':
			stack = append(stack, openToClose[ch])
		case ch == ']' || ch == '}':
			if len(stack) == 0 || ch != stack[len(stack)-1] {
				return 0, 0, fmt.Errorf("unbalanced JSON near offset %d", i)
			}
			stack = stack[:len(stack)-1]
		}
		i++
	}
	if len(stack) > 0 {
		return 0, 0, fmt.Errorf("unterminated JSON value")
	}
	return start, i, nil
}

// inputArrayRe matches a multi-line rendered "input" array so it can be
// collapsed back to one line, like the hand-written entries.
var inputArrayRe = regexp.MustCompile(`"input": \[\s*("(?:text|image)"(?:\s*,\s*"(?:text|image)")*)\s*\]`)
var commaSpaceRe = regexp.MustCompile(`\s*,\s*`)

// SpliceRouterModels replaces exactly the
// providers["ai-model-router"].models array in models.json text,
// preserving every other byte (formatting included). The result is
// re-parsed and checked against the original: everything must be identical
// except that one array.
func SpliceRouterModels(text string, newModels []router.Entry) (string, error) {
	var original map[string]any
	if err := json.Unmarshal([]byte(text), &original); err != nil {
		return "", err
	}
	providers, ok := original["providers"].(map[string]any)
	if !ok {
		return "", fmt.Errorf(`models.json has no "providers" object`)
	}
	routerSection, ok := providers["ai-model-router"].(map[string]any)
	if !ok {
		return "", fmt.Errorf(`models.json has no providers["ai-model-router"]`)
	}
	originalModels, ok := routerSection["models"]
	if !ok {
		return "", fmt.Errorf(`models.json has no providers["ai-model-router"].models`)
	}

	const marker = `"ai-model-router"`
	providerPos := strings.Index(text, marker)
	if providerPos == -1 || strings.Contains(text[providerPos+1:], marker) {
		return "", fmt.Errorf(`expected exactly one "ai-model-router" key in models.json`)
	}
	keyPos := strings.Index(text[providerPos:], `"models"`)
	if keyPos == -1 {
		return "", fmt.Errorf(`no "models" key after "ai-model-router"`)
	}
	keyPos += providerPos
	bracketPos := strings.Index(text[keyPos:], "[")
	if bracketPos == -1 {
		return "", fmt.Errorf(`no "[" after "models" key`)
	}
	bracketPos += keyPos
	start, end, err := findValueSpan(text, bracketPos)
	if err != nil {
		return "", err
	}

	// Match the file's style: 2-space indent, the "models" key sits at
	// depth 3 (root > providers > ai-model-router), so array items indent
	// by 8. The opening bracket stays on the key line; the short flat
	// "input" arrays stay inline, like the hand-written entries.
	if newModels == nil {
		newModels = []router.Entry{}
	}
	rendered, err := marshalIndent(newModels)
	if err != nil {
		return "", err
	}
	rendered = inputArrayRe.ReplaceAllStringFunc(rendered, func(m string) string {
		inner := inputArrayRe.FindStringSubmatch(m)[1]
		return `"input": [` + commaSpaceRe.ReplaceAllString(inner, ", ") + `]`
	})
	lines := strings.Split(rendered, "\n")
	var indented strings.Builder
	indented.WriteString(lines[0])
	for _, line := range lines[1:] {
		indented.WriteString("\n      " + line)
	}
	spliced := text[:start] + indented.String() + text[end:]

	// Verify: re-parse and confirm everything outside the models array is
	// identical.
	var check map[string]any
	if err := json.Unmarshal([]byte(spliced), &check); err != nil {
		return "", fmt.Errorf("splice produced invalid JSON: %w", err)
	}
	checkProviders, ok := check["providers"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("splice removed providers")
	}
	checkRouter, ok := checkProviders["ai-model-router"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("splice removed router provider")
	}
	checkRouter["models"] = originalModels
	if !reflect.DeepEqual(check, original) {
		return "", fmt.Errorf("splice changed something outside the router models array")
	}
	return spliced, nil
}

// RenderModelsJSON regenerates the router section. newModels is sorted by
// id for deterministic, near-empty diffs on unchanged runs.
func RenderModelsJSON(text string, newModels []router.Entry) (string, error) {
	sorted := make([]router.Entry, len(newModels))
	copy(sorted, newModels)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	return SpliceRouterModels(text, sorted)
}

// WriteModelsJSON regenerates and atomically writes the router section.
func WriteModelsJSON(path string, newModels []router.Entry) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text, err := RenderModelsJSON(string(data), newModels)
	if err != nil {
		return err
	}
	return AtomicWrite(path, text)
}
