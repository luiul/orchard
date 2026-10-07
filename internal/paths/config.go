// Package paths resolves Orchard's model sources, regions, and probe settings.
// The configured regions include EU, US, Japan, and Australia by default.
package paths

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultRouterBaseURL is the HelloFresh ai-model-router's OpenAI-compatible API base.
const DefaultRouterBaseURL = "https://ai-model-router-api.eu.foundations.prod.int.hellofresh.io/v1"

// DefaultRegion is the region pi invokes Bedrock in by default.
const DefaultRegion = "eu-west-1"

// DefaultExtraRegions are the regions scanned in addition to DefaultRegion.
var DefaultExtraRegions = []string{"us-east-1", "ap-northeast-1", "ap-southeast-2"}

// readFile is a package-level variable so tests can swap the filesystem
// out (the convention for every external call in this repo's family).
var readFile = os.ReadFile

// Config is the validated runtime configuration every command consumes.
type Config struct {
	Dotfiles          string
	PISettings        string // dotfiles copy of settings.json
	LiveSettings      string // ~/.pi/agent/settings.json
	BedrockModelsJSON string
	ModelsJSON        string
	ModelRegistry     string
	AWSProfile        string
	DefaultRegion     string
	ExtraRegions      []string
	ProbeTimeout      int // seconds before a probe is killed and counted systemic
	ProbeConcurrency  int // how many probes run at once
	ProbeFailCircuit  int // consecutive systemic failures that trip the circuit breaker
	RouterBaseURL     string
	RouterAPIKey      string // empty when unresolved (router state is unknown)
	ProbeAgentDir     string // temporary Pi agent directory for staged router probes
}

// Regions returns each region checked for Bedrock models.
func (c Config) Regions() []string {
	return append([]string{c.DefaultRegion}, c.ExtraRegions...)
}

// Load reads environment and exports the router key only when it came from
// the gitignored dotfiles .env, so Pi subprocesses can use that key.
func Load() (Config, error) {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	cfg, err := FromEnv(env)
	if err != nil {
		return Config{}, err
	}
	if cfg.RouterAPIKey != "" && env["AI_MODEL_ROUTER_API_KEY"] == "" {
		if err := os.Setenv("AI_MODEL_ROUTER_API_KEY", cfg.RouterAPIKey); err != nil {
			return Config{}, fmt.Errorf("export router key: %w", err)
		}
	}
	return cfg, nil
}

// FromEnv resolves the configuration from env (a KEY -> value map, as
// opposed to the process environment), so tests control every input.
// It has no side effects; see Load for the probe-inheritance export.
func FromEnv(env map[string]string) (Config, error) {
	home := env["HOME"]
	if home == "" {
		if dir, err := os.UserHomeDir(); err == nil {
			home = dir
		}
	}
	expand := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "~/"); ok && home != "" {
			return home + "/" + rest
		}
		return p
	}

	timeout, err := positiveInt(env, "PROBE_TIMEOUT", 45)
	if err != nil {
		return Config{}, err
	}
	concurrency, err := positiveInt(env, "PROBE_CONCURRENCY", 3)
	if err != nil {
		return Config{}, err
	}
	failCircuit, err := positiveInt(env, "PROBE_FAIL_CIRCUIT", 6)
	if err != nil {
		return Config{}, err
	}

	dotfiles := expand(or(env["DOTFILES"], "~/dotfiles"))
	agentDir := dotfiles + "/pi/.pi/agent"

	defaultRegion := or(env["AWS_REGION"], DefaultRegion)
	extraRegions := DefaultExtraRegions
	if raw, exists := env["BEDROCK_REGIONS"]; exists {
		extraRegions = strings.Fields(raw)
	} else {
		filtered := make([]string, 0, len(extraRegions))
		for _, region := range extraRegions {
			if region != defaultRegion {
				filtered = append(filtered, region)
			}
		}
		extraRegions = filtered
	}
	seenRegions := map[string]bool{defaultRegion: true}
	for _, region := range extraRegions {
		if seenRegions[region] {
			return Config{}, fmt.Errorf("BEDROCK_REGIONS contains duplicate region %q", region)
		}
		seenRegions[region] = true
	}

	routerKey := loadAPIKey(env, dotfiles, "AI_MODEL_ROUTER_API_KEY")

	return Config{
		Dotfiles:          dotfiles,
		PISettings:        expand(or(env["PI_SETTINGS"], agentDir+"/settings.json")),
		LiveSettings:      expand("~/.pi/agent/settings.json"),
		BedrockModelsJSON: expand(or(env["BEDROCK_MODELS_JSON"], agentDir+"/bedrock-models.json")),
		ModelsJSON:        expand(or(env["MODELS_JSON"], agentDir+"/models.json")),
		ModelRegistry:     expand(or(env["MODEL_REGISTRY"], agentDir+"/model-registry.json")),
		AWSProfile:        or(env["AWS_PROFILE"], "sso-bedrock"),
		DefaultRegion:     defaultRegion,
		ExtraRegions:      extraRegions,
		ProbeTimeout:      timeout,
		ProbeConcurrency:  concurrency,
		ProbeFailCircuit:  failCircuit,
		RouterBaseURL:     or(env["ROUTER_BASE_URL"], DefaultRouterBaseURL),
		RouterAPIKey:      routerKey,
	}, nil
}

// positiveInt parses env[name] as a strictly positive integer, mirroring
// the bash script's exit-2 contract on a bad value.
func positiveInt(env map[string]string, name string, def int) (int, error) {
	raw := env[name]
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer (got: %s)", name, raw)
	}
	return n, nil
}

// loadAPIKey returns the named key from env, falling back to a KEY=value
// (or export KEY=value) line in <dotfiles>/.env. Empty means unresolved;
// callers degrade instead of failing.
func loadAPIKey(env map[string]string, dotfiles, name string) string {
	if key := env[name]; key != "" {
		return key
	}
	data, err := readFile(dotfiles + "/.env")
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keyPart, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		keyPart = strings.TrimSpace(strings.TrimPrefix(keyPart, "export "))
		if keyPart != name {
			continue
		}
		if key := strings.Trim(strings.TrimSpace(value), `"'`); key != "" {
			return key
		}
	}
	return ""
}

func or(value, def string) string {
	if value != "" {
		return value
	}
	return def
}
