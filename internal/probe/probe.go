// Package probe tests candidate models by asking Pi for a short response.
// A successful process exit alone is not proof that the model answered.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/progress"
)

var systemicRE = regexp.MustCompile(`(?i)sso|expired.?token|credential|throttl|econnrefused|enotfound|etimedout|socket hang up|could not connect|unable to locate`)

// Status is the probe verdict.
type Status string

const (
	OK   Status = "OK"
	FAIL Status = "FAIL"
	SKIP Status = "SKIP" // cancelled or skipped after the circuit breaker tripped
)

// Outcome is one probe's verdict.
type Outcome struct {
	ID       string
	Provider string
	Region   string
	Status   Status
	Systemic bool
	Reason   string
}

// Target names one model to probe.
type Target struct {
	ID       string
	Provider string
	Region   string
	AgentDir string // isolated Pi config containing the proposed router definitions
}

type assistantResult struct {
	Role         string `json:"role"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
	Content      []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// Classify reads Pi's completed assistant message, never the wording of a
// successful answer. A zero exit without a completed response is uncertain.
func Classify(exitCode int, output string, timedOut bool, timeoutSecs int) (Status, bool, string) {
	status, systemic, reason, _ := classify(exitCode, output, timedOut, timeoutSecs)
	return status, systemic, reason
}

func classify(exitCode int, output string, timedOut bool, timeoutSecs int) (Status, bool, string, assistantResult) {
	if timedOut {
		return FAIL, true, fmt.Sprintf("(timed out after %ds)", timeoutSecs), assistantResult{}
	}
	var last assistantResult
	for line := range strings.SplitSeq(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return FAIL, true, "(invalid Pi JSON event stream)", last
		}
		if event.Type != "message_end" {
			continue
		}
		var header struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(event.Message, &header); err != nil {
			return FAIL, true, "(invalid Pi message envelope)", last
		}
		if header.Role != "assistant" {
			continue
		}
		if err := json.Unmarshal(event.Message, &last); err != nil {
			return FAIL, true, "(invalid Pi assistant response)", last
		}
	}
	if last.Role == "" {
		return FAIL, true, "(no completed Pi assistant response)", last
	}
	if last.StopReason == "error" || last.StopReason == "aborted" || last.ErrorMessage != "" {
		reason := strings.Join(strings.Fields(last.ErrorMessage), " ")
		if reason == "" {
			reason = "Pi response ended with " + last.StopReason
		}
		runes := []rune(reason)
		if len(runes) > 120 {
			reason = string(runes[:120])
		}
		return FAIL, last.StopReason == "aborted" || systemicRE.MatchString(last.ErrorMessage), "(" + reason + ")", last
	}
	if last.Provider == "" || last.Model == "" {
		return FAIL, true, "(Pi response did not identify its provider and model)", last
	}
	if exitCode == 0 && (last.StopReason == "stop" || last.StopReason == "length") {
		for _, block := range last.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				return OK, false, "", last
			}
		}
	}
	return FAIL, true, "(Pi did not complete a usable text response)", last
}

// runPi captures stdout JSONL and adds a structured diagnostic if Pi fails
// before starting a response. Stderr is never mixed into the event stream.
var runPi = func(ctx context.Context, id, provider, region string, cfg paths.Config) (exitCode int, output string, timedOut bool) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ProbeTimeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pi",
		"-p", "Reply with only hi.",
		"--mode", "json",
		"--no-context-files",
		"--no-skills",
		"--no-prompt-templates",
		"--thinking", "off",
		"--provider", provider,
		"--model", id,
		"--no-session",
		"--no-extensions",
		"--no-approve",
		"--no-tools",
	)
	cmd.Stdin = nil // devnull
	if provider == model.Bedrock {
		cmd.Env = append(os.Environ(),
			"AWS_PROFILE="+cfg.AWSProfile,
			"AWS_REGION="+region,
		)
	} else if cfg.ProbeAgentDir != "" {
		cmd.Env = append(os.Environ(), "PI_CODING_AGENT_DIR="+cfg.ProbeAgentDir, "PI_OFFLINE=1")
	}
	// Stdout is JSONL. Stderr diagnostics must not corrupt the event stream.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	} else if err != nil {
		exitCode = 1
	}
	timedOut = ctx.Err() == context.DeadlineExceeded
	if strings.TrimSpace(stdout.String()) == "" && err != nil && !timedOut {
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic == "" {
			diagnostic = err.Error()
		}
		// A CLI/setup failure must stop sync, even if its text resembles a
		// per-model provider error. Preserve a short diagnostic for progress.
		message := map[string]any{"role": "assistant", "provider": provider, "model": id, "stopReason": "aborted", "errorMessage": "Pi process failed: " + diagnostic}
		_ = json.NewEncoder(&stdout).Encode(map[string]any{"type": "message_end", "message": message})
	}
	return exitCode, stdout.String(), timedOut
}

// probeOne probes one model through pi, with a kill-on-timeout deadline.
var probeOne = func(ctx context.Context, id, provider, region string, cfg paths.Config) Outcome {
	exitCode, output, timedOut := runPi(ctx, id, provider, region, cfg)
	status, systemic, reason, message := classify(exitCode, output, timedOut, cfg.ProbeTimeout)
	if status == OK && (message.Provider != provider || message.Model != id) {
		status, systemic, reason = FAIL, true, "(Pi answered with a different provider or model)"
	}
	return Outcome{ID: id, Provider: provider, Region: region, Status: status, Systemic: systemic, Reason: reason}
}

// Breaker counts consecutive systemic failures and trips at the threshold.
// Per-model failures are neutral: they neither advance nor reset the
// streak. Any OK resets it.
type Breaker struct {
	threshold int
	streak    int
	tripped   bool
	mu        sync.Mutex
}

// NewBreaker returns a Breaker that trips after threshold consecutive
// systemic failures.
func NewBreaker(threshold int) *Breaker {
	return &Breaker{threshold: threshold}
}

// Record folds one outcome into the streak.
func (b *Breaker) Record(o Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case o.Status == OK:
		b.streak = 0
	case o.Status == FAIL && o.Systemic:
		b.streak++
		if b.streak >= b.threshold {
			b.tripped = true
		}
	}
	// Per-model FAILs are neutral: they neither advance nor reset the streak.
}

// Tripped reports whether the breaker has tripped.
func (b *Breaker) Tripped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped
}

// Batch is the result of probing every candidate.
type Batch struct {
	Outcomes []Outcome
	Tripped  bool
}

// All probes every candidate with bounded workers and a circuit breaker.
// Outcomes preserve target order.
func All(ctx context.Context, targets []Target, cfg paths.Config) Batch {
	breaker := NewBreaker(cfg.ProbeFailCircuit)
	outcomes := make([]Outcome, len(targets))
	jobs := make(chan int)
	var wg sync.WaitGroup
	started, completed := 0, 0
	var progressMu sync.Mutex
	for range max(cfg.ProbeConcurrency, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				target := targets[i]
				if breaker.Tripped() || ctx.Err() != nil {
					reason := "(circuit breaker tripped)"
					if ctx.Err() != nil {
						reason = "(probe cancelled)"
					}
					outcomes[i] = Outcome{
						ID:       target.ID,
						Provider: target.Provider,
						Region:   target.Region,
						Status:   SKIP,
						Reason:   reason,
					}
					continue
				}
				progressMu.Lock()
				started++
				progress.Report(ctx, "probe %d/%d started: %s/%s%s", started, len(targets), target.Provider, target.ID, regionLabel(target.Region))
				progressMu.Unlock()
				probeCfg := cfg
				probeCfg.ProbeAgentDir = target.AgentDir
				outcome := probeOne(ctx, target.ID, target.Provider, target.Region, probeCfg)
				breaker.Record(outcome)
				outcomes[i] = outcome
				progressMu.Lock()
				completed++
				progress.Report(ctx, "probe %d/%d %s: %s/%s%s %s", completed, len(targets), outcome.Status, target.Provider, target.ID, regionLabel(target.Region), outcome.Reason)
				progressMu.Unlock()
			}
		}()
	}
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if breaker.Tripped() {
		progress.Report(ctx, "probe batch stopped: circuit breaker tripped (%d/%d completed)", completed, len(targets))
	}
	return Batch{Outcomes: outcomes, Tripped: breaker.Tripped()}
}

func regionLabel(region string) string {
	if region == "" {
		return ""
	}
	return " (" + region + ")"
}
