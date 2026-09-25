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

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/version"
)

const usage = `cloudburrow - a local Google Cloud emulator for development and testing

Usage:
  cloudburrow <command> [flags]

Commands:
  doctor      Check workstation prerequisites without changing anything
  env         Print the environment that points Google tooling at this instance
  up          Create the environment and run in the foreground; --detach
              runs it in the background and returns once it is ready
  wait        Wait for an instance to be ready (exit 0 ready, 1 failed, 2 timed out)
  logs        Print emulator, component and Cloud Run logs (--service, --follow)
  diagnose    Collect a redacted bundle for a bug report (diagnose -o bundle.tar.gz)
  gcloud-setup
              Write a gcloud configuration for this instance and print the
              export that selects it: eval "$(cloudburrow gcloud-setup)"
  gcloud-teardown
              Remove that configuration and print the unset
  state       Save or load the instance's state (state save|load <file>)
  storage-server
              Run the builtin Cloud Storage server alone (in development, #485)
  terraform   Run terraform (or --binary tofu) with the google provider pointed here
  status      Report the configured instance and its state
  stop        End a running up, then stop the cluster, preserving state a backend persists
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

` + "`up`" + ` creates a local Kubernetes cluster and serves Cloud Storage, Pub/Sub,
Cloud Tasks, Cloud Run and Secret Manager, with more behind --services, plus a
local web console. What is supported is recorded per operation in
docs/compatibility.md, each Verified row naming the official-SDK test behind it;
docs/status.md is the one-page version.

Project: https://github.com/cloudburrow/cloudburrow
`

// errUsage signals that usage should be printed and a non-zero status returned.
var errUsage = errors.New("usage")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		// A command with documented exit statuses, such as `wait`, chooses
		// its own; the message is still printed.
		var exit *exitError
		if errors.As(err, &exit) {
			if !exit.quiet {
				fmt.Fprintf(os.Stderr, "cloudburrow: %v\n", err)
			}
			os.Exit(exit.code)
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

	case "env":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "env")
		}
		return runEnv(context.Background(), args[1:], stdout, stderr)

	case "gcloud-setup":
		if hasHelpFlag(args[1:]) {
			fmt.Fprintln(stdout, "Usage: eval \"$(cloudburrow gcloud-setup [flags])\"\n\n"+
				"Write a gcloud configuration named cloudburrow-<name>, pointing storage and pubsub at this\n"+
				"instance with credentials disabled, and print the export that selects it in this shell.\n"+
				"Your default configuration is never changed.")
			return nil
		}
		return runGcloudSetup(args[1:], stdout, stderr)

	case "storage-server":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "storage-server")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runStorageServer(ctx, args[1:], stdout, stderr)

	case "gcloud-teardown":
		if hasHelpFlag(args[1:]) {
			fmt.Fprintln(stdout, "Usage: eval \"$(cloudburrow gcloud-teardown [flags])\"\n\n"+
				"Remove the cloudburrow-<name> gcloud configuration and print the unset. Harmless to repeat.")
			return nil
		}
		return runGcloudTeardown(args[1:], stdout, stderr)

	case "doctor":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "doctor")
		}
		// doctor changes nothing, so it needs no signal handling beyond the
		// bound each probe applies to itself.
		return runDoctor(context.Background(), args[1:], stdout, stderr)

	case "up":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "up")
		}
		// SIGINT/SIGTERM cancel the context, which unblocks runUp and begins a
		// bounded drain. A second signal is left to the Go default, so an
		// operator can always force an exit.
		detach, timeout, rest, err := upFlags(args[1:])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return errUsage
		}
		if detach {
			return runDetached(rest, timeout, stdout, stderr)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runUp(ctx, rest, stdout, stderr)

	case "wait":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "wait")
		}
		return runWait(args[1:], stdout, stderr)

	case "state":
		if hasHelpFlag(args[1:]) {
			fmt.Fprintln(stdout, "Usage: cloudburrow state save|load <file> [flags]\n\n"+
				"save writes the running instance's state to <file>; load replaces the state of the\n"+
				"services in <file> with it. See docs/configuration.md.")
			return nil
		}
		return runState(args[1:], stdout, stderr)

	case "diagnose":
		if hasHelpFlag(args[1:]) {
			fmt.Fprintln(stdout, "Usage: cloudburrow diagnose [-o bundle.tar.gz] [flags]\n\n"+
				"Collect version, configuration, doctor output, readiness, status, pods, events and\n"+
				"redacted logs into a bundle for a bug report. It never reads the kubeconfig's\n"+
				"contents, Kubernetes Secrets, the ADC key or Secret Manager payloads.")
			return nil
		}
		return runDiagnose(context.Background(), args[1:], stdout, stderr)

	case "terraform":
		// No help flag of our own: `terraform --help` is terraform's.
		return runTerraform(args[1:], stdout, stderr)

	case "logs":
		if hasHelpFlag(args[1:]) {
			return printCommandHelp(stdout, "logs")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runLogs(ctx, args[1:], stdout, stderr)

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
	fmt.Fprintf(w, "Usage: cloudburrow %s [flags]\n\n", cmd)
	switch cmd {
	case "up":
		fmt.Fprint(w, `Flags of up:
  -detach
    	run in the background and return once /readyz is 200; the log is
    	up.log in the instance directory, and `+"`stop`"+` ends the process
  -detach-timeout duration
    	how long -detach waits for readiness (default 10m)

`)
	case "storage-server":
		fmt.Fprint(w, `Run CloudBurrow's own Cloud Storage server, which is being built to Google's spec
to replace fake-gcs-server (#485). Methods not built yet answer 501 notImplemented.

Flags of storage-server:
  -listen address
    	address to serve on (default 127.0.0.1:4443)
  -host names
    	comma-separated host names clients use, for virtual-hosted XML requests
    	(<bucket>.<host>)
  -allow-remote
    	permit a non-loopback listen address, which exposes an unauthenticated server
  -data-dir directory
    	keep state there, durably (default: memory only)
  -pubsub-emulator host:port
    	deliver notifications to this Pub/Sub emulator; without it,
    	notificationConfigs cannot be created (501)

`)
	case "wait":
		fmt.Fprint(w, `Flags of wait:
  -timeout duration
    	how long to wait for readiness (default 5m)

Exit status: 0 ready, 1 a component failed or the process exited, 2 timed out.

`)
	case "logs":
		fmt.Fprint(w, `Flags of logs:
  -service name
    	one service: storage, pubsub, run, an opt-in emulator, or an in-process
    	one (tasks, secretmanager, metadata); default all of this instance's
  -resource name
    	with -service run, one Cloud Run service
  -follow
    	stream until interrupted (exit 130)
  -since duration
    	only lines newer than this, such as 10m
  -tail n
    	lines per container, and of the in-process log, before following (default 100)
  -format text|json
    	one JSON object per line with json

Reads only this instance's cluster, through its own kubeconfig, and only
CloudBurrow's resources in it. Credentials in a line are redacted.

`)
	}
	fmt.Fprintln(w, "Flags:")
	config.Usage(w)
	return nil
}
