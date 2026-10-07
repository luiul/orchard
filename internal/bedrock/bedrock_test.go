package bedrock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/progress"
)

var fixtureRegions = []string{"eu-west-1", "us-east-1", "ap-northeast-1", "ap-southeast-2"}

func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "tests", "fixtures")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("fixtures dir: %v", err)
	}
	return dir
}

func loadFixture(t *testing.T, kind, region string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir(t), "bedrock-"+kind+"-"+region+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loadProfiles(t *testing.T, region string) []ProfileSummary {
	t.Helper()
	profiles, err := parseInferenceProfiles(loadFixture(t, "inference-profiles", region))
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}

func loadFoundations(t *testing.T, region string) []ModelSummary {
	t.Helper()
	models, err := parseFoundationModels(loadFixture(t, "foundation-models", region))
	if err != nil {
		t.Fatal(err)
	}
	return models
}

func TestInferenceProfilesFromFixture(t *testing.T) {
	profiles := loadProfiles(t, "eu-west-1")
	ids := InferenceProfileIDs(profiles)
	if len(ids) != len(profiles) {
		t.Errorf("got %d ids from %d summaries", len(ids), len(profiles))
	}
	found := false
	for id := range ids {
		if strings.HasPrefix(id, "eu.anthropic.") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no eu.anthropic.* profile id in eu-west-1 fixture")
	}
}

func TestOnDemandFromFixtureFiltersInferenceTypes(t *testing.T) {
	foundations := loadFoundations(t, "us-east-1")
	ids := OnDemandModelIDs(foundations)
	if len(ids) == 0 {
		t.Fatal("us-east-1 fixture has ON_DEMAND models")
	}
	for _, s := range foundations {
		onDemand := false
		for _, typ := range s.InferenceTypes {
			if typ == "ON_DEMAND" {
				onDemand = true
			}
		}
		if !onDemand && ids[s.ID] {
			t.Errorf("%s is not ON_DEMAND but was included", s.ID)
		}
	}
}

func TestEveryPrefixGroupIsRepresentedInFixtures(t *testing.T) {
	all := map[string]bool{}
	for _, region := range fixtureRegions {
		for id := range InferenceProfileIDs(loadProfiles(t, region)) {
			all[id] = true
		}
	}
	prefixes := map[string]bool{}
	for id := range all {
		if before, _, ok := strings.Cut(id, "."); ok {
			prefixes[before] = true
		}
	}
	for _, want := range []string{"eu", "us", "global", "jp", "au", "apac"} {
		if !prefixes[want] {
			t.Errorf("prefix group %q missing from fixtures", want)
		}
	}
}

func TestAssignCandidatesPrefersDefaultRegion(t *testing.T) {
	listed := map[string]map[string]bool{
		"eu-west-1": {"a": true, "b": true},
		"us-east-1": {"a": true, "c": true},
	}
	assigned := AssignCandidates(listed, map[string]bool{"a": true, "b": true, "c": true},
		[]string{"eu-west-1", "us-east-1"}, "eu-west-1", nil)
	want := map[string]string{"a": "eu-west-1", "b": "eu-west-1", "c": "us-east-1"}
	if len(assigned) != len(want) {
		t.Fatalf("got %v, want %v", assigned, want)
	}
	for id, region := range want {
		if assigned[id] != region {
			t.Errorf("%s: got %q, want %q", id, assigned[id], region)
		}
	}
}

func TestAssignCandidatesUsesPrefixHomeRegion(t *testing.T) {
	listed := map[string]map[string]bool{
		"us-east-1":      {"us.anthropic.x": true},
		"ap-northeast-1": {"jp.anthropic.y": true},
	}
	assigned := AssignCandidates(listed,
		map[string]bool{"us.anthropic.x": true, "jp.anthropic.y": true},
		[]string{"eu-west-1", "us-east-1", "ap-northeast-1"}, "eu-west-1", nil)
	if assigned["us.anthropic.x"] != "us-east-1" {
		t.Errorf("us.anthropic.x: got %q", assigned["us.anthropic.x"])
	}
	if assigned["jp.anthropic.y"] != "ap-northeast-1" {
		t.Errorf("jp.anthropic.y: got %q", assigned["jp.anthropic.y"])
	}
}

func TestAssignCandidatesRegistryOverrideWins(t *testing.T) {
	listed := map[string]map[string]bool{
		"eu-west-1": {"global.openai.gpt-5.6-sol": true},
		"us-east-1": {"global.openai.gpt-5.6-sol": true},
	}
	assigned := AssignCandidates(listed, map[string]bool{"global.openai.gpt-5.6-sol": true},
		[]string{"eu-west-1", "us-east-1"}, "eu-west-1",
		map[string]string{"global.openai.gpt-5.6-sol": "us-east-1"})
	if assigned["global.openai.gpt-5.6-sol"] != "us-east-1" {
		t.Errorf("got %q", assigned["global.openai.gpt-5.6-sol"])
	}
}

func TestAssignCandidatesIntersectsWithCatalog(t *testing.T) {
	listed := map[string]map[string]bool{
		"eu-west-1": {"known": true, "unknown-to-pi": true},
	}
	assigned := AssignCandidates(listed, map[string]bool{"known": true},
		[]string{"eu-west-1"}, "eu-west-1", nil)
	if len(assigned) != 1 || assigned["known"] != "eu-west-1" {
		t.Errorf("got %v", assigned)
	}
}

// withAWS swaps the only AWS process seam for the duration of one test.
func withAWS(t *testing.T, run func(context.Context, string, string, ...string) ([]byte, error)) {
	t.Helper()
	old := runAWS
	runAWS = run
	t.Cleanup(func() { runAWS = old })
}

const callerIdentityJSON = `{"UserId":"test-user","Account":"123456789012","Arn":"arn:aws:sts::123456789012:assumed-role/test/user"}`

func TestSSOFailurePrintsFixCommand(t *testing.T) {
	withAWS(t, func(context.Context, string, string, ...string) ([]byte, error) {
		return nil, errors.New("token expired")
	})
	cfg := paths.Config{AWSProfile: "sso-bedrock", DefaultRegion: "eu-west-1"}
	err := CheckSSO(context.Background(), cfg)
	var ssoErr *SsoInvalidError
	if !errors.As(err, &ssoErr) {
		t.Fatalf("got %v, want SsoInvalidError", err)
	}
	if !strings.Contains(err.Error(), "aws sso login --profile sso-bedrock") {
		t.Errorf("error lacks the fix command: %v", err)
	}
}

func TestSSOCheckRunsOnce(t *testing.T) {
	calls := 0
	cfg := paths.Config{AWSProfile: "sso-bedrock", DefaultRegion: "eu-west-1"}
	withAWS(t, func(_ context.Context, profile, region string, args ...string) ([]byte, error) {
		calls++
		if profile != cfg.AWSProfile || region != cfg.DefaultRegion || !reflect.DeepEqual(args, []string{"sts", "get-caller-identity"}) {
			t.Errorf("unexpected AWS call: %q %q %v", profile, region, args)
		}
		return []byte(callerIdentityJSON), nil
	})
	if err := CheckSSO(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("get-caller-identity ran %d times, want 1", calls)
	}
}

func TestSSORejectsInvalidIdentityResponse(t *testing.T) {
	for _, data := range []string{
		`not JSON`, `null`, `{}`, `[]`,
		`{"UserId":"user","Account":"123456789012"}`,
		`{"UserId":null,"Account":"123456789012","Arn":"arn"}`,
		`{"UserId":"user","Account":" ","Arn":"arn"}`,
		`{"UserId":"user","Account":123,"Arn":"arn"}`,
	} {
		t.Run(data, func(t *testing.T) {
			withAWS(t, func(context.Context, string, string, ...string) ([]byte, error) {
				return []byte(data), nil
			})
			err := CheckSSO(context.Background(), paths.Config{AWSProfile: "sso-bedrock", DefaultRegion: "eu-west-1"})
			var ssoErr *SsoInvalidError
			if !errors.As(err, &ssoErr) {
				t.Fatalf("invalid identity accepted: %v", err)
			}
		})
	}
}

func TestProfileResponseValidation(t *testing.T) {
	for _, data := range []string{
		`not JSON`, `null`, `{}`, `[]`, `{"inferenceProfileSummaries":null}`,
		`{"inferenceProfileSummaries":{}}`, `{"inferenceProfileSummaries":[null]}`,
		`{"inferenceProfileSummaries":[{}]}`,
		`{"inferenceProfileSummaries":[{"inferenceProfileId":""}]}`,
		`{"inferenceProfileSummaries":[{"inferenceProfileId":" "}]}`,
		`{"inferenceProfileSummaries":[{"inferenceProfileId":123}]}`,
		`{"inferenceProfileSummaries":[{"inferenceProfileId":"good"},{}]}`,
		`{"inferenceProfileSummaries":[]} {}`,
	} {
		t.Run(data, func(t *testing.T) {
			profiles, err := parseInferenceProfiles([]byte(data))
			if err == nil || profiles != nil {
				t.Fatalf("invalid response accepted: %v, %v", profiles, err)
			}
		})
	}
}

func TestFoundationResponseValidation(t *testing.T) {
	for _, data := range []string{
		`not JSON`, `null`, `{}`, `[]`, `{"modelSummaries":null}`,
		`{"modelSummaries":{}}`, `{"modelSummaries":[null]}`, `{"modelSummaries":[{}]}`,
		`{"modelSummaries":[{"modelId":"","inferenceTypesSupported":[]}]}`,
		`{"modelSummaries":[{"modelId":" ","inferenceTypesSupported":[]}]}`,
		`{"modelSummaries":[{"modelId":123,"inferenceTypesSupported":[]}]}`,
		`{"modelSummaries":[{"modelId":"model"}]}`,
		`{"modelSummaries":[{"modelId":"model","inferenceTypesSupported":null}]}`,
		`{"modelSummaries":[{"modelId":"model","inferenceTypesSupported":"ON_DEMAND"}]}`,
		`{"modelSummaries":[{"modelId":"model","inferenceTypesSupported":[""]}]}`,
		`{"modelSummaries":[{"modelId":"model","inferenceTypesSupported":[null]}]}`,
		`{"modelSummaries":[{"modelId":"model","inferenceTypesSupported":[123]}]}`,
		`{"modelSummaries":[{"modelId":"good","inferenceTypesSupported":["ON_DEMAND"]},{}]}`,
	} {
		t.Run(data, func(t *testing.T) {
			models, err := parseFoundationModels([]byte(data))
			if err == nil || models != nil {
				t.Fatalf("invalid response accepted: %v, %v", models, err)
			}
		})
	}
}

func TestEmptyListsAreValid(t *testing.T) {
	profiles, err := parseInferenceProfiles([]byte(`{"inferenceProfileSummaries":[]}`))
	if err != nil || profiles == nil || len(profiles) != 0 {
		t.Fatalf("empty profiles: %v, %v", profiles, err)
	}
	models, err := parseFoundationModels([]byte(`{"modelSummaries":[]}`))
	if err != nil || models == nil || len(models) != 0 {
		t.Fatalf("empty foundations: %v, %v", models, err)
	}
	// A model can have no supported inference types without breaking the scan.
	models, err = parseFoundationModels([]byte(`{"modelSummaries":[{"modelId":"model","inferenceTypesSupported":[]}]}`))
	if err != nil || len(OnDemandModelIDs(models)) != 0 {
		t.Fatalf("empty inference types: %v, %v", models, err)
	}
	withAWS(t, func(_ context.Context, _, _ string, args ...string) ([]byte, error) {
		if args[1] == "list-inference-profiles" {
			return []byte(`{"inferenceProfileSummaries":[]}`), nil
		}
		return []byte(`{"modelSummaries":[]}`), nil
	})
	result := FetchListings(context.Background(), paths.Config{AWSProfile: "sso-bedrock", DefaultRegion: "eu-west-1"})
	if len(result.Degraded) != 0 || len(result.ByRegion) != 1 || len(result.ByRegion["eu-west-1"]) != 0 {
		t.Fatalf("valid empty scan was degraded: %+v", result)
	}
}

func TestFetchListingsFromFixtures(t *testing.T) {
	cfg := paths.Config{AWSProfile: "sso-bedrock", DefaultRegion: fixtureRegions[0], ExtraRegions: fixtureRegions[1:]}
	var calls []string
	withAWS(t, func(_ context.Context, profile, region string, args ...string) ([]byte, error) {
		if profile != cfg.AWSProfile || len(args) != 2 || args[0] != "bedrock" {
			t.Fatalf("unexpected AWS call: %q %q %v", profile, region, args)
		}
		calls = append(calls, region+":"+args[1])
		switch args[1] {
		case "list-inference-profiles":
			return loadFixture(t, "inference-profiles", region), nil
		case "list-foundation-models":
			return loadFixture(t, "foundation-models", region), nil
		default:
			t.Fatalf("unexpected operation %q", args[1])
			return nil, nil
		}
	})
	var messages []string
	ctx := progress.WithReporter(context.Background(), func(message string) { messages = append(messages, message) })
	result := FetchListings(ctx, cfg)
	if len(result.Degraded) != 0 || len(result.ByRegion) != len(fixtureRegions) {
		t.Fatalf("unexpected scan: %+v", result)
	}
	for i, region := range fixtureRegions {
		want := InferenceProfileIDs(loadProfiles(t, region))
		for id := range OnDemandModelIDs(loadFoundations(t, region)) {
			want[id] = true
		}
		if !reflect.DeepEqual(result.ByRegion[region], want) {
			t.Errorf("%s: got %d models, want %d", region, len(result.ByRegion[region]), len(want))
		}
		wantCalls := []string{region + ":list-inference-profiles", region + ":list-foundation-models"}
		if !reflect.DeepEqual(calls[i*2:i*2+2], wantCalls) {
			t.Errorf("%s calls: %v", region, calls[i*2:i*2+2])
		}
		wantMessages := []string{
			"fetching Bedrock models in " + region,
			fmt.Sprintf("Bedrock %s: %d listed models", region, len(want)),
		}
		if !reflect.DeepEqual(messages[i*2:i*2+2], wantMessages) {
			t.Errorf("%s progress: %v", region, messages[i*2:i*2+2])
		}
	}
}

func TestFetchListingsDegradesFailedRegionWithoutPartialIDs(t *testing.T) {
	for _, tc := range []struct {
		name, failedOperation string
		data                  []byte
		err                   error
	}{
		{"profile failure", "list-inference-profiles", nil, errors.New("AccessDeniedException")},
		{"foundation failure", "list-foundation-models", nil, errors.New("AccessDeniedException")},
		{"bad profiles", "list-inference-profiles", []byte(`{"inferenceProfileSummaries":[{}]}`), nil},
		{"bad foundations", "list-foundation-models", []byte(`{}`), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withAWS(t, func(_ context.Context, _, region string, args ...string) ([]byte, error) {
				if region == "eu-west-1" && args[1] == tc.failedOperation {
					return tc.data, tc.err
				}
				if args[1] == "list-inference-profiles" {
					return loadFixture(t, "inference-profiles", region), nil
				}
				return loadFixture(t, "foundation-models", region), nil
			})
			cfg := paths.Config{AWSProfile: "sso-bedrock", DefaultRegion: "eu-west-1", ExtraRegions: []string{"us-east-1"}}
			var messages []string
			ctx := progress.WithReporter(context.Background(), func(message string) { messages = append(messages, message) })
			result := FetchListings(ctx, cfg)
			if len(result.Degraded) != 1 || result.Degraded["eu-west-1"] == "" {
				t.Fatalf("failed region not degraded: %+v", result)
			}
			if _, ok := result.ByRegion["eu-west-1"]; ok || len(result.ByRegion["us-east-1"]) == 0 {
				t.Fatalf("partial scan accepted or next region skipped: %+v", result)
			}
			if len(messages) != 4 || !strings.Contains(messages[1], "Bedrock eu-west-1 unavailable:") {
				t.Errorf("missing degraded progress: %v", messages)
			}
		})
	}
}

func installFakeAWS(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return dir
}

func TestAWSExecutableArgumentsAndAutomaticPagination(t *testing.T) {
	cfg := paths.Config{AWSProfile: "sso bedrock; not-a-shell-command", DefaultRegion: "eu-west-1"}
	dir := installFakeAWS(t, `
service=$1
operation=$2
printf '%s\n' "$@" >> "$AWS_TEST_ARGS"
shift 2
[ "$#" -eq 12 ] || exit 91
[ "$1" = "--profile" ] && [ "$2" = "$AWS_TEST_PROFILE" ] || exit 92
shift 2
[ "$1" = "--region" ] && [ "$2" = "$AWS_TEST_REGION" ] || exit 93
shift 2
[ "$1" = "--output" ] && [ "$2" = "json" ] || exit 94
shift 2
[ "$1" = "--no-cli-pager" ] && [ "$2" = "--no-cli-auto-prompt" ] || exit 95
shift 2
[ "$1" = "--cli-connect-timeout" ] && [ "$2" = "10" ] || exit 96
shift 2
[ "$1" = "--cli-read-timeout" ] && [ "$2" = "20" ] || exit 97
case "$service:$operation" in
    sts:get-caller-identity) printf '%s\n' "$AWS_TEST_IDENTITY" ;;
    bedrock:list-inference-profiles) /bin/cat "$AWS_TEST_PROFILES" ;;
    bedrock:list-foundation-models) /bin/cat "$AWS_TEST_FOUNDATIONS" ;;
    *) exit 98 ;;
esac`)
	argsPath := filepath.Join(dir, "args")
	t.Setenv("AWS_TEST_ARGS", argsPath)
	t.Setenv("AWS_TEST_PROFILE", cfg.AWSProfile)
	t.Setenv("AWS_TEST_REGION", cfg.DefaultRegion)
	t.Setenv("AWS_TEST_IDENTITY", callerIdentityJSON)
	fixtures, err := filepath.Abs(fixtureDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_TEST_PROFILES", filepath.Join(fixtures, "bedrock-inference-profiles-eu-west-1.json"))
	t.Setenv("AWS_TEST_FOUNDATIONS", filepath.Join(fixtures, "bedrock-foundation-models-eu-west-1.json"))
	if err := CheckSSO(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	result := FetchListings(context.Background(), cfg)
	if len(result.Degraded) != 0 || len(result.ByRegion[cfg.DefaultRegion]) == 0 {
		t.Fatalf("fake AWS scan failed: %+v", result)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, args := range [][]string{
		{"sts", "get-caller-identity"},
		{"bedrock", "list-inference-profiles"},
		{"bedrock", "list-foundation-models"},
	} {
		want = append(want, args...)
		want = append(want, "--profile", cfg.AWSProfile, "--region", cfg.DefaultRegion, "--output", "json",
			"--no-cli-pager", "--no-cli-auto-prompt", "--cli-connect-timeout", "10", "--cli-read-timeout", "20")
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AWS arguments: got %q, want %q", got, want)
	}
	for _, arg := range got {
		if arg == "--no-paginate" || arg == "--max-items" || arg == "--starting-token" || arg == "sso" || arg == "login" {
			t.Errorf("pagination disabled or browser login started: %q", arg)
		}
	}
}

func TestAWSExecutableFailureDoesNotLeakSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, stderr, want string
	}{
		{"access denied", "An error occurred (AccessDeniedException) when calling ListInferenceProfiles: credential=super-secret", "AccessDeniedException"},
		{"SSO", "Error loading SSO Token: session=super-secret", "SSO token is missing or invalid"},
		{"credentials", "Unable to locate credentials. secret=super-secret", "AWS credentials were not found"},
		{"profile", "The config profile (super-secret) could not be found", "AWS profile was not found"},
		{"old CLI", "Unknown options: --no-cli-auto-prompt super-secret", "install AWS CLI v2"},
		{"endpoint", "Could not connect to endpoint URL: https://super-secret", "could not connect to the AWS endpoint"},
		{"timeout", "Read timeout on https://super-secret", "AWS request timed out"},
		{"unknown", "Unhandled error: super-secret", "stderr withheld to protect credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeAWS(t, `printf '%s\n' "$AWS_TEST_STDERR" >&2
printf '%s\n' 'super-secret stdout'
exit 42`)
			t.Setenv("AWS_TEST_STDERR", tc.stderr)
			data, err := execAWS(context.Background(), "sso-bedrock", "eu-west-1", "bedrock", "list-inference-profiles")
			if err == nil || data != nil {
				t.Fatalf("subprocess failure accepted: %q, %v", data, err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "exit status 42") || !strings.Contains(err.Error(), "aws bedrock list-inference-profiles") {
				t.Errorf("unclear error: %v", err)
			}
			if strings.Contains(err.Error(), "super-secret") {
				t.Errorf("error leaked credentials: %v", err)
			}
		})
	}
}

func TestAWSExecutableMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := execAWS(context.Background(), "sso-bedrock", "eu-west-1", "sts", "get-caller-identity")
	if err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("missing AWS CLI error: %v", err)
	}
}

func TestAWSExecutableTimeoutAndCancellation(t *testing.T) {
	if awsCommandTimeout <= 0 || awsCommandTimeout > 60*time.Second {
		t.Fatalf("AWS command timeout is not bounded: %v", awsCommandTimeout)
	}
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, context.DeadlineExceeded},
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// exec replaces the shell so cancellation kills the process, not a child shell.
			installFakeAWS(t, "exec /bin/sleep 30")
			ctx, cancel := tc.ctx()
			defer cancel()
			start := time.Now()
			data, err := execAWS(ctx, "sso-bedrock", "eu-west-1", "bedrock", "list-inference-profiles")
			if !errors.Is(err, tc.want) || data != nil {
				t.Fatalf("got %q, %v; want %v", data, err, tc.want)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("AWS cancellation took %v", elapsed)
			}
		})
	}
}

func TestMalformedJSONDoesNotLeakResponseContents(t *testing.T) {
	installFakeAWS(t, `printf '%s\n' '{"inferenceProfileSummaries":[{"inferenceProfileId":"super-secret"},broken]}'`)
	_, err := listInferenceProfiles(context.Background(), "sso-bedrock", "eu-west-1")
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("malformed JSON error: %v", err)
	}
}
