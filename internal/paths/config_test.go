package paths

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// stubEnv is a clean, fully controlled environment: HOME set so path
// expansion is deterministic, nothing else.
func stubEnv() map[string]string {
	return map[string]string{"HOME": "/home/test"}
}

func TestFromEnvDefaults(t *testing.T) {
	cfg, err := FromEnv(stubEnv())
	if err != nil {
		t.Fatalf("FromEnv(defaults) error: %v", err)
	}
	want := Config{
		Dotfiles:          "/home/test/dotfiles",
		PISettings:        "/home/test/dotfiles/pi/.pi/agent/settings.json",
		LiveSettings:      "/home/test/.pi/agent/settings.json",
		BedrockModelsJSON: "/home/test/dotfiles/pi/.pi/agent/bedrock-models.json",
		ModelsJSON:        "/home/test/dotfiles/pi/.pi/agent/models.json",
		ModelRegistry:     "/home/test/dotfiles/pi/.pi/agent/model-registry.json",
		AWSProfile:        "sso-bedrock",
		DefaultRegion:     "eu-west-1",
		ExtraRegions:      []string{"us-east-1", "ap-northeast-1", "ap-southeast-2"},
		ProbeTimeout:      45,
		ProbeConcurrency:  3,
		ProbeFailCircuit:  6,
		RouterBaseURL:     DefaultRouterBaseURL,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("FromEnv(defaults):\n got %+v\nwant %+v", cfg, want)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	env := stubEnv()
	env["DOTFILES"] = "/custom/dotfiles"
	env["PI_SETTINGS"] = "~/elsewhere/settings.json"
	env["AWS_PROFILE"] = "other-profile"
	env["AWS_REGION"] = "us-west-2"
	env["BEDROCK_REGIONS"] = "  eu-central-1   us-east-1 "
	env["PROBE_TIMEOUT"] = "90"
	env["PROBE_CONCURRENCY"] = "5"
	env["PROBE_FAIL_CIRCUIT"] = "2"
	env["ROUTER_BASE_URL"] = "https://example.test/v2"
	env["AI_MODEL_ROUTER_API_KEY"] = "router-key"

	cfg, err := FromEnv(env)
	if err != nil {
		t.Fatalf("FromEnv(overrides) error: %v", err)
	}
	if cfg.Dotfiles != "/custom/dotfiles" {
		t.Errorf("Dotfiles = %q", cfg.Dotfiles)
	}
	if cfg.PISettings != "/home/test/elsewhere/settings.json" {
		t.Errorf("PISettings = %q (tilde not expanded)", cfg.PISettings)
	}
	// Unset path vars derive from the DOTFILES override.
	if cfg.ModelsJSON != "/custom/dotfiles/pi/.pi/agent/models.json" {
		t.Errorf("ModelsJSON = %q", cfg.ModelsJSON)
	}
	if cfg.AWSProfile != "other-profile" || cfg.DefaultRegion != "us-west-2" {
		t.Errorf("AWS config = %q / %q", cfg.AWSProfile, cfg.DefaultRegion)
	}
	if got := strings.Join(cfg.ExtraRegions, " "); got != "eu-central-1 us-east-1" {
		t.Errorf("ExtraRegions = %q", got)
	}
	if cfg.ProbeTimeout != 90 || cfg.ProbeConcurrency != 5 || cfg.ProbeFailCircuit != 2 {
		t.Errorf("probe tuning = %d/%d/%d", cfg.ProbeTimeout, cfg.ProbeConcurrency, cfg.ProbeFailCircuit)
	}
	if cfg.RouterBaseURL != "https://example.test/v2" {
		t.Errorf("RouterBaseURL = %q", cfg.RouterBaseURL)
	}
	if cfg.RouterAPIKey != "router-key" {
		t.Error("router key override not applied")
	}
}

func TestFromEnvRegionsOrder(t *testing.T) {
	cfg, err := FromEnv(stubEnv())
	if err != nil {
		t.Fatalf("FromEnv error: %v", err)
	}
	got := cfg.Regions()
	want := []string{"eu-west-1", "us-east-1", "ap-northeast-1", "ap-southeast-2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Regions() = %v, want %v", got, want)
	}
}

func TestFromEnvRejectsDuplicateRegions(t *testing.T) {
	env := stubEnv()
	env["BEDROCK_REGIONS"] = "us-east-1 us-east-1"
	if _, err := FromEnv(env); err == nil {
		t.Fatal("duplicate region accepted")
	}
}

func TestFromEnvChangedDefaultIsScannedOnce(t *testing.T) {
	env := stubEnv()
	env["AWS_REGION"] = "us-east-1"
	cfg, err := FromEnv(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Regions(); len(got) != 3 || got[0] != "us-east-1" {
		t.Errorf("regions: %v", got)
	}
}

func TestFromEnvCanScanOnlyTheDefaultRegion(t *testing.T) {
	env := stubEnv()
	env["BEDROCK_REGIONS"] = ""
	cfg, err := FromEnv(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Regions(); len(got) != 1 || got[0] != "eu-west-1" {
		t.Errorf("regions: %v", got)
	}
}

func TestFromEnvRejectsBadInts(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-3", "4.5", "45s"} {
		env := stubEnv()
		env["PROBE_TIMEOUT"] = raw
		_, err := FromEnv(env)
		if err == nil {
			t.Fatalf("PROBE_TIMEOUT=%q: expected error, got none", raw)
		}
		if !strings.Contains(err.Error(), "PROBE_TIMEOUT must be a positive integer") {
			t.Fatalf("PROBE_TIMEOUT=%q: error = %v", raw, err)
		}
	}
}

// swapReadFile replaces the package-level readFile for one test.
func swapReadFile(t *testing.T, content string, err error) {
	t.Helper()
	old := readFile
	t.Cleanup(func() { readFile = old })
	readFile = func(string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return []byte(content), nil
	}
}

func TestAPIKeyFallsBackToDotEnv(t *testing.T) {
	swapReadFile(t, "# comment\n\nexport AI_MODEL_ROUTER_API_KEY=\"from-env-file\"\n", nil)
	cfg, err := FromEnv(stubEnv())
	if err != nil {
		t.Fatalf("FromEnv error: %v", err)
	}
	if cfg.RouterAPIKey != "from-env-file" {
		t.Errorf("RouterAPIKey = %q, want .env fallback", cfg.RouterAPIKey)
	}
}

func TestAPIKeyEnvWinsOverDotEnv(t *testing.T) {
	swapReadFile(t, "AI_MODEL_ROUTER_API_KEY=env-file\n", nil)
	env := stubEnv()
	env["AI_MODEL_ROUTER_API_KEY"] = "real-env"
	cfg, err := FromEnv(env)
	if err != nil {
		t.Fatalf("FromEnv error: %v", err)
	}
	if cfg.RouterAPIKey != "real-env" {
		t.Errorf("RouterAPIKey = %q, want env to win over .env", cfg.RouterAPIKey)
	}
}

func TestAPIKeyMissingDotEnvIsNotAnError(t *testing.T) {
	swapReadFile(t, "", errors.New("no such file"))
	cfg, err := FromEnv(stubEnv())
	if err != nil {
		t.Fatalf("FromEnv error: %v", err)
	}
	if cfg.RouterAPIKey != "" {
		t.Error("expected no router key")
	}
}
