package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// `cloudburrow state save <file>` and `state load <file>` (#289), over the
// running instance's loopback-only admin API.
func runState(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || (args[0] != "save" && args[0] != "load") {
		fmt.Fprintln(stderr, "usage: cloudburrow state save|load <file> [flags]")
		return errUsage
	}
	verb, file := args[0], args[1]
	cfg, err := config.Load(config.Options{Args: args[2:], Output: stderr})
	if err != nil {
		return err
	}
	info, ok := running(cfg)
	if !ok {
		return fmt.Errorf("instance %q is not running; start it with `cloudburrow up`", cfg.Name)
	}
	base := "http://" + info.Control + "/admin/state/"
	if verb == "save" {
		return saveState(base+"export", file, stdout)
	}
	return loadState(base+"import", file, stdout)
}

func saveState(url, file string, stdout io.Writer) error {
	c := &http.Client{Timeout: 30 * time.Minute}
	resp, err := c.Post(url, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("export: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	// Owner-only, written whole and renamed: the archive can hold secret
	// values, and a half-written one must never sit where a whole one was.
	tmp, err := os.CreateTemp(filepath.Dir(file), ".cloudburrow-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("export: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	m, err := readManifest(tmp.Name())
	if err != nil {
		return fmt.Errorf("the export did not produce a readable archive: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "saved %s\n", file)
	printManifest(stdout, m)
	return nil
}

func loadState(url, file string, stdout io.Writer) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	c := &http.Client{Timeout: 30 * time.Minute}
	resp, err := c.Post(url, "application/gzip", f)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("load %s: %s", file, e.Error)
	}
	var res admin.StateImportResult
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "loaded %s: %s\n", file, strings.Join(res.Loaded, ", "))
	for _, s := range res.NotCaptured {
		fmt.Fprintf(stdout, "  not in the archive: %-14s %s\n", s.Name, s.Reason)
	}
	return nil
}

func readManifest(path string) (admin.Manifest, error) {
	var m admin.Manifest
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return m, err
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return m, err
	}
	if hdr.Name != "manifest.json" {
		return m, errors.New("manifest.json is not the first entry")
	}
	return m, json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m)
}

func printManifest(w io.Writer, m admin.Manifest) {
	for _, s := range m.Services {
		if s.Captured {
			fmt.Fprintf(w, "  captured:     %s\n", s.Name)
		}
	}
	for _, s := range m.Services {
		if !s.Captured {
			fmt.Fprintf(w, "  not captured: %-14s %s\n", s.Name, s.Reason)
		}
	}
	if m.ContainsSecretValues {
		fmt.Fprintln(w, "\n  WARNING: this archive contains secret values in plain form.")
		fmt.Fprintln(w, "  It is readable only by you (0600); treat it as a secret itself.")
	}
}
