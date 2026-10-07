// Package catalog reads the models that Pi knows by running pi --list-models.
// A catalog ID says nothing about account access or a successful model call.
package catalog

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/luiul/orchard/internal/model"
)

// runListModels is the subprocess seam, swapped in tests.
var runListModels = func(ctx context.Context) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pi", "--offline", "--no-extensions", "--no-approve", "--list-models")
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	return outBuf.String(), errBuf.String(), runErr
}

// Parse turns `pi --list-models` output into entries. The header line,
// blank lines, and malformed lines (field count != 6) are skipped.
func Parse(text string) []model.CatalogEntry {
	var entries []model.CatalogEntry
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "provider") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 6 {
			continue
		}
		entries = append(entries, model.CatalogEntry{
			Provider: parts[0],
			ID:       parts[1],
			Context:  parts[2],
			MaxOut:   parts[3],
			Thinking: parts[4],
			Images:   parts[5],
		})
	}
	return entries
}

// Load runs `pi --list-models` and parses it. Failing to find pi on PATH or
// parsing zero rows is a hard error: nothing downstream means anything
// without the catalog.
func Load(ctx context.Context) ([]model.CatalogEntry, error) {
	stdout, stderr, err := runListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("pi --list-models failed: %w (%s)", err, strings.TrimSpace(stderr))
	}
	entries := Parse(stdout)
	if len(entries) == 0 {
		return nil, fmt.Errorf("pi --list-models produced no rows (err: %v): %s", err, strings.TrimSpace(stderr))
	}
	return entries, nil
}
