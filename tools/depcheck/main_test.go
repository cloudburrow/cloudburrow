package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tag of v1.56.1 and an inventory entry of 1.56.1 are the same release.
// Reporting that as an update would train a reader to ignore the output.
func TestNormalizeIgnoresTheVPrefix(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{"v1.56.1", "1.56.1"},
		{"1.56.1", "1.56.1"},
		{" v0.33.0 ", "0.33.0"},
	}
	for _, p := range pairs {
		if got := normalize(p[0]); got != p[1] {
			t.Errorf("normalize(%q) = %q, want %q", p[0], got, p[1])
		}
	}
	if normalize("v1.56.1") == normalize("v1.57.0") {
		t.Error("normalize collapsed genuinely different versions")
	}
}

// fakeGitHub serves /repos/<owner>/<repo>/releases/latest from tags, answers
// 404 for a repository not in tags and 500 for "broken/repo". The tests never
// reach the network.
func fakeGitHub(t *testing.T, tags map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/releases/latest")
		switch {
		case !ok:
			http.Error(w, "unexpected path", http.StatusBadRequest)
		case repo == "broken/repo":
			http.Error(w, "upstream down", http.StatusInternalServerError)
		case tags[repo] == "":
			http.NotFound(w, r)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tags[repo]})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func byName(results []result) map[string]result {
	m := map[string]result{}
	for _, r := range results {
		m[r.Group+"/"+r.Name] = r
	}
	return m
}

// A component whose source is not on GitHub and that records no skipDiscovery
// reason used to be dropped with a bare continue, so it appeared nowhere in
// the output (#703). It is now reported as skipped, with a reason naming what
// depcheck cannot read.
func TestNonGitHubComponentWithoutSkipDiscoveryIsReported(t *testing.T) {
	t.Parallel()
	inv := []byte(`{"components": {"emulatorBackends": {
		"$comment": "not a component",
		"pubsubEmulator": {"version": "0.8.35", "updateFeed": "gcloud-component-snapshot"},
		"cliImage": {"version": "585.0.0-emulators", "image": "gcr.io/example/cli", "updateFeed": "gcr-tag-list"}
	}}}`)
	srv := fakeGitHub(t, nil)
	results, err := check(inv, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(results), results)
	}
	got := byName(results)
	for name, feed := range map[string]string{
		"emulatorBackends/pubsubEmulator": "gcloud-component-snapshot",
		"emulatorBackends/cliImage":       "gcr-tag-list",
	} {
		r, ok := got[name]
		if !ok {
			t.Errorf("%s is missing from the results", name)
			continue
		}
		if r.Status != statusSkipped || !strings.Contains(r.Detail, feed) {
			t.Errorf("%s = %s %q, want skipped with a reason naming %s", name, r.Status, r.Detail, feed)
		}
	}

	var out, errOut bytes.Buffer
	report(&out, &errOut, results)
	for name := range got {
		if !strings.Contains(out.String(), name) {
			t.Errorf("report does not mention %s:\n%s", name, out.String())
		}
	}
}

// Each GitHub outcome maps to one status, and a failed request is unreachable,
// never current.
func TestEveryOutcomeHasAStatus(t *testing.T) {
	t.Parallel()
	inv := []byte(`{"components": {"g": {
		"newer":     {"version": "1.0.0", "source": "https://github.com/o/newer"},
		"same":      {"version": "1.2.0", "source": "https://github.com/o/same/"},
		"down":      {"version": "1.0.0", "source": "https://github.com/broken/repo"},
		"noRelease": {"version": "1.0.0", "source": "https://github.com/o/none"},
		"declared":  {"version": "1.0.0", "source": "https://github.com/o/newer", "skipDiscovery": "tracks another project's version"},
		"notRepo":   {"version": "1.0.0", "source": "https://github.com/o"},
		"noVersion": {"source": "https://github.com/o/newer"}
	}}}`)
	srv := fakeGitHub(t, map[string]string{"o/newer": "v2.0.0", "o/same": "v1.2.0"})
	results, err := check(inv, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]status{
		"g/newer":     statusCandidate,
		"g/same":      statusCurrent,
		"g/down":      statusUnreachable,
		"g/noRelease": statusSkipped,
		"g/declared":  statusSkipped,
		"g/notRepo":   statusSkipped,
		"g/noVersion": statusSkipped,
	}
	got := byName(results)
	if len(got) != len(want) {
		t.Errorf("got %d results, want %d: %+v", len(got), len(want), results)
	}
	for name, st := range want {
		r := got[name]
		if r.Status != st {
			t.Errorf("%s = %q (%s), want %q", name, r.Status, r.Detail, st)
		}
		if r.Status == statusSkipped && r.Detail == "" {
			t.Errorf("%s is skipped without a reason", name)
		}
	}
	if d := got["g/declared"].Detail; d != "tracks another project's version" {
		t.Errorf("declared skip reason = %q, want the inventory's", d)
	}
}

// Every component in the real inventory is accounted for: the report has one
// status per component object, whatever the upstream answers.
func TestEveryInventoryComponentIsReported(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	var want []string
	for g, names := range inv.Components {
		if strings.HasPrefix(g, "$") {
			continue
		}
		for n, v := range names {
			var c component
			if strings.HasPrefix(n, "$") || json.Unmarshal(v, &c) != nil {
				continue
			}
			want = append(want, g+"/"+n)
		}
	}

	srv := fakeGitHub(t, nil)
	results, err := check(raw, srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(results)
	if len(results) != len(want) {
		t.Errorf("got %d results for %d components", len(results), len(want))
	}
	for _, name := range want {
		r, ok := got[name]
		if !ok {
			t.Errorf("%s is not reported", name)
			continue
		}
		if r.Status == statusSkipped && r.Detail == "" {
			t.Errorf("%s is skipped without a reason", name)
		}
	}
}

// The feeds depcheck cannot read carry a reviewed skipDiscovery reason in the
// inventory itself, rather than depcheck's generic "no discovery" note.
func TestUnreadFeedsDeclareSkipDiscovery(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inv inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	for g, names := range inv.Components {
		for n, v := range names {
			var c component
			if strings.HasPrefix(n, "$") || json.Unmarshal(v, &c) != nil {
				continue
			}
			switch c.UpdateFeed {
			case "gcr-tag-list", "gcloud-component-snapshot", "go-release-feed", "go-module-proxy":
				if c.SkipDiscovery == "" {
					t.Errorf("%s/%s uses updateFeed %q, which depcheck does not read, and has no skipDiscovery reason", g, n, c.UpdateFeed)
				}
			}
		}
	}
}
