package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/luiul/orchard/internal/model"
	"github.com/luiul/orchard/internal/paths"
	"github.com/luiul/orchard/internal/progress"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func eventOutput(provider, model, stop, text, failure string) string {
	message := map[string]any{
		"role": "assistant", "provider": provider, "model": model,
		"stopReason": stop, "content": []map[string]string{{"type": "text", "text": text}},
	}
	if failure != "" {
		message["errorMessage"] = failure
	}
	data, _ := json.Marshal(map[string]any{"type": "message_end", "message": message})
	return string(data) + "\n"
}

func TestClassifyFullPiStreamWithStringSystemContent(t *testing.T) {
	status, systemic, reason := Classify(0, fixture(t, "probe-ok-json.jsonl"), false, 45)
	if status != OK || systemic || reason != "" {
		t.Fatalf("valid Pi stream was rejected: %s systemic=%v reason=%s", status, systemic, reason)
	}
}

func TestClassifyIgnoresNonAssistantStringContentOnFailure(t *testing.T) {
	prefix := `{"type":"message_end","message":{"role":"system","content":"system prompt"}}` + "\n"
	output := prefix + eventOutput(model.Bedrock, "model", "error", "", "AccessDeniedException: subscription missing")
	status, systemic, reason := Classify(0, output, false, 45)
	if status != FAIL || systemic || !strings.Contains(reason, "subscription missing") {
		t.Fatalf("provider failure hidden by system message: %s %v %s", status, systemic, reason)
	}
}

func TestValidFullPiStreamsDoNotTripCircuit(t *testing.T) {
	old := runPi
	t.Cleanup(func() { runPi = old })
	output := fixture(t, "probe-ok-json.jsonl")
	runPi = func(context.Context, string, string, string, paths.Config) (int, string, bool) {
		return 0, output, false
	}
	targets := make([]Target, 8)
	for i := range targets {
		targets[i] = Target{ID: "eu.amazon.nova-2-lite-v1:0", Provider: model.Bedrock, Region: "eu-west-1"}
	}
	batch := All(context.Background(), targets, paths.Config{ProbeConcurrency: 3, ProbeFailCircuit: 6, ProbeTimeout: 45})
	if batch.Tripped {
		t.Fatal("valid full Pi streams tripped the breaker")
	}
	for _, result := range batch.Outcomes {
		if result.Status != OK {
			t.Fatalf("valid probe failed: %+v", result)
		}
	}
}

func TestClassifyOKBedrock(t *testing.T) {
	status, systemic, reason := Classify(0, eventOutput(model.Bedrock, "model", "stop", fixture(t, "probe-ok-bedrock.txt"), ""), false, 45)
	if status != OK || systemic || reason != "" {
		t.Errorf("got %v %v %q", status, systemic, reason)
	}
}

func TestClassifyOKRouter(t *testing.T) {
	status, _, _ := Classify(0, eventOutput(model.Router, "model", "stop", fixture(t, "probe-ok-router.txt"), ""), false, 45)
	if status != OK {
		t.Errorf("got %v", status)
	}
}

func TestClassifyPerModelFailure(t *testing.T) {
	status, systemic, reason := Classify(1, eventOutput(model.Bedrock, "model", "error", "", fixture(t, "probe-fail-model.txt")), false, 45)
	if status != FAIL {
		t.Errorf("status: %v", status)
	}
	if systemic {
		t.Error("marketplace subscription missing is per-model, not systemic")
	}
	if !strings.HasPrefix(reason, "(AccessDeniedException") {
		t.Errorf("reason: %q", reason)
	}
	if len(reason) > 122 { // truncated to 120 chars plus parens
		t.Errorf("reason too long: %d", len(reason))
	}
}

func TestClassifySystemicFailure(t *testing.T) {
	status, systemic, _ := Classify(1, eventOutput(model.Bedrock, "model", "error", "", fixture(t, "probe-fail-systemic.txt")), false, 45)
	if status != FAIL || !systemic {
		t.Errorf("got %v %v", status, systemic) // "Could not load credentials"
	}
}

func TestClassifyTimeoutIsSystemic(t *testing.T) {
	status, systemic, reason := Classify(1, "", true, 45)
	if status != FAIL || !systemic {
		t.Errorf("got %v %v", status, systemic)
	}
	if !strings.Contains(reason, "timed out after 45s") {
		t.Errorf("reason: %q", reason)
	}
}

func TestEmptyOutputIsNotAWorkingModel(t *testing.T) {
	status, _, _ := Classify(0, "", false, 45)
	if status != FAIL {
		t.Fatalf("empty pi response: %s, want FAIL", status)
	}
}

func TestExitZeroWithErrorTextStillFails(t *testing.T) {
	// pi can exit 0 even when the model call fails: text wins over exit code.
	status, systemic, _ := Classify(0, eventOutput(model.Bedrock, "model", "error", "", fixture(t, "probe-fail-model.txt")), false, 45)
	if status != FAIL || systemic {
		t.Errorf("got %v %v", status, systemic)
	}
}

func TestSuccessfulTextMayContainErrorWords(t *testing.T) {
	status, systemic, _ := Classify(0, eventOutput(model.Router, "model", "stop", "No error or warning occurred.", ""), false, 45)
	if status != OK || systemic {
		t.Fatal("classified successful assistant text as an error")
	}
}

func TestMalformedOrUnfinishedResponseIsUncertain(t *testing.T) {
	for _, output := range []string{"hello", `{"type":"agent_start"}`, eventOutput(model.Router, "model", "pending", "hi", ""), eventOutput(model.Router, "model", "stop", "", "")} {
		status, systemic, _ := Classify(0, output, false, 45)
		if status != FAIL || !systemic {
			t.Errorf("accepted incomplete stream: %q", output)
		}
	}
}

func TestRealPiJSONStreamIntegration(t *testing.T) {
	if os.Getenv("ORCHARD_PI_INTEGRATION") != "1" {
		t.Skip("set ORCHARD_PI_INTEGRATION=1 to test real Pi against a local mock")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	dir := t.TempDir()
	config := map[string]any{"providers": map[string]any{model.Router: map[string]any{
		"baseUrl": server.URL + "/v1", "api": "openai-completions", "apiKey": "$AI_MODEL_ROUTER_API_KEY",
		"models": []map[string]any{{"id": "fixture-model", "name": "Fixture", "reasoning": false, "input": []string{"text"}, "contextWindow": 200000, "maxTokens": 100}},
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_MODEL_ROUTER_API_KEY", "local-test")
	outcome := probeOne(context.Background(), "fixture-model", model.Router, "", paths.Config{ProbeTimeout: 15, ProbeAgentDir: dir})
	if outcome.Status != OK || outcome.Systemic {
		t.Fatalf("real Pi stream rejected: %+v", outcome)
	}
}

func TestProbeRejectsPiFallbackModel(t *testing.T) {
	old := runPi
	t.Cleanup(func() { runPi = old })
	runPi = func(context.Context, string, string, string, paths.Config) (int, string, bool) {
		return 0, eventOutput(model.Router, "fallback", "stop", "hi", ""), false
	}
	result := probeOne(context.Background(), "requested", model.Router, "", paths.Config{ProbeTimeout: 1})
	if result.Status != FAIL || !result.Systemic {
		t.Errorf("fallback response counted as requested model: %+v", result)
	}
}

func TestRunPiUsesJSONWithoutProjectResources(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ORCHARD_TEST_ARGS\"\nprintf '%s\\n' '" + strings.TrimSpace(eventOutput(model.Router, "model", "stop", "hi", "")) + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ORCHARD_TEST_ARGS", argsPath)
	result := probeOne(context.Background(), "model", model.Router, "", paths.Config{ProbeTimeout: 5, ProbeAgentDir: t.TempDir()})
	if result.Status != OK {
		t.Fatalf("fake Pi failed: %+v", result)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--mode\njson", "--no-extensions", "--no-approve", "--no-tools", "--no-context-files"} {
		if !strings.Contains(string(data), flag) {
			t.Errorf("missing %s", flag)
		}
	}
}

func TestProbeBatchReportsBeforeAndAfterEachCall(t *testing.T) {
	old := probeOne
	t.Cleanup(func() { probeOne = old })
	var messages []string
	ctx := progress.WithReporter(context.Background(), func(message string) { messages = append(messages, message) })
	probeOne = func(_ context.Context, id, provider, region string, _ paths.Config) Outcome {
		if len(messages) == 0 || !strings.Contains(messages[len(messages)-1], "started") {
			t.Error("no progress before blocking model call")
		}
		return Outcome{ID: id, Provider: provider, Region: region, Status: OK}
	}
	batch := All(ctx, []Target{{ID: "one", Provider: model.Router}}, paths.Config{ProbeConcurrency: 1, ProbeFailCircuit: 2})
	if batch.Tripped || len(messages) != 2 || !strings.Contains(messages[1], "1/1 OK") {
		t.Fatalf("missing completed progress: %v", messages)
	}
}

func TestRunPiPreservesStartupError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte("#!/bin/sh\necho 'unknown model argument' >&2\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	result := probeOne(context.Background(), "model", model.Router, "", paths.Config{ProbeTimeout: 5})
	if result.Status != FAIL || !result.Systemic || !strings.Contains(result.Reason, "unknown model argument") {
		t.Fatalf("startup diagnostic discarded: %+v", result)
	}
}

func outcome(status Status, systemic bool) Outcome {
	return Outcome{ID: "m", Provider: model.Bedrock, Region: "r", Status: status, Systemic: systemic}
}

func TestBreakerTripsOnConsecutiveSystemic(t *testing.T) {
	breaker := NewBreaker(3)
	breaker.Record(outcome(FAIL, true))
	breaker.Record(outcome(FAIL, true))
	if breaker.Tripped() {
		t.Fatal("tripped early")
	}
	breaker.Record(outcome(FAIL, true))
	if !breaker.Tripped() {
		t.Fatal("not tripped after 3 systemic failures")
	}
}

func TestBreakerPerModelFailuresDoNotTrip(t *testing.T) {
	breaker := NewBreaker(3)
	for range 10 {
		breaker.Record(outcome(FAIL, false))
	}
	if breaker.Tripped() {
		t.Fatal("tripped on per-model failures")
	}
	if breaker.streak != 0 {
		t.Errorf("streak: %d", breaker.streak)
	}
}

func TestBreakerPerModelFailuresDoNotResetSystemicStreak(t *testing.T) {
	// A systemic cascade in progress must not be hidden by per-model noise.
	breaker := NewBreaker(3)
	breaker.Record(outcome(FAIL, true))
	breaker.Record(outcome(FAIL, false)) // neutral
	breaker.Record(outcome(FAIL, true))
	if breaker.streak != 2 || breaker.Tripped() {
		t.Fatalf("streak %d, tripped %v", breaker.streak, breaker.Tripped())
	}
	breaker.Record(outcome(FAIL, true))
	if !breaker.Tripped() {
		t.Fatal("not tripped")
	}
}

func TestBreakerOKResetsStreak(t *testing.T) {
	breaker := NewBreaker(2)
	breaker.Record(outcome(FAIL, true))
	breaker.Record(outcome(OK, false))
	if breaker.streak != 0 {
		t.Fatalf("streak: %d", breaker.streak)
	}
	breaker.Record(outcome(FAIL, true))
	if breaker.Tripped() {
		t.Fatal("tripped")
	}
}

func TestProbeAllSkipsAfterTrip(t *testing.T) {
	scripted := []Status{FAIL, FAIL, FAIL, OK, OK, OK}
	var mu sync.Mutex
	calls := 0
	old := probeOne
	probeOne = func(_ context.Context, id, _, _ string, _ paths.Config) Outcome {
		mu.Lock()
		defer mu.Unlock()
		status := scripted[calls%len(scripted)]
		calls++
		return Outcome{ID: id, Provider: model.Bedrock, Region: "eu-west-1", Status: status, Systemic: status == FAIL}
	}
	t.Cleanup(func() { probeOne = old })

	cfg := paths.Config{ProbeTimeout: 5, ProbeConcurrency: 1, ProbeFailCircuit: 3}
	targets := make([]Target, 6)
	for i := range targets {
		targets[i] = Target{ID: "m" + string(rune('0'+i)), Provider: model.Bedrock, Region: "eu-west-1"}
	}
	batch := All(context.Background(), targets, cfg)
	if !batch.Tripped {
		t.Fatal("breaker did not trip")
	}
	sawSkip := false
	for _, o := range batch.Outcomes {
		if o.Status == SKIP {
			sawSkip = true
		}
	}
	if !sawSkip {
		t.Error("no SKIP outcome after the trip")
	}
	if len(batch.Outcomes) != len(targets) {
		t.Errorf("outcomes: %d, want %d", len(batch.Outcomes), len(targets))
	}
}
