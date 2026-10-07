// Package model defines the shared model and report types.
// Catalog, discovery, and probing are separate facts. A model can match a
// curated Pi scope pattern without working, and a working model may be outside
// that scope. Unknown means the source failed or probing did not run.
package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Provider ids as pi reports them.
const (
	Bedrock = "amazon-bedrock"
	Router  = "ai-model-router"
)

// Providers lists every provider orchard covers.
var Providers = []string{Bedrock, Router}

// State records yes, no, or unknown for one independent model status.
type State string

const (
	Yes     State = "yes"
	No      State = "no"
	Unknown State = "unknown"
)

// CatalogEntry is one parsed row of `pi --list-models`.
type CatalogEntry struct {
	Provider string
	ID       string
	Context  string
	MaxOut   string
	Thinking string
	Images   string
}

// Stats holds metadata from Pi's catalog or the router, when present.
type Stats struct {
	Context       int
	MaxOut        int
	CostIn        *float64 // per 1M tokens
	CostOut       *float64
	CostCacheRead *float64
	Reasoning     bool
	Vision        bool
}

// Row holds independent status flags for one provider and model ID.
type Row struct {
	Provider       string
	ID             string
	Catalog        State
	ListedDeployed State // listed (Bedrock) / deployed (router)
	Candidate      State
	Invocable      State
	Scoped         State
	Region         string // Bedrock: the region it probes from
	Note           string
	Stats          Stats
}

// Drift holds four kinds of mismatch between sources and Pi's scope.
type Drift struct {
	DeployedUnknownToPi    []string
	ListedButUndeployed    []string
	UnmatchedScopePatterns []string
	InvocableUncovered     []string
}

// MarshalIndent produces sorted, indented JSON without a trailing newline.
func MarshalIndent(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// ParseTokenCount converts the catalog's human token counts ("200K",
// "131.1K", "1M") to an absolute number. Unknown formats yield 0.
func ParseTokenCount(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1_000, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1_000_000, strings.TrimSuffix(s, "M")
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("bad token count %q", s)
	}
	return int(n * mult), nil
}
