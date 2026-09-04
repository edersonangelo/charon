// Package main is the entry point for the charon binary.
//
// Charon is a self-hosted inbound webhook gateway: it accepts webhooks, makes
// them durable before acknowledging, then routes and delivers them with retry,
// dead lettering and replay.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `charon %s

Usage:
  charon <command> [flags]

Commands:
  version   Print the build version and exit
  help      Print this message

Charon is pre-alpha: no serving commands are implemented yet.
`

// errUnknownCommand is returned when the first argument is not a known command.
var errUnknownCommand = errors.New("unknown command")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "charon: %v\n", err)
		os.Exit(1)
	}
}

// run holds the whole command line so that it can be exercised by tests without
// touching os.Args, os.Exit or the real standard streams.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("charon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// Nothing useful can be done if writing usage to stderr fails.
	fs.Usage = func() { _, _ = fmt.Fprintf(stderr, usage, version) }

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}

	switch cmd := fs.Arg(0); cmd {
	case "version":
		_, err := fmt.Fprintf(stdout, "charon %s %s %s/%s\n",
			version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return err
	case "", "help":
		_, err := fmt.Fprintf(stdout, usage, version)
		return err
	default:
		return fmt.Errorf("%w: %q", errUnknownCommand, cmd)
	}
}
