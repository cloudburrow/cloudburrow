package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// loadCounter is an InstanceSource whose load reads the archive through and
// counts it, as the admin API's import streams it to disk.
type loadCounter struct{ read int64 }

func (l *loadCounter) Info(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (l *loadCounter) SaveState(context.Context, string) (StateArchive, error) {
	return StateArchive{}, nil
}
func (l *loadCounter) LoadState(_ context.Context, _ string, archive io.Reader) (json.RawMessage, error) {
	n, err := io.Copy(io.Discard, archive)
	l.read = n
	if err != nil {
		return nil, err
	}
	return json.RawMessage(`{"loaded":["storage"]}`), nil
}
func (l *loadCounter) Reset(context.Context, string, url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (l *loadCounter) Seed(context.Context, string, []byte, bool) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

// TestStateLoadIsNotHeldToTheUploadLimit (#1250): Save state captures the
// whole instance, so an archive larger than the upload limit in Settings
// loads, read through to the restore; past the state load limit it is
// refused with a 413 that names the limit and `cloudburrow state load`,
// whether the request says its length or not.
func TestStateLoadIsNotHeldToTheUploadLimit(t *testing.T) {
	src := &loadCounter{}
	c := New("127.0.0.1:0", nil)
	c.SetInstance(src, "")
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)

	post := func(body io.Reader) (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/instance/load", "application/gzip", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	size := DefaultUploadLimit + 8<<20
	if code, body := post(bytes.NewReader(make([]byte, size))); code != http.StatusOK || !strings.Contains(body, `"loaded":["storage"]`) {
		t.Fatalf("load an archive over the default upload limit = %d %s", code, body)
	}
	if src.read != size {
		t.Errorf("the restore read %d bytes of %d", src.read, size)
	}

	c.stateLoadLimit = 1 << 20
	over := make([]byte, 2<<20)
	for name, body := range map[string]io.Reader{
		"with its length": bytes.NewReader(over),
		"chunked":         io.MultiReader(bytes.NewReader(over)),
	} {
		code, msg := post(body)
		if code != http.StatusRequestEntityTooLarge || !strings.Contains(msg, "state load limit of 1 MiB") ||
			!strings.Contains(msg, "cloudburrow state load") {
			t.Errorf("load an archive past the limit, %s = %d %s", name, code, msg)
		}
	}
	if StateLoadLimit <= DefaultUploadLimit {
		t.Errorf("StateLoadLimit %d is not above the default upload limit", StateLoadLimit)
	}
}
