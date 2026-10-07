// Package progress sends optional, serialized progress messages to the CLI.
// The context keeps presentation out of the fetch and probe APIs.
package progress

import (
	"context"
	"fmt"
	"sync"
)

type key struct{}

type reporter struct {
	mu    sync.Mutex
	write func(string)
}

// WithReporter attaches a per-run reporter. Worker callbacks are serialized.
func WithReporter(ctx context.Context, write func(string)) context.Context {
	return context.WithValue(ctx, key{}, &reporter{write: write})
}

// Report does nothing unless the caller supplied a reporter.
func Report(ctx context.Context, format string, args ...any) {
	r, ok := ctx.Value(key{}).(*reporter)
	if !ok || r.write == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.write(fmt.Sprintf(format, args...))
}
