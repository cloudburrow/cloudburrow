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
		return saveState(cfg, base+"export", file, stdout)
	}
	return loadState(cfg, base+"import", file, stdout)
}

func saveState(cfg config.Config, url, file string, stdout io.Writer) error {
	req, err := adminRequest(cfg, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	tmp, m, err := exportStateArchive(&http.Client{Timeout: 30 * time.Minute}, req, filepath.Dir(file))
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, file); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "saved %s\n", file)
	printManifest(stdout, m)
	return nil
}

// stateRefused is the admin API's refusal of an export or an import, with
// its own status and message.
type stateRefused struct {
	status  int
	message string
}

func (e *stateRefused) Error() string { return e.message }

// exportStateArchive sends req, a POST /admin/state/export, with c, and
// writes the archive to a new owner-only file in dir, which it returns with
// the archive's manifest. The file is written whole and its manifest read
// before it is handed back, so neither `state save` nor the console's Save
// state (#801) ever gives out an archive an import would refuse as
// unreadable. The caller removes the file.
func exportStateArchive(c *http.Client, req *http.Request, dir string) (string, admin.Manifest, error) {
	resp, err := c.Do(req)
	if err != nil {
		return "", admin.Manifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", admin.Manifest{}, &stateRefused{status: resp.StatusCode,
			message: fmt.Sprintf("export: %s: %s", resp.Status, adminError(resp.StatusCode, b))}
	}
	// Owner-only, written whole and renamed by the caller: the archive can
	// hold secret values, and a half-written one must never sit where a
	// whole one was.
	tmp, err := os.CreateTemp(dir, ".cloudburrow-state-*")
	if err != nil {
		return "", admin.Manifest{}, err
	}
	fail := func(err error) (string, admin.Manifest, error) {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", admin.Manifest{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return fail(fmt.Errorf("export: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	m, err := readManifest(tmp.Name())
	if err != nil {
		return fail(fmt.Errorf("the export did not produce a readable archive: %w", err))
	}
	return tmp.Name(), m, nil
}

func loadState(cfg config.Config, url, file string, stdout io.Writer) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := adminRequest(cfg, http.MethodPost, url, f)
	if err != nil {
		return err
	}
	res, err := importStateArchive(&http.Client{Timeout: 30 * time.Minute}, req)
	if err != nil {
		var refused *stateRefused
		if errors.As(err, &refused) {
			return fmt.Errorf("load %s: %s", file, refused.message)
		}
		return err
	}
	fmt.Fprintf(stdout, "loaded %s: %s\n", file, strings.Join(res.Loaded, ", "))
	for _, s := range res.NotCaptured {
		fmt.Fprintf(stdout, "  not in the archive: %-14s %s\n", s.Name, s.Reason)
	}
	return nil
}

// importStateArchive sends req, a POST /admin/state/import whose body is
// the archive, with c, and returns what the admin API loaded; a refusal is a
// *stateRefused with its message. `state load` and the console's Load state
// (#801) both call it.
func importStateArchive(c *http.Client, req *http.Request) (admin.StateImportResult, error) {
	var res admin.StateImportResult
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return res, &stateRefused{status: resp.StatusCode, message: adminError(resp.StatusCode, body)}
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return res, err
	}
	return res, nil
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
