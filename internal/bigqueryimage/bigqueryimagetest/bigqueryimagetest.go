// Package bigqueryimagetest gives tests stand-ins for the BigQuery emulator
// builds the CLI embeds (internal/bigqueryimage), so a test behaves the same
// whether or not `make bigquery-binaries` has run.
package bigqueryimagetest

import (
	"bytes"
	"compress/gzip"
	"testing"
	"testing/fstest"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryimage"
	"github.com/cloudburrow/cloudburrow/internal/storageimage/storageimagetest"
)

// Gzip is b compressed, as the embedded files are.
func Gzip(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// FS is the embedded directory as a build with files would have it: the
// README, and each named file with its content, which is stored as given.
func FS(files map[string][]byte) fstest.MapFS {
	fsys := fstest.MapFS{"bin/README.md": {Data: []byte("build outputs\n")}}
	for name, data := range files {
		fsys["bin/"+name] = &fstest.MapFile{Data: data}
	}
	return fsys
}

// Files are stand-ins for every architecture's build and the licence
// bundle, compressed: an ELF header and nothing else.
func Files() map[string][]byte {
	files := map[string][]byte{"bigquery-emulator-licenses.txt.gz": Gzip([]byte("licences\n"))}
	for _, a := range bigqueryimage.Arches {
		files["bigquery-emulator-linux-"+a+".gz"] = Gzip(storageimagetest.ELF(a))
	}
	return files
}

// Present embeds a stand-in for every architecture until the test ends.
func Present(t testing.TB) {
	t.Helper()
	Use(t, FS(Files()))
}

// Missing embeds what a plain `go build` does, the README alone, until the
// test ends.
func Missing(t testing.TB) {
	t.Helper()
	Use(t, FS(nil))
}

// Use makes bigqueryimage read fsys until the test ends.
func Use(t testing.TB, fsys fstest.MapFS) {
	t.Helper()
	t.Cleanup(bigqueryimage.UseBinaries(fsys))
}
