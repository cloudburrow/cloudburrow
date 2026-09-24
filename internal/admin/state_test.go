package admin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// memSnap is a snapshotter over a map, recording whether Import ran.
type memSnap struct {
	name     string
	data     map[string]string
	imported *bool
}

func (m memSnap) Name() string { return m.name }
func (m memSnap) Secret() bool { return m.name == "secretmanager" }
func (m memSnap) Export(_ context.Context, w EntryWriter) error {
	b, _ := json.Marshal(m.data)
	return w.Add("data.json", int64(len(b)), bytes.NewReader(b))
}
func (m memSnap) Import(_ context.Context, r EntryReader) error {
	*m.imported = true
	f, err := r.Open("data.json")
	if err != nil {
		return err
	}
	defer f.Close()
	for k := range m.data {
		delete(m.data, k)
	}
	return json.NewDecoder(f).Decode(&m.data)
}

func stateAPI() (*API, map[string]string, *bool) {
	data := map[string]string{"q1": "queue one"}
	imported := false
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterSnapshotter(memSnap{"tasks", data, &imported}, memSnap{"secretmanager", map[string]string{"s": "v"}, new(bool)})
	a.RegisterNotCaptured("pubsub", "Google's emulator has no export")
	return a, data, &imported
}

func export(t *testing.T, srvURL string) []byte {
	t.Helper()
	resp, err := http.Post(srvURL+"/admin/state/export", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("export %d", resp.StatusCode)
	}
	return b
}

func importArchive(t *testing.T, srvURL string, archive []byte) (int, string) {
	t.Helper()
	resp, err := http.Post(srvURL+"/admin/state/import", "application/gzip", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestStateRoundTripsAndNamesWhatItLeftOut(t *testing.T) {
	a, data, _ := stateAPI()
	srv := serve(a)
	defer srv.Close()
	archive := export(t, srv.URL)

	m, err := firstManifest(archive)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != StateVersion || !m.ContainsSecretValues {
		t.Errorf("manifest %+v", m)
	}
	var left string
	for _, s := range m.Services {
		if !s.Captured {
			left = s.Name + ": " + s.Reason
		}
	}
	if left != "pubsub: Google's emulator has no export" {
		t.Errorf("not-captured line %q", left)
	}

	data["q1"] = "changed"
	data["q2"] = "created after the snapshot"
	code, body := importArchive(t, srv.URL, archive)
	if code != 200 || !strings.Contains(body, `"loaded":["secretmanager","tasks"]`) || !strings.Contains(body, "pubsub") {
		t.Fatalf("import %d %s", code, body)
	}
	if len(data) != 1 || data["q1"] != "queue one" {
		t.Errorf("state after load: %v", data)
	}
}

// rewrite changes the manifest of an archive, keeping the rest.
func rewrite(t *testing.T, archive []byte, edit func(*Manifest)) []byte {
	t.Helper()
	gz, _ := gzip.NewReader(bytes.NewReader(archive))
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gw)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		b, _ := io.ReadAll(tr)
		if hdr.Name == "manifest.json" {
			var m Manifest
			_ = json.Unmarshal(b, &m)
			edit(&m)
			b, _ = json.Marshal(m)
			hdr.Size = int64(len(b))
		}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	_ = gw.Close()
	return out.Bytes()
}

// Refused archives touch nothing: no service's Import runs.
func TestARefusedArchiveLoadsNothing(t *testing.T) {
	a, _, imported := stateAPI()
	srv := serve(a)
	defer srv.Close()
	archive := export(t, srv.URL)
	for name, bad := range map[string][]byte{
		"unknown version": rewrite(t, archive, func(m *Manifest) { m.Version = 99 }),
		"foreign format":  rewrite(t, archive, func(m *Manifest) { m.Format = "something-else" }),
		"a service this instance does not run": rewrite(t, archive, func(m *Manifest) {
			m.Services = append(m.Services, ManifestService{Name: "bigtable", Captured: true})
		}),
		"not an archive": []byte("plain text"),
	} {
		code, body := importArchive(t, srv.URL, bad)
		if code != http.StatusBadRequest || !strings.Contains(body, "nothing was loaded") {
			t.Errorf("%s: %d %s", name, code, body)
		}
		if *imported {
			t.Fatalf("%s: a service was imported from a refused archive", name)
		}
	}
	code, body := importArchive(t, srv.URL, rewrite(t, archive, func(m *Manifest) { m.Version = 2 }))
	if !strings.Contains(body, "unknown manifest version 2") || code != 400 {
		t.Errorf("version 2: %d %s", code, body)
	}
}

func firstManifest(archive []byte) (Manifest, error) {
	var m Manifest
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return m, err
	}
	tr := tar.NewReader(gz)
	if _, err := tr.Next(); err != nil {
		return m, err
	}
	return m, json.NewDecoder(tr).Decode(&m)
}
