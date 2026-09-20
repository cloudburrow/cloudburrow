// Command cloudburrow runs a local Google Cloud emulator for development and
// testing.
//
// This entry point deliberately contains no behavior beyond argument dispatch.
// Configuration, validation, and process lifecycle belong to internal/config
// and internal/lifecycle; see docs/architecture.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/identity-wael/cloudburrow/internal/version"
)

const usage = `cloudburrow - a local Google Cloud emulator for development and testing

Usage:
  cloudburrow <command> [flags]

Commands:
  up          Start the emulator (not implemented yet)
  version     Print version information
  help        Print this message

Flags:
  -h, --help  Print this message

No emulator service is implemented yet. See docs/architecture.md for the
planned design and docs/compatibility.md for the per-operation status of every
service, all of which is currently marked Planned.

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

	case "up":
		// Registered rather than omitted: users will reach for it, and an
		// explicit pointer to the tracking issue is more useful than an
		// "unknown command" error that implies a typo.
		return errors.New("`up` is not implemented yet: the CLI, configuration, " +
			"and process lifecycle are tracked by issue #3")

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
