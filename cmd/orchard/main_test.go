package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseArgsVersion(t *testing.T) {
	var out bytes.Buffer
	cfg, err := parseArgs([]string{"--version"}, &out)
	if err != nil {
		t.Fatalf("parseArgs(--version) error: %v", err)
	}
	if !cfg.showVersion {
		t.Fatal("parseArgs(--version): showVersion not set")
	}
}

func TestParseArgsHelp(t *testing.T) {
	var out bytes.Buffer
	_, err := parseArgs([]string{"--help"}, &out)
	if err == nil {
		t.Fatal("parseArgs(--help): expected flag.ErrHelp, got nil")
	}
	if !strings.Contains(out.String(), "orchard <command>") {
		t.Fatalf("parseArgs(--help): help text missing usage line, got:\n%s", out.String())
	}
}

func TestParseArgsCommands(t *testing.T) {
	for _, cmd := range []string{"fetch", "probe", "report", "sync"} {
		var out bytes.Buffer
		cfg, err := parseArgs([]string{cmd}, &out)
		if err != nil {
			t.Fatalf("parseArgs(%q) error: %v", cmd, err)
		}
		if cfg.command != cmd {
			t.Fatalf("parseArgs(%q): command = %q", cmd, cfg.command)
		}
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(--version) exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "orchard ") {
		t.Fatalf("run(--version) stdout = %q", stdout.String())
	}
}

func TestRunUnknownCommandExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"frobnicate"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run(frobnicate) exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "frobnicate"`) {
		t.Fatalf("run(frobnicate) stderr = %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "orchard <command>") {
		t.Fatalf("run(frobnicate): usage text not printed to stdout")
	}
}

func TestRunMissingCommandExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("run() exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "command required") {
		t.Fatalf("run() stderr = %q", stderr.String())
	}
}
