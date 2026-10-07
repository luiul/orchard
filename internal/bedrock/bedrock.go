// Package bedrock discovers model IDs in each scanned AWS region. Listings
// suggest where to probe; they do not prove that a Pi call will work.
// Orchard checks SSO once and never opens a login flow automatically.
package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/progress"
)

// ProfileSummary / ModelSummary are the shapes used by the discovery logic.
// The AWS CLI responses are checked before being converted.
type ProfileSummary struct {
	ID string
}

type ModelSummary struct {
	ID             string
	InferenceTypes []string
}

const awsCommandTimeout = 60 * time.Second

// runAWS is the only AWS process seam. Orchard does not connect to AWS itself.
var runAWS = execAWS

func execAWS(ctx context.Context, profile, region string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, awsCommandTimeout)
	defer cancel()
	cliArgs := append(append([]string{}, args...),
		"--profile", profile, "--region", region, "--output", "json",
		"--no-cli-pager", "--no-cli-auto-prompt",
		"--cli-connect-timeout", "10", "--cli-read-timeout", "20",
	)
	// Leave automatic pagination enabled so every inference profile is listed.
	cmd := exec.CommandContext(ctx, "aws", cliArgs...)
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	operation := "aws " + strings.Join(args, " ")
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: %w", operation, ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w (%s)", operation, err, awsErrorSummary(stderr.String()))
	}
	return data, nil
}

// AWS error messages can include credentials or endpoint details. Report only
// known error codes and fixed hints, never raw stderr or response contents.
func awsErrorSummary(stderr string) string {
	for _, code := range []string{
		"AccessDenied", "AccessDeniedException", "UnauthorizedException",
		"ExpiredToken", "ExpiredTokenException", "UnrecognizedClientException",
		"InvalidClientTokenId", "SignatureDoesNotMatch", "ValidationException",
		"Throttling", "ThrottlingException", "TooManyRequestsException",
		"ServiceUnavailableException", "InternalServerException",
		"RequestTimeout", "RequestTimeoutException",
	} {
		if strings.Contains(stderr, "("+code+")") {
			return code
		}
	}
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "sso"), strings.Contains(lower, "token has expired"):
		return "SSO token is missing or invalid"
	case strings.Contains(lower, "unable to locate credentials"):
		return "AWS credentials were not found"
	case strings.Contains(lower, "config profile") && strings.Contains(lower, "could not be found"):
		return "AWS profile was not found"
	case strings.Contains(lower, "unknown options"):
		return "AWS CLI options are not supported; install AWS CLI v2"
	case strings.Contains(lower, "could not connect to endpoint"):
		return "could not connect to the AWS endpoint"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return "AWS request timed out"
	default:
		return "AWS CLI failed; stderr withheld to protect credentials"
	}
}

func getCallerIdentity(ctx context.Context, profile, region string) error {
	data, err := runAWS(ctx, profile, region, "sts", "get-caller-identity")
	if err != nil {
		return err
	}
	var wire struct {
		UserID  string `json:"UserId"`
		Account string `json:"Account"`
		ARN     string `json:"Arn"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("aws sts get-caller-identity returned invalid JSON")
	}
	if strings.TrimSpace(wire.UserID) == "" || strings.TrimSpace(wire.Account) == "" || strings.TrimSpace(wire.ARN) == "" {
		return fmt.Errorf("aws sts get-caller-identity response is missing UserId, Account, or Arn")
	}
	return nil
}

func listInferenceProfiles(ctx context.Context, profile, region string) ([]ProfileSummary, error) {
	data, err := runAWS(ctx, profile, region, "bedrock", "list-inference-profiles")
	if err != nil {
		return nil, err
	}
	return parseInferenceProfiles(data)
}

func parseInferenceProfiles(data []byte) ([]ProfileSummary, error) {
	var wire struct {
		Summaries []struct {
			ID string `json:"inferenceProfileId"`
		} `json:"inferenceProfileSummaries"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("aws bedrock list-inference-profiles returned invalid JSON")
	}
	if wire.Summaries == nil {
		return nil, fmt.Errorf("aws bedrock list-inference-profiles response is missing inferenceProfileSummaries array")
	}
	out := make([]ProfileSummary, 0, len(wire.Summaries))
	for i, s := range wire.Summaries {
		if strings.TrimSpace(s.ID) == "" {
			return nil, fmt.Errorf("aws bedrock list-inference-profiles response is missing inferenceProfileId at index %d", i)
		}
		out = append(out, ProfileSummary{ID: s.ID})
	}
	return out, nil
}

func listFoundationModels(ctx context.Context, profile, region string) ([]ModelSummary, error) {
	data, err := runAWS(ctx, profile, region, "bedrock", "list-foundation-models")
	if err != nil {
		return nil, err
	}
	return parseFoundationModels(data)
}

func parseFoundationModels(data []byte) ([]ModelSummary, error) {
	var wire struct {
		Summaries []struct {
			ID             string   `json:"modelId"`
			InferenceTypes []string `json:"inferenceTypesSupported"`
		} `json:"modelSummaries"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("aws bedrock list-foundation-models returned invalid JSON")
	}
	if wire.Summaries == nil {
		return nil, fmt.Errorf("aws bedrock list-foundation-models response is missing modelSummaries array")
	}
	out := make([]ModelSummary, 0, len(wire.Summaries))
	for i, s := range wire.Summaries {
		if strings.TrimSpace(s.ID) == "" || s.InferenceTypes == nil {
			return nil, fmt.Errorf("aws bedrock list-foundation-models response is missing modelId or inferenceTypesSupported at index %d", i)
		}
		for _, typ := range s.InferenceTypes {
			if strings.TrimSpace(typ) == "" {
				return nil, fmt.Errorf("aws bedrock list-foundation-models response has an empty inference type at index %d", i)
			}
		}
		out = append(out, ModelSummary{ID: s.ID, InferenceTypes: s.InferenceTypes})
	}
	return out, nil
}

// prefixHomeRegion maps an inference-profile id prefix to the region group
// it is invocable from. Order matters (it is a slice, not a map);
// "global." is special-cased to the default region in AssignCandidates.
var prefixHomeRegion = []struct{ prefix, region string }{
	{"eu.", "eu-west-1"},
	{"us.", "us-east-1"},
	{"jp.", "ap-northeast-1"},
	{"apac.", "ap-northeast-1"},
	{"au.", "ap-southeast-2"},
}

// SsoInvalidError means the SSO session is invalid. The message carries the
// exact fix command; a human must log in, orchard never does.
type SsoInvalidError struct {
	Profile string
	Detail  string
}

func (e *SsoInvalidError) Error() string {
	return fmt.Sprintf("AWS SSO session invalid for %q. Run this yourself: aws sso login --profile %s\n(%s)",
		e.Profile, e.Profile, e.Detail)
}

// CheckSSO is the single SSO validity check for the whole run. It never
// logs in.
func CheckSSO(ctx context.Context, cfg paths.Config) error {
	if err := getCallerIdentity(ctx, cfg.AWSProfile, cfg.DefaultRegion); err != nil {
		return &SsoInvalidError{Profile: cfg.AWSProfile, Detail: err.Error()}
	}
	return nil
}

// Listings holds listed model ids per scanned region, plus per-region
// failures.
type Listings struct {
	ByRegion map[string]map[string]bool
	Degraded map[string]string // region -> error summary
}

// AllIDs unions every region's listed ids.
func (e Listings) AllIDs() map[string]bool {
	out := map[string]bool{}
	for _, ids := range e.ByRegion {
		for id := range ids {
			out[id] = true
		}
	}
	return out
}

// InferenceProfileIDs extracts profile ids from list-inference-profiles
// output.
func InferenceProfileIDs(summaries []ProfileSummary) map[string]bool {
	ids := map[string]bool{}
	for _, s := range summaries {
		if s.ID != "" {
			ids[s.ID] = true
		}
	}
	return ids
}

// OnDemandModelIDs extracts model ids whose inference types include
// ON_DEMAND from list-foundation-models output.
func OnDemandModelIDs(summaries []ModelSummary) map[string]bool {
	ids := map[string]bool{}
	for _, s := range summaries {
		onDemand := false
		for _, t := range s.InferenceTypes {
			if t == "ON_DEMAND" {
				onDemand = true
				break
			}
		}
		if onDemand && s.ID != "" {
			ids[s.ID] = true
		}
	}
	return ids
}

// FetchListings lists listed ids per scanned region. A failing region
// is degraded, not fatal.
func FetchListings(ctx context.Context, cfg paths.Config) Listings {
	result := Listings{ByRegion: map[string]map[string]bool{}, Degraded: map[string]string{}}
	for _, region := range cfg.Regions() {
		progress.Report(ctx, "fetching Bedrock models in %s", region)
		ids, err := fetchRegion(ctx, cfg, region)
		if err != nil {
			result.Degraded[region] = err.Error()
			progress.Report(ctx, "Bedrock %s unavailable: %s", region, err)
			continue
		}
		result.ByRegion[region] = ids
		progress.Report(ctx, "Bedrock %s: %d listed models", region, len(ids))
	}
	return result
}

func fetchRegion(ctx context.Context, cfg paths.Config, region string) (map[string]bool, error) {
	profiles, err := listInferenceProfiles(ctx, cfg.AWSProfile, region)
	if err != nil {
		return nil, err
	}
	foundations, err := listFoundationModels(ctx, cfg.AWSProfile, region)
	if err != nil {
		return nil, err
	}
	ids := InferenceProfileIDs(profiles)
	for id := range OnDemandModelIDs(foundations) {
		ids[id] = true
	}
	return ids, nil
}

// AssignCandidates intersects listed ids with the catalog per region,
// then assigns each candidate id exactly ONE probe region.
//
// Preference order, ported from the bash script: explicit registry
// override, then the default region (if the id is a candidate there), then
// the id's prefix home region, then the first scanned region that listed
// it.
func AssignCandidates(listedByRegion map[string]map[string]bool, catalogIDs map[string]bool, regions []string, defaultRegion string, regionOverrides map[string]string) map[string]string {
	perRegion := map[string][]string{}
	for _, region := range regions {
		var ids []string
		for id := range listedByRegion[region] {
			if catalogIDs[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		perRegion[region] = ids
	}

	assigned := map[string]string{}
	for _, region := range regions {
		for _, id := range perRegion[region] {
			if _, done := assigned[id]; done {
				continue
			}
			if override, ok := regionOverrides[id]; ok {
				assigned[id] = override
				continue
			}
			if contains(perRegion[defaultRegion], id) {
				assigned[id] = defaultRegion
				continue
			}
			home := ""
			if strings.HasPrefix(id, "global.") {
				home = defaultRegion
			} else {
				for _, ph := range prefixHomeRegion {
					if strings.HasPrefix(id, ph.prefix) {
						home = ph.region
						break
					}
				}
			}
			if home == "" {
				home = region
			}
			assigned[id] = home
		}
	}
	return assigned
}

func contains(ids []string, id string) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
