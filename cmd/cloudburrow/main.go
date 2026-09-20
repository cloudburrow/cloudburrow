// Command cloudburrow runs a local Google Cloud emulator for development and
// testing.
//
// This entry point deliberately contains no behavior beyond argument dispatch.
// Configuration, validation, and process lifecycle belong to internal/config
// and internal/lifecycle; see docs/architecture.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/identity-wael/cloudburrow/internal/config"
	"github.com/identity-wael/cloudburrow/internal/version"
)

const usage = `cloudburrow - a local Google Cloud emulator for development and testing

Usage:
  cloudburrow <command> [flags]

Commands:
  up          Create the environment and run in the foreground
  status      Report the configured instance and its state
  stop        Stop the cluster, preserving state a backend persists
  reset       Destroy CloudBurrow-managed state, keeping the cluster
  delete      Destroy the cluster CloudBurrow created
  version     Print version information
  help        Print this message

stop, reset and delete are distinct: none implies another.

Flags:
  -h, --help  Print this message

Configuration precedence, highest first:
  flags  >  environment (CLOUDBURROW_*)  >  config file  >  defaults

The config file is located by --config, then CLOUDBURROW_CONFIG, then
./cloudburrow.json when present.

No emulator service is implemented yet: ` + "`up`" + ` starts the lifecycle coordinator
and the control port only. See docs/architecture.md for the planned design and
docs/compatibility.md for the per-operation status of every service, all of
which is currently marked Planned.

Project: https://github.com/identity-wael/cloudburrow
`

// errUsage signals that usage should be printed and a non-zero status returned.
var errUsage = errors.New("usage")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "cloudburrow: %v\n", err)
		os.Exit(1)
	}
}

// run dispatches a command. It takes its output streams as arguments so that
// tests can exercise it without touching the process globals.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}

	switch cmd := args[0]; cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil

	case "version", "-v", "--version":
		return runVersion(args[1:], stdout, stderr)

	case "status":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "status")
		}
		return runStatus(args[1:], stdout, stderr)

	case "stop":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "stop")
		}
		return runStop(args[1:], stdout, stderr)

	case "reset":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "reset")
		}
		return runReset(args[1:], stdout, stderr)

	case "delete":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "delete")
		}
		return runDelete(args[1:], stdout, stderr)

	case "up":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "up")
		}
		// SIGINT/SIGTERM cancel the context, which unblocks runUp and begins a
		// bounded drain. A second signal is left to the Go default, so an
		// operator can always force an exit.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runUp(ctx, args[1:], stdout, stderr)

	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		fmt.Fprint(stderr, usage)
		return errUsage
	}
}

func runVersion(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	short := fs.Bool("short", false, "print only the version string")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	info := version.Get()
	if *short {
		fmt.Fprintln(stdout, info.Version)
		return nil
	}
	fmt.Fprintln(stdout, info.String())
	return nil
}

// hasHelpFlag reports whether the arguments request help.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}

// printCommandHelp prints the shared flag documentation for a subcommand.
func printCommandHelp(w io.Writer, cmd string) error {
	fmt.Fprintf(w, "Usage: cloudburrow %s [flags]\n\nFlags:\n", cmd)
	config.Usage(w)
	return nil
}
