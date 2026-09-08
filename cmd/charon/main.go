package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
)

var version = "dev"

const usage = `charon %s

Usage:
  charon <command> [flags]

Commands:
  serve     Accept inbound webhooks and record them
  dispatch  Deliver recorded events to their destinations
  route     Manage where a provider's events are delivered
  user      Create an operator who can sign in to the panel
  verify    Configure how a provider's signature is checked
  sign      Configure how deliveries leaving here are signed
  tenant    Manage the tenants events are recorded for
  role      Manage roles, what they grant and who gets them
  migrate   Apply pending schema migrations and exit
  version   Print the build version and exit
  help      Print this message

Run "charon <command> -h" for the flags a command accepts.
`

var errUnknownCommand = errors.New("unknown command")

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "charon: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var cmd string
	rest := args
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		return serve(ctx, rest, stdout, stderr)
	case "dispatch":
		return dispatch(ctx, rest, stdout, stderr)
	case "route":
		return route(ctx, rest, stdout, stderr)
	case "user":
		return user(ctx, rest, stdout, stderr)
	case "verify":
		return verify(ctx, rest, stdout, stderr)
	case "sign":
		return sign(ctx, rest, stdout, stderr)
	case "tenant":
		return tenant(ctx, rest, stdout, stderr)
	case "role":
		return role(ctx, rest, stdout, stderr)
	case "migrate":
		return migrate(ctx, rest, stdout, stderr)
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
