package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// fakePostgres stands in for the server's pod: it answers the statements the
// snapshotter sends through psql and keeps each database's "dump" as bytes.
type fakePostgres struct {
	dbs   map[string]string
	calls []string
}

func (f *fakePostgres) exec(_ context.Context, stdin io.Reader, stdout io.Writer, argv ...string) error {
	f.calls = append(f.calls, strings.Join(argv, " "))
	arg := func(flag string) string {
		for i, a := range argv {
			if a == flag && i+1 < len(argv) {
				return argv[i+1]
			}
		}
		return ""
	}
	switch argv[0] {
	case "psql":
		stmt := arg("-c")
		switch {
		case strings.HasPrefix(stmt, "SELECT datname"):
			var names []string
			for n := range f.dbs {
				names = append(names, n)
			}
			sort.Strings(names)
			_, _ = io.WriteString(stdout, strings.Join(names, "\n")+"\n")
		case strings.HasPrefix(stmt, "DROP DATABASE "):
			delete(f.dbs, unquotePGIdent(strings.TrimSuffix(strings.TrimPrefix(stmt, "DROP DATABASE "), " WITH (FORCE)")))
		case strings.HasPrefix(stmt, "CREATE DATABASE "):
			f.dbs[unquotePGIdent(strings.TrimPrefix(stmt, "CREATE DATABASE "))] = ""
		}
	case "pg_dump":
		_, _ = io.WriteString(stdout, f.dbs[arg("-d")])
	case "pg_restore":
		b, _ := io.ReadAll(stdin)
		f.dbs[arg("-d")] = string(b)
	}
	return nil
}

func unquotePGIdent(s string) string {
	return strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`), `""`, `"`)
}

func exportImport(t *testing.T, s admin.Snapshotter) (func() []byte, func([]byte)) {
	t.Helper()
	api := admin.NewAPI(admin.NewRecorder(10, nil))
	api.RegisterSnapshotter(s)
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	export := func() []byte {
		resp, err := http.Post(srv.URL+"/admin/state/export", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	load := func(archive []byte) {
		resp, err := http.Post(srv.URL+"/admin/state/import", "application/gzip", bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 {
			t.Fatalf("import %d %s", resp.StatusCode, b)
		}
	}
	return export, load
}

// A load replaces the server's databases with the archive's: one created
// since the snapshot is gone, a dropped one is back, and each holds exactly
// what was dumped.
func TestPostgresSnapshotReplacesTheDatabases(t *testing.T) {
	pg := &fakePostgres{dbs: map[string]string{"cloudburrow": "rows-v1", `odd "name"`: "other"}}
	export, load := exportImport(t, &postgresSnapshotter{exec: pg.exec})
	archive := export()
	before := map[string]string{"cloudburrow": "rows-v1", `odd "name"`: "other"}

	pg.dbs["cloudburrow"] = "rows-v2"
	pg.dbs["later"] = "made after the snapshot"
	delete(pg.dbs, `odd "name"`)

	load(archive)
	if !reflect.DeepEqual(pg.dbs, before) {
		t.Errorf("after the load the databases are %v, want %v", pg.dbs, before)
	}
	for _, c := range pg.calls {
		if strings.HasPrefix(c, "pg_restore") && !strings.Contains(c, "--exit-on-error") {
			t.Errorf("pg_restore without --exit-on-error would load half a dump: %s", c)
		}
	}
}

// An archive without the default database still leaves one, empty, so an
// application finds what a fresh instance has.
func TestPostgresSnapshotRecreatesTheDefaultDatabase(t *testing.T) {
	pg := &fakePostgres{dbs: map[string]string{"only": "x"}}
	export, load := exportImport(t, &postgresSnapshotter{exec: pg.exec})
	archive := export()
	pg.dbs["cloudburrow"] = "y"
	load(archive)
	if want := map[string]string{"only": "x", "cloudburrow": ""}; !reflect.DeepEqual(pg.dbs, want) {
		t.Errorf("databases %v, want %v", pg.dbs, want)
	}
}

// The archive holds what pg_dump wrote and nothing else: the kubeconfig is
// passed to kubectl as a flag and never read into it. Driven through the real
// kubectl invocation, with a stand-in kubectl on PATH.
func TestPostgresSnapshotArchiveHoldsNoClusterCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in kubectl is a shell script")
	}
	const marker = "client-key-data: NEVER-IN-AN-ARCHIVE"
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = dir
	kubeconfig := cfg.KubeconfigPath()
	if err := os.MkdirAll(filepath.Dir(kubeconfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nusers:\n- user:\n    "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// Records its arguments, then answers as the pod would.
	script := `#!/bin/sh
echo "$@" >> "` + filepath.Join(dir, "argv") + `"
while [ "$1" != "--" ]; do shift; done; shift
case "$1" in
  psql) case "$*" in *"SELECT datname"*) echo cloudburrow ;; esac ;;
  pg_dump) printf 'PGDMP fake dump' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	export, _ := exportImport(t, newPostgresSnapshotter(cfg))
	archive := export()

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("not an archive: %v\n%s", err, archive)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		b, _ := io.ReadAll(tr)
		if strings.Contains(string(b), "NEVER-IN-AN-ARCHIVE") || strings.Contains(string(b), kubeconfig) {
			t.Errorf("%s holds the kubeconfig or its path", hdr.Name)
		}
		if hdr.Name == "services/cloudsql/databases/000.dump" && string(b) != "PGDMP fake dump" {
			t.Errorf("the dump entry is %q, want pg_dump's output", b)
		}
	}
	want := []string{"manifest.json", "services/cloudsql/databases.json", "services/cloudsql/databases/000.dump"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("archive entries %v, want %v", names, want)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if !strings.Contains(string(argv), "--kubeconfig "+kubeconfig+" -n "+cfg.Cluster.Namespace+" exec deploy/cloudsql -- pg_dump") {
		t.Errorf("kubectl was not pointed at the instance's own kubeconfig and pod:\n%s", argv)
	}
}

// An archive naming the maintenance or a template database is refused before
// anything is dropped.
func TestPostgresSnapshotRefusesSystemDatabases(t *testing.T) {
	for _, name := range []string{"postgres", "template1", ""} {
		target := &fakePostgres{dbs: map[string]string{"cloudburrow": "keep"}}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, pgDatabasesEntry), []byte(`["`+name+`"]`), 0o600); err != nil {
			t.Fatal(err)
		}
		err := (&postgresSnapshotter{exec: target.exec}).Import(context.Background(), dirReader(dir))
		if err == nil || target.dbs["cloudburrow"] != "keep" {
			t.Errorf("%q: import returned %v and left %v; want a refusal that changes nothing", name, err, target.dbs)
		}
	}
}

type dirReader string

func (d dirReader) Open(name string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(string(d), filepath.FromSlash(name)))
}
func (d dirReader) List(string) []string { return nil }
