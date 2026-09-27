package hostguard

import "testing"

func TestLoopbackOrigin(t *testing.T) {
	t.Parallel()
	for origin, want := range map[string]bool{
		"http://127.0.0.1":            true,
		"http://127.0.0.1:5173":       true,
		"https://127.0.0.1:8443":      true,
		"http://127.1.2.3:80":         true,
		"http://[::1]:3000":           true,
		"http://localhost":            true,
		"http://LocalHost:8080":       true,
		"https://app.localhost:3000":  true,
		"http://localhost.:3000":      true,
		"https://evil.example":        false,
		"null":                        false,
		"":                            false,
		"file://":                     false,
		"ws://localhost:3000":         false,
		"http://localhost.evil.com":   false,
		"http://127.0.0.1.nip.io":     false,
		"http://10.0.0.1:9001":        false,
		"http://[::ffff:10.0.0.1]:80": false,
		"http://0.0.0.0:3000":         false,
	} {
		if got := LoopbackOrigin(origin); got != want {
			t.Errorf("LoopbackOrigin(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestParseOrigin(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://app.example":         "https://app.example",
		"HTTPS://App.Example/":        "https://app.example",
		"https://app.example:443":     "https://app.example",
		"http://app.example:80":       "http://app.example",
		"http://app.example:8080":     "http://app.example:8080",
		"https://app.example:80":      "https://app.example:80",
		"http://[::1]:3000":           "http://[::1]:3000",
		" https://dev.internal:5173 ": "https://dev.internal:5173",
	} {
		got, err := ParseOrigin(in)
		if err != nil || got != want {
			t.Errorf("ParseOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "*", "null", "app.example", "https://", "https://*.example", "https://app.example/x",
		"https://app.example?q", "https://app.example#f", "ftp://app.example", "https://u:p@app.example", "file:///tmp"} {
		if got, err := ParseOrigin(bad); err == nil {
			t.Errorf("ParseOrigin(%q) = %q, want an error", bad, got)
		}
	}
}
