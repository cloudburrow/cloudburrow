package admin

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// State snapshots (#289): POST /admin/state/export streams a gzipped tar of
// the instance's state; POST /admin/state/import restores one.
//
// The archive's first entry is manifest.json, which names every service,
// captured or not, and why not. Each captured service owns the entries
// under services/<name>/. An import is extracted to a temporary directory
// before anything is touched, so an archive that is unreadable, or whose
// manifest version is unknown, changes nothing, and a service can read its
// entries in any order without holding them in memory.

// StateFormat and StateVersion identify the archive. An importer refuses
// any version it does not know rather than guess at one.
const (
	StateFormat  = "cloudburrow-state"
	StateVersion = 1
)

// Manifest describes an archive.
type Manifest struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	// Producer is the CloudBurrow version that wrote it.
	Producer string    `json:"producer,omitempty"`
	Instance string    `json:"instance,omitempty"`
	Created  time.Time `json:"created"`
	// ContainsSecretValues is true when a captured service holds secrets
	// in plain form. The archive must be handled as a secret itself.
	ContainsSecretValues bool              `json:"contains_secret_values"`
	Services             []ManifestService `json:"services"`
}

// ManifestService is one service's line in the manifest.
type ManifestService struct {
	Name     string `json:"name"`
	Captured bool   `json:"captured"`
	Reason   string `json:"reason,omitempty"` // why it was not captured
}

// EntryWriter adds a file to an archive being exported.
type EntryWriter interface {
	Add(name string, size int64, r io.Reader) error
}

// EntryReader reads a service's files from an archive being imported.
type EntryReader interface {
	Open(name string) (io.ReadCloser, error)
	// List names the entries under a prefix, sorted.
	List(prefix string) []string
}

// Snapshotter captures one service's state and restores it.
type Snapshotter interface {
	Name() string
	// Secret reports whether the captured state holds secret values.
	Secret() bool
	Export(ctx context.Context, w EntryWriter) error
	// Import replaces the service's state with the archive's: whatever the
	// service held before is gone afterwards.
	Import(ctx context.Context, r EntryReader) error
}

// RegisterSnapshotter adds a service to state snapshots, in export order.
func (a *API) RegisterSnapshotter(s ...Snapshotter) { a.snapshots = append(a.snapshots, s...) }

// RegisterNotCaptured records a service snapshots leave out, and why, so
// the manifest and the CLI can say so rather than stay silent about it.
func (a *API) RegisterNotCaptured(name, reason string) {
	a.notCaptured = append(a.notCaptured, ManifestService{Name: name, Reason: reason})
}

// SetStateProducer names what writes archives, for the manifest.
func (a *API) SetStateProducer(producer, instance string) {
	a.producer, a.instance = producer, instance
}

type tarEntries struct {
	tw     *tar.Writer
	prefix string
}

func (t tarEntries) Add(name string, size int64, r io.Reader) error {
	if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("bad entry name %q", name)
	}
	hdr := &tar.Header{Name: t.prefix + name, Mode: 0o600, Size: size, ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
	if err := t.tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(t.tw, r)
	if err == nil && n != size {
		err = fmt.Errorf("%s: wrote %d bytes, declared %d", name, n, size)
	}
	return err
}

func (a *API) manifest() Manifest {
	m := Manifest{Format: StateFormat, Version: StateVersion, Producer: a.producer, Instance: a.instance, Created: time.Now().UTC()}
	for _, s := range a.snapshots {
		m.Services = append(m.Services, ManifestService{Name: s.Name(), Captured: true})
		m.ContainsSecretValues = m.ContainsSecretValues || s.Secret()
	}
	m.Services = append(m.Services, a.notCaptured...)
	sort.Slice(m.Services, func(i, j int) bool { return m.Services[i].Name < m.Services[j].Name })
	return m
}

// handleStateExport streams the archive. Errors after the first byte cannot
// change the status, so a service that fails mid-export truncates the
// archive, which an import then refuses as unreadable rather than loading
// half of it.
func (a *API) handleStateExport(w http.ResponseWriter, r *http.Request) {
	m := a.manifest()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="cloudburrow-state.tar.gz"`)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := (tarEntries{tw: tw}).Add("manifest.json", int64(len(mb)), strings.NewReader(string(mb))); err != nil {
		return
	}
	for _, s := range a.snapshots {
		if err := s.Export(r.Context(), tarEntries{tw: tw, prefix: "services/" + s.Name() + "/"}); err != nil {
			// A trailer that makes the archive invalid, rather than a
			// well-formed archive missing a service.
			_, _ = io.WriteString(w, "\nexport failed: "+err.Error())
			return
		}
	}
	_ = tw.Close()
	_ = gz.Close()
}

type dirEntries struct{ dir, prefix string }

func (d dirEntries) Open(name string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(d.dir, filepath.FromSlash(d.prefix+name)))
}

func (d dirEntries) List(prefix string) []string {
	var out []string
	root := filepath.Join(d.dir, filepath.FromSlash(d.prefix))
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel = filepath.ToSlash(rel); strings.HasPrefix(rel, prefix) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// StateImportResult is the import's answer.
type StateImportResult struct {
	Loaded      []string          `json:"loaded"`
	NotCaptured []ManifestService `json:"not_captured,omitempty"`
}

func (a *API) handleStateImport(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp("", "cloudburrow-state-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.RemoveAll(dir)

	m, err := extractArchive(r.Body, dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error() + "; nothing was loaded"})
		return
	}
	byName := map[string]Snapshotter{}
	for _, s := range a.snapshots {
		byName[s.Name()] = s
	}
	var load []Snapshotter
	res := StateImportResult{Loaded: []string{}}
	for _, ms := range m.Services {
		if !ms.Captured {
			res.NotCaptured = append(res.NotCaptured, ms)
			continue
		}
		s, ok := byName[ms.Name]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf(
				"the archive holds %s, which this instance does not run; nothing was loaded", ms.Name)})
			return
		}
		load = append(load, s)
	}
	for _, s := range load {
		if err := s.Import(r.Context(), dirEntries{dir: dir, prefix: "services/" + s.Name() + "/"}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": fmt.Sprintf("load %s: %v", s.Name(), err), "loaded": res.Loaded})
			return
		}
		res.Loaded = append(res.Loaded, s.Name())
	}
	writeJSON(w, http.StatusOK, res)
}

// extractArchive writes an archive to dir and returns its manifest, which
// must come first and must be a version this reads: an unknown version is
// refused before a byte of state is written anywhere but dir.
func extractArchive(body io.Reader, dir string) (Manifest, error) {
	var m Manifest
	gz, err := gzip.NewReader(body)
	if err != nil {
		return m, fmt.Errorf("not a state archive: %w", err)
	}
	tr := tar.NewReader(gz)
	first := true
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("unreadable archive: %w", err)
		}
		name := path.Clean(hdr.Name)
		if first {
			if name != "manifest.json" {
				return m, errors.New("not a state archive: manifest.json is not its first entry")
			}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
				return m, fmt.Errorf("unreadable manifest: %w", err)
			}
			if m.Format != StateFormat {
				return m, fmt.Errorf("not a state archive: format %q", m.Format)
			}
			if m.Version != StateVersion {
				return m, fmt.Errorf("unknown manifest version %d: this CloudBurrow reads version %d", m.Version, StateVersion)
			}
			first = false
			continue
		}
		if hdr.Typeflag != tar.TypeReg || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "..") {
			return m, fmt.Errorf("unsafe entry %q", hdr.Name)
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return m, err
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return m, err
		}
		_, err = io.Copy(f, tr)
		_ = f.Close()
		if err != nil {
			return m, fmt.Errorf("unreadable archive: %w", err)
		}
	}
	if first {
		return m, errors.New("not a state archive: it is empty")
	}
	return m, nil
}
