package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hooks"
	"github.com/cloudburrow/cloudburrow/internal/trust"
)

// The trust gate (#598).
//
// `up` acts on two things a directory can carry without the developer naming
// them: ./cloudburrow.json, which chooses images, paths and ports, and the
// default .cloudburrow/hooks, whose scripts run on this machine as the
// developer. Neither is used until the developer has seen the list and said
// so, with `up --trust` or `cloudburrow trust`; the record is a hash of the
// files, so an edit, a new script or a chmod +x asks again. What the
// developer names themselves, with --hooks-dir, CLOUDBURROW_HOOKS_DIR or
// --config, is their own choice and needs no record: that is how CI passes
// its hook fixtures.

// trustSubject lists the files `up` would act on that the developer did not
// name: the discovered config file, and each hook script that would run from
// a hooks directory they did not choose.
func trustSubject(cfg config.Config) ([]string, error) {
	var files []string
	if cfg.Source.Discovered && cfg.Source.File != "" {
		files = append(files, cfg.Source.File)
	}
	if cfg.Source.HooksDirNamed || cfg.HooksDir == "" {
		return files, nil
	}
	for _, stage := range []string{hooks.Ready, hooks.Shutdown} {
		names, err := hooks.Scripts(cfg.HooksDir, stage)
		if err != nil {
			return nil, fmt.Errorf("hooks: cannot read %s: %w", cfg.HooksDir, err)
		}
		for _, name := range names {
			path := filepath.Join(cfg.HooksDir, stage, name)
			// Only what would run: hooks.Run skips anything that is not a
			// regular executable file. Making one executable later changes
			// the list, and so the hash.
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				files = append(files, path)
			}
		}
	}
	return files, nil
}

// errUntrusted marks the gate's refusal.
var errUntrusted = errors.New("not trusted")

// checkTrust passes when there is nothing to trust or the current files are
// the ones trusted; with record, it trusts them. Otherwise it refuses,
// naming every file, before anything is created.
func checkTrust(cfg config.Config, record bool, stdout io.Writer) error {
	files, err := trustSubject(cfg)
	if err != nil || len(files) == 0 {
		return err
	}
	if cfg.Source.TrustDir == "" {
		return fmt.Errorf("no home directory to keep the trust record in; pass --state-dir " +
			"(or name the config file with --config and the hooks with --hooks-dir)")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	sum, err := trust.Hash(files)
	if err != nil {
		return fmt.Errorf("hash the files to trust: %w", err)
	}
	entry, known, err := trust.Lookup(cfg.Source.TrustDir, dir)
	if err != nil {
		return err
	}
	if known && entry.SHA256 == sum {
		return nil
	}
	if record {
		if err := trust.Record(cfg.Source.TrustDir, dir, sum, files); err != nil {
			return fmt.Errorf("record trust: %w", err)
		}
		fmt.Fprintf(stdout, "trusted %s:\n%s", dir, listTrustFiles(cfg, files))
		return nil
	}
	what := "has files `up` would act on that you have not trusted"
	if known {
		what = "has changed since you trusted it"
	}
	return fmt.Errorf("%w: %s %s:\n%s"+
		"hooks run on this machine as you, and the config file chooses images, paths and ports.\n"+
		"Read them, then run `cloudburrow up --trust` (or `cloudburrow trust`) to trust these exact\n"+
		"contents; any change asks again. See docs/configuration.md#trust", errUntrusted, dir, what, listTrustFiles(cfg, files))
}

func listTrustFiles(cfg config.Config, files []string) string {
	var b strings.Builder
	for _, f := range files {
		kind := "hook"
		if f == cfg.Source.File {
			kind = "config"
		}
		fmt.Fprintf(&b, "  %-6s  %s\n", kind, f)
	}
	return b.String()
}

// trustFlag separates `up`'s own -trust from the shared configuration flags.
func trustFlag(args []string) (bool, []string, error) {
	v, found, rest, err := splitFlag(args, "trust", true)
	if err != nil || !found {
		return false, rest, err
	}
	switch v {
	case "true", "1", "":
		return true, rest, nil
	case "false", "0":
		return false, rest, nil
	}
	return false, nil, fmt.Errorf("invalid -trust %q", v)
}

// gateUp runs the trust gate for `up`, before a cluster, a port or a
// background process exists. The detached child finds the record its
// parent wrote, so it passes too.
func gateUp(args []string, record bool, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	return checkTrust(cfg, record, stdout)
}

// runTrust implements `cloudburrow trust`.
func runTrust(args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	files, err := trustSubject(cfg)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintln(stdout, "nothing here needs trust: no ./cloudburrow.json and no hooks you did not name")
		return nil
	}
	if err := checkTrust(cfg, false, stdout); err == nil {
		fmt.Fprintf(stdout, "already trusted:\n%s", listTrustFiles(cfg, files))
		return nil
	}
	return checkTrust(cfg, true, stdout)
}
