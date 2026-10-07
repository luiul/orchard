package progress

import (
	"context"
	"sync"
	"testing"
)

func TestConcurrentProgressIsSerialized(t *testing.T) {
	var messages []string
	ctx := WithReporter(context.Background(), func(message string) { messages = append(messages, message) })
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); Report(ctx, "probe %d", i) }()
	}
	wg.Wait()
	if len(messages) != 20 {
		t.Fatalf("messages: %d", len(messages))
	}
}

func TestNoReporterIsSafe(t *testing.T) {
	Report(context.Background(), "unused progress")
}
