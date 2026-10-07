// Command orchard checks which Pi models are usable across Bedrock regions
// and the AI Model Router. Use Pi's /model command to choose a model.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// version is overridden at build time via:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.1.0-dev"

const helpText = `orchard: check which models pi can use across Bedrock regions
and the AI Model Router. Use pi's /model command to choose a model.

Usage:
  orchard <command> [flags]

Commands:
  fetch    Discover the pi catalog, Bedrock models, and router deployment.
           Flags: --catalog-only  --strict
  probe    Probe discovered and scoped models through pi.
           Flags: --timeout N  --concurrency N  --fail-circuit N
  report   Show discovery and probe results without writing files.
           Flags: --json  --no-probe  --strict
  sync     Preview changes or write verified model configuration.
           Flags: --dry-run  --strict

Flags:
  --version    Show the version and exit.
  -h, --help   Show this help and exit.

Setup and model terms: https://github.com/luiul/orchard
`

// commands is the v1 subcommand set, reserved so no other use of these
// verbs creeps in while the rewrite lands command by command (see
// https://github.com/luiul/orchard/issues/9 for the milestone mapping).
var commands = []string{"fetch", "probe", "report", "sync"}

// config is parseArgs' validated result: exactly what main needs to
// decide what to do next, split out from flag.FlagSet's own parsing so
// that decision is unit-testable without exec'ing the real binary (see
// main_test.go).
type config struct {
	command     string
	args        []string // everything after the command, for its own FlagSet
	showVersion bool
}

// parseArgs validates args (os.Args[1:] in production), writing usage
// text to out. It returns flag.ErrHelp for -h/--help (callers treat it
// as "exit 0, not an error") and orchard's own errors for an unknown or
// missing command.
func parseArgs(args []string, out io.Writer) (config, error) {
	fs := flag.NewFlagSet("orchard", flag.ContinueOnError)
	fs.SetOutput(out)
	showVersion := fs.Bool("version", false, "Show the version and exit.")
	fs.Usage = func() { _, _ = fmt.Fprint(out, helpText) }

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if *showVersion {
		return config{showVersion: true}, nil
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return config{}, errors.New("orchard: command required")
	}
	for _, cmd := range commands {
		if rest[0] == cmd {
			return config{command: cmd, args: rest[1:]}, nil
		}
	}
	fs.Usage()
	return config{}, fmt.Errorf("orchard: unknown command %q", rest[0])
}

// run is main's testable body: it returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args, stdout)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.showVersion {
		_, _ = fmt.Fprintf(stdout, "orchard %s\n", version)
		return 0
	}
	return dispatch(cfg, stdout, stderr)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
