package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/luiul/orchard/internal/model"
)

// Scope uses Pi's own resolver for provider-qualified patterns, fuzzy names,
// thinking pins, and case-insensitive globs. Reimplementing minimatch in Go
// would quietly give Orchard a different model scope from Pi.
type Scope struct {
	Selected map[string]bool
	Matches  map[string][]string
}

const scopeScript = `
import {pathToFileURL} from 'node:url';
const {resolveModelScopeWithDiagnostics, ModelRuntime} = await import(pathToFileURL(process.argv[1]).href);
let input = ''; for await (const chunk of process.stdin) input += chunk;
const {models, patterns, modelsPath} = JSON.parse(input);
const runtime = await ModelRuntime.create({
  modelsPath, authPath: '/dev/null/orchard-auth.json',
  allowModelNetwork: false, refreshOnCreate: false,
});
if (runtime.getError()) throw new Error(runtime.getError());
for (const model of models) {
  model.name = runtime.getModel(model.provider, model.id)?.name ?? model.id;
}
const matches = {};
for (const pattern of patterns) {
  const result = await resolveModelScopeWithDiagnostics([pattern], {getAvailable: async () => models});
  matches[pattern] = result.scopedModels.map(({model}) => model.provider + '\u0000' + model.id);
}
console.log(JSON.stringify(matches));
`

var runScope = func(ctx context.Context, payload []byte) ([]byte, error) {
	bin, err := exec.LookPath("pi")
	if err != nil {
		return nil, err
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		return nil, err
	}
	for dir := filepath.Dir(bin); ; dir = filepath.Dir(dir) {
		data, readErr := os.ReadFile(filepath.Join(dir, "package.json"))
		var pkg struct {
			Name string `json:"name"`
		}
		if readErr == nil && json.Unmarshal(data, &pkg) == nil && pkg.Name == "@earendil-works/pi-coding-agent" {
			cmd := exec.CommandContext(ctx, "node", "--input-type=module", "-e", scopeScript, filepath.Join(dir, "dist", "index.js"))
			cmd.Stdin = bytes.NewReader(payload)
			return cmd.Output()
		}
		if filepath.Dir(dir) == dir {
			return nil, fmt.Errorf("cannot locate Pi's model scope resolver")
		}
	}
}

func ResolveScope(ctx context.Context, models []model.CatalogEntry, patterns []string, modelsPath string) (Scope, error) {
	result := Scope{Selected: map[string]bool{}, Matches: map[string][]string{}}
	if len(patterns) == 0 {
		for _, entry := range models {
			result.Selected[entry.Provider+"\x00"+entry.ID] = true
		}
		return result, nil
	}
	type modelRef struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
		Name     string `json:"name"`
	}
	refs := make([]modelRef, 0, len(models))
	for _, model := range models {
		refs = append(refs, modelRef{model.Provider, model.ID, model.ID})
	}
	payload, err := json.Marshal(struct {
		Models     []modelRef `json:"models"`
		Patterns   []string   `json:"patterns"`
		ModelsPath string     `json:"modelsPath"`
	}{refs, patterns, modelsPath})
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := runScope(ctx, payload)
	if err != nil {
		return result, fmt.Errorf("resolve Pi model scope: %w", err)
	}
	if err := json.Unmarshal(out, &result.Matches); err != nil {
		return result, fmt.Errorf("parse Pi model scope: %w", err)
	}
	known := make(map[string]bool, len(models))
	for _, model := range models {
		known[model.Provider+"\x00"+model.ID] = true
	}
	for _, pattern := range patterns {
		ids, ok := result.Matches[pattern]
		if !ok {
			return result, fmt.Errorf("pi scope resolver omitted pattern %q", pattern)
		}
		for _, id := range ids {
			if !known[id] {
				return result, fmt.Errorf("pi scope resolver returned a model outside the catalog")
			}
			result.Selected[id] = true
		}
	}
	// Pi cycles all available models when the configured scope is empty or
	// none of its patterns resolve. Keep empty per-pattern matches for drift.
	if len(result.Selected) == 0 {
		for key := range known {
			result.Selected[key] = true
		}
	}
	return result, nil
}
