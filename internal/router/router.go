// Package router reads deployed model IDs and metadata from the AI Model
// Router. A malformed response is not an empty deployment: it is an
// incomplete source, and sync must leave Pi's model list alone.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/luiul/orchard/internal/paths"
)

// requestTimeout bounds each gateway call.
const requestTimeout = 10 * time.Second

// sourceRank orders the fallback chain; the reported source for a
// multi-part field (cost) is the lowest-priority one used.
var sourceRank = map[string]int{"router": 0, "bundled": 1, "override": 2, "default": 3}

// Registry is the hand-maintained model registry stowed in ~/dotfiles.
type Registry struct {
	ProbeRegionOverrides map[string]string
	RouterModelOverrides map[string]map[string]any
}

// LoadRegistry reads the registry. A missing file is fine (empty registry);
// invalid JSON is a hard error — a broken hand-maintained file should be
// fixed, not silently ignored.
func LoadRegistry(path string) (Registry, error) {
	empty := Registry{
		ProbeRegionOverrides: map[string]string{},
		RouterModelOverrides: map[string]map[string]any{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, nil
		}
		return empty, err
	}
	var raw struct {
		ProbeRegionOverrides map[string]string         `json:"probeRegionOverrides"`
		RouterModelOverrides map[string]map[string]any `json:"routerModelOverrides"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return empty, err
	}
	reg := empty
	if raw.ProbeRegionOverrides != nil {
		reg.ProbeRegionOverrides = raw.ProbeRegionOverrides
	}
	if raw.RouterModelOverrides != nil {
		reg.RouterModelOverrides = raw.RouterModelOverrides
	}
	return reg, nil
}

// NormalizeModelID lowercases and strips the org prefix:
// "zai-org/GLM-5.3-Flash" -> "glm-5.3-flash".
func NormalizeModelID(modelID string) string {
	if i := strings.LastIndex(modelID, "/"); i >= 0 {
		modelID = modelID[i+1:]
	}
	return strings.ToLower(modelID)
}

// humanize turns "deepseek-ai/DeepSeek-V4.1-Flash" into "Deepseek V4 1
// Flash" (Python str.title() semantics: first letter of each word upper,
// rest lower).
func humanize(modelID string) string {
	spaced := strings.NewReplacer("-", " ", ".", " ").Replace(NormalizeModelID(modelID))
	words := strings.Fields(spaced)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

// perMillion converts a per-token cost to a per-million-token cost.
func perMillion(perToken *float64) *float64 {
	if perToken == nil {
		return nil
	}
	v := math.Round(*perToken*1_000_000*1e6) / 1e6
	return &v
}

// nonzeroFirst uses the first nonzero number, or falls back to b.
func nonzeroFirst(a, b *float64) *float64 {
	if a != nil && *a != 0 {
		return a
	}
	return b
}

// --- untyped JSON helpers (the gateway payloads are map-shaped) ---

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func asFloat(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case int64:
		f := float64(n)
		return &f
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return &f
		}
	}
	return nil
}

func asBool(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

// The pick() chain needs genuine nil interfaces for "absent": a typed nil
// pointer or slice wrapped in `any` is NOT nil. These converters unwrap.

// asAny converts a decoded JSON value to a plain value (nil when absent or
// wrong type), for chain candidates straight out of a payload map.
func asAny(v any) any {
	switch v.(type) {
	case string, bool, float64:
		return v
	}
	return nil
}

// ptrAny unwraps a pointer to a plain value (nil interface when absent).
func ptrAny[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// sliceAny converts a []string to a plain value (nil interface when absent).
func sliceAny(s []string) any {
	if s == nil {
		return nil
	}
	return s
}

// asStringSliceAny validates a decoded JSON array of strings for the chain.
func asStringSliceAny(v any) any {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// --- pi's bundled provider data ---

// bundledModel is one entry of pi-ai's bundled per-provider model data.
// Pointers track field presence for the fallback chain.
type bundledModel struct {
	Name      *string  `json:"name"`
	Reasoning *bool    `json:"reasoning"`
	Input     []string `json:"input"`
	Cost      *struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cacheRead"`
		CacheWrite *float64 `json:"cacheWrite"`
	} `json:"cost"`
	ContextWindow *float64 `json:"contextWindow"`
	MaxTokens     *float64 `json:"maxTokens"`
}

// lookPath is a seam so tests can hide pi.
var lookPath = exec.LookPath

// FindBundledDataDir locates pi-ai's bundled per-provider model data inside
// the pi install. `pi` resolves to the pi-coding-agent package; pi-ai sits
// in its node_modules. Override with PI_PROVIDER_DATA_DIR (used by tests).
func FindBundledDataDir() string {
	if override := os.Getenv("PI_PROVIDER_DATA_DIR"); override != "" {
		if info, err := os.Stat(override); err == nil && info.IsDir() {
			return override
		}
		return ""
	}
	piBin, err := lookPath("pi")
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(piBin); err == nil {
		piBin = resolved
	}
	for dir := filepath.Dir(piBin); ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "node_modules", "@earendil-works", "pi-ai", "dist", "providers", "data")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}

// LoadBundledModels indexes bundled Pi provider metadata by model ID.
// Missing or invalid entries are ignored; the gateway and registry still
// supply the fields needed for the generated router definition.
func LoadBundledModels(dataDir string) map[string]bundledModel {
	index := map[string]bundledModel{}
	if dataDir == "" {
		return index
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return index
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dataDir, entry.Name()))
		if err != nil {
			continue
		}
		var top map[string]any
		if err := json.Unmarshal(data, &top); err != nil {
			continue
		}
		for _, groupV := range top {
			group, ok := groupV.(map[string]any)
			if !ok {
				continue
			}
			for _, entryV := range group {
				m, ok := entryV.(map[string]any)
				if !ok {
					continue
				}
				id, ok := m["id"].(string)
				if !ok {
					continue
				}
				raw, err := json.Marshal(m)
				if err != nil {
					continue
				}
				var bm bundledModel
				if err := json.Unmarshal(raw, &bm); err != nil {
					continue
				}
				norm := NormalizeModelID(id)
				if _, exists := index[norm]; !exists {
					index[norm] = bm
				}
			}
		}
	}
	return index
}

// --- the generated entry ---

// Cost is the per-1M-token cost block of a models.json entry.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// Entry is one generated models.json router entry. The field order fixes
// the rendered key order (Python dicts render in insertion order).
type Entry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Reasoning     bool     `json:"reasoning"`
	Input         []string `json:"input"`
	Cost          Cost     `json:"cost"`
	ContextWindow int64    `json:"contextWindow"`
	MaxTokens     int64    `json:"maxTokens"`
}

// ModelEntry is a generated router entry plus per-field provenance.
type ModelEntry struct {
	ID      string
	Entry   Entry
	Sources map[string]string // field -> "router" | "bundled" | "override" | "default"
}

// Deployment is the live router state: deployed ids and generated entries.
type Deployment struct {
	Reachable bool
	Deployed  []string
	Entries   map[string]ModelEntry
	Err       string
}

// candidate is one (source, value) option in the fallback chain; a nil
// value means the source does not have the field.
type candidate struct {
	source string
	value  any
}

// pick walks the chain: the first candidate with a non-nil value wins and
// records its source. Everything absent records "default".
func pick(sources map[string]string, field string, candidates ...candidate) any {
	for _, c := range candidates {
		if c.value != nil {
			sources[field] = c.source
			return c.value
		}
	}
	sources[field] = "default"
	return nil
}

func toInt64(v any) int64 {
	if n, ok := v.(float64); ok {
		return int64(n)
	}
	return 0
}

func toBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// buildEntry assembles one pi router entry, walking the fallback chain per
// field. modelsItem and infoItem are the raw gateway payloads (infoItem may
// be nil); overrides come from the registry.
func buildEntry(modelID string, modelsItem, infoItem map[string]any, bundled map[string]bundledModel, overrides map[string]map[string]any) ModelEntry {
	modelInfo := asMap(infoItem["model_info"])
	params := asMap(infoItem["litellm_params"])
	bundledEntry, hasBundled := bundled[NormalizeModelID(modelID)]
	override, hasOverride := overrides[modelID]
	if !hasOverride {
		override = overrides[NormalizeModelID(modelID)]
	}

	var overrideCost map[string]any
	var overrideName, overrideReasoning, overrideInput, overrideCtx, overrideMax any
	if override != nil {
		overrideCost = asMap(override["cost"])
		overrideName = asAny(override["name"])
		overrideReasoning = asAny(override["reasoning"])
		overrideInput = asStringSliceAny(override["input"])
		overrideCtx = asAny(override["contextWindow"])
		overrideMax = asAny(override["maxTokens"])
	}

	sources := map[string]string{}

	// Cost: per key override, then router, then bundled metadata, then zero.
	routerCosts := map[string]*float64{
		"input":      perMillion(nonzeroFirst(asFloat(modelInfo["input_cost_per_token"]), asFloat(params["input_cost_per_token"]))),
		"output":     perMillion(nonzeroFirst(asFloat(modelInfo["output_cost_per_token"]), asFloat(params["output_cost_per_token"]))),
		"cacheRead":  perMillion(asFloat(modelInfo["cache_read_input_token_cost"])),
		"cacheWrite": perMillion(asFloat(modelInfo["cache_creation_input_token_cost"])),
	}
	bundledCosts := map[string]*float64{}
	if hasBundled && bundledEntry.Cost != nil {
		bundledCosts["input"] = bundledEntry.Cost.Input
		bundledCosts["output"] = bundledEntry.Cost.Output
		bundledCosts["cacheRead"] = bundledEntry.Cost.CacheRead
		bundledCosts["cacheWrite"] = bundledEntry.Cost.CacheWrite
	}
	cost := Cost{}
	costPtrs := map[string]*float64{"input": &cost.Input, "output": &cost.Output, "cacheRead": &cost.CacheRead, "cacheWrite": &cost.CacheWrite}
	costSource := "default"
	for _, key := range []string{"input", "output", "cacheRead", "cacheWrite"} {
		var source string
		switch v := asFloat(overrideCost[key]); {
		case v != nil:
			*costPtrs[key], source = *v, "override"
		case routerCosts[key] != nil:
			*costPtrs[key], source = *routerCosts[key], "router"
		case bundledCosts[key] != nil:
			*costPtrs[key], source = *bundledCosts[key], "bundled"
		default:
			*costPtrs[key], source = 0, "default"
		}
		if sourceRank[source] < sourceRank[costSource] {
			costSource = source
		}
	}
	sources["cost"] = costSource

	// Router-side scalar candidates.
	contextWindow := nonzeroFirst(asFloat(modelInfo["max_input_tokens"]), asFloat(modelsItem["max_input_tokens"]))
	maxTokens := nonzeroFirst(asFloat(modelInfo["max_output_tokens"]), asFloat(modelsItem["max_output_tokens"]))
	var routerInput []string
	if vision := asBool(modelInfo["supports_vision"]); vision != nil {
		if *vision {
			routerInput = []string{"text", "image"}
		} else {
			routerInput = []string{"text"}
		}
	}

	var bundledName, bundledReasoning, bundledInput, bundledCtx, bundledMax any
	if hasBundled {
		bundledName = ptrAny(bundledEntry.Name)
		bundledReasoning = ptrAny(bundledEntry.Reasoning)
		bundledInput = sliceAny(bundledEntry.Input)
		bundledCtx = ptrAny(bundledEntry.ContextWindow)
		bundledMax = ptrAny(bundledEntry.MaxTokens)
	}

	name := pick(sources, "name", candidate{"override", overrideName}, candidate{"bundled", bundledName})
	entry := Entry{
		ID: modelID,
		Name: func() string {
			if s, ok := name.(string); ok {
				return s
			}
			return humanize(modelID)
		}(),
		Reasoning: toBool(pick(sources, "reasoning",
			candidate{"override", overrideReasoning},
			candidate{"router", ptrAny(asBool(modelInfo["supports_reasoning"]))},
			candidate{"bundled", bundledReasoning})),
		Input: func() []string {
			if v, ok := pick(sources, "input",
				candidate{"override", overrideInput},
				candidate{"router", sliceAny(routerInput)},
				candidate{"bundled", bundledInput}).([]string); ok {
				return v
			}
			return []string{"text"}
		}(),
		Cost: cost,
		ContextWindow: toInt64(pick(sources, "contextWindow",
			candidate{"override", overrideCtx},
			candidate{"router", ptrAny(contextWindow)},
			candidate{"bundled", bundledCtx})),
		MaxTokens: toInt64(pick(sources, "maxTokens",
			candidate{"override", overrideMax},
			candidate{"router", ptrAny(maxTokens)},
			candidate{"bundled", bundledMax})),
	}
	return ModelEntry{ID: modelID, Entry: entry, Sources: sources}
}

// Fetch reads the live deployment and generates entries for every deployed
// id. Network/auth failures never error out: they return
// Reachable=false with the error recorded (the caller decides under
// --strict whether that is fatal). The returned error is reserved for hard
// failures: a broken hand-maintained registry. Network requests are made by
// curl, not Orchard, so the installed CLI owns firewall and TLS handling.
func Fetch(ctx context.Context, cfg paths.Config) (Deployment, error) {
	if cfg.RouterAPIKey == "" {
		return Deployment{Err: "AI_MODEL_ROUTER_API_KEY not set (env or ~/dotfiles/.env)"}, nil
	}
	base := strings.TrimRight(cfg.RouterBaseURL, "/")

	get := func(path string) (map[string]any, error) {
		body, err := runCurl(ctx, base+path, cfg.RouterAPIKey)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("GET %s: invalid JSON", path)
		}
		return doc, nil
	}

	modelsDoc, err := get("/models")
	if err != nil {
		return Deployment{Err: err.Error()}, nil
	}
	models, err := dataList(modelsDoc, "/models", "id")
	if err != nil {
		return Deployment{Err: err.Error()}, nil
	}
	infoDoc, err := get("/model/info")
	if err != nil {
		return Deployment{Err: err.Error()}, nil
	}
	info, err := dataList(infoDoc, "/model/info", "model_name")
	if err != nil {
		return Deployment{Err: err.Error()}, nil
	}

	infoByName := map[string]map[string]any{}
	for _, item := range info {
		infoByName[item["model_name"].(string)] = item
	}
	modelsByID := map[string]map[string]any{}
	for _, item := range models {
		modelsByID[item["id"].(string)] = item
	}

	bundled := LoadBundledModels(FindBundledDataDir())
	registry, err := LoadRegistry(cfg.ModelRegistry)
	if err != nil {
		return Deployment{}, fmt.Errorf("model registry: %w", err)
	}

	deployed := make([]string, 0, len(modelsByID))
	for id := range modelsByID {
		deployed = append(deployed, id)
	}
	sort.Strings(deployed)
	entries := make(map[string]ModelEntry, len(deployed))
	for _, id := range deployed {
		entries[id] = buildEntry(id, modelsByID[id], infoByName[id], bundled, registry.RouterModelOverrides)
	}
	return Deployment{Reachable: true, Deployed: deployed, Entries: entries}, nil
}

// dataList validates a gateway list before it can replace the live deployment.
func dataList(doc map[string]any, path, nameKey string) ([]map[string]any, error) {
	items, ok := doc["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("GET %s: data must be an array", path)
	}
	if path == "/models" && len(items) == 0 {
		return nil, fmt.Errorf("GET %s: data must not be empty", path)
	}
	out := make([]map[string]any, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("GET %s: data[%d] must be an object", path, i)
		}
		name, ok := m[nameKey].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("GET %s: data[%d].%s must be a non-empty string", path, i, nameKey)
		}
		if path == "/model/info" {
			for _, key := range []string{"model_info", "litellm_params"} {
				if m[key] != nil && asMap(m[key]) == nil {
					return nil, fmt.Errorf("GET %s: data[%d].%s must be an object or null", path, i, key)
				}
			}
		}
		out = append(out, m)
	}
	return out, nil
}
