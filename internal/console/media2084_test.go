package console

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The media route plays an audio object, answers a Range request as a
// player seeking sends it, and refuses anything that is not audio (#2084).
func TestMediaPlaysAudioOnly(t *testing.T) {
	p := newObjectProvider()
	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt 0123456789")
	p.objects["bkt/transcriptions/t1/clip.wav"] = fakeObject{wav, "audio/x-wav"}
	p.objects["bkt/clip.flac"] = fakeObject{[]byte("fLaC...."), "application/octet-stream"}
	p.objects["bkt/page.html"] = fakeObject{[]byte("<script>alert(1)</script>"), "text/html"}
	p.objects["bkt/fake.wav"] = fakeObject{[]byte("<script>alert(1)</script>"), "text/html"}
	srv := serve(t, p)
	get := func(u, rng string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+u, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	u := MediaURL("storage", "p1", []string{"bkt", "transcriptions", "t1", "clip.wav"})
	if !strings.HasPrefix(u, "/api/objects/storage/media?") || !strings.Contains(u, "name=transcriptions") {
		t.Fatalf("MediaURL = %s", u)
	}
	resp, body := get(u, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/wav" || string(body) != string(wav) {
		t.Fatalf("play: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("headers: %v", resp.Header)
	}
	resp, body = get(u, "bytes=4-7")
	if resp.StatusCode != http.StatusPartialContent || string(body) != "\x24\x00\x00\x00" {
		t.Errorf("range: %d %q", resp.StatusCode, body)
	}
	resp, _ = get("/api/objects/storage/media?name=bkt&name=clip.flac", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/flac" {
		t.Errorf("octet-stream .flac: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, n := range []string{"page.html", "fake.wav"} {
		resp, _ = get("/api/objects/storage/media?name=bkt&name="+n, "")
		if resp.StatusCode != http.StatusUnsupportedMediaType || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("%s: %d %q", n, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	resp, _ = get("/api/objects/storage/media?name=bkt&name=missing.wav", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing: %d", resp.StatusCode)
	}
}

func TestMediaType(t *testing.T) {
	for _, c := range []struct{ name, ct, want string }{
		{"a.wav", "audio/wav", "audio/wav"}, {"a.mp3", "audio/mp3", "audio/mpeg"}, {"a.ogg", "", "audio/ogg"},
		{"a.wav", "text/html", ""}, {"a.txt", "application/octet-stream", ""}, {"a.webm", "audio/webm;codecs=opus", "audio/webm"},
	} {
		if got := MediaType(c.name, c.ct); got != c.want {
			t.Errorf("MediaType(%q, %q) = %q, want %q", c.name, c.ct, got, c.want)
		}
	}
}
