package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// TestRenamedAIPagesRedirect (#1839): every address the console served
// before Vertex AI became Agent Platform, and Google's own /vertex-ai/
// prefix, answers 301 to its page now, with the query kept.
func TestRenamedAIPagesRedirect(t *testing.T) {
	s := New("", func(context.Context) Status { return Status{} })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for from, to := range map[string]string{
		"/ai/models?project=p":    "/agent-platform/model-garden?project=p",
		"/ai/models/gemma":        "/agent-platform/model-garden",
		"/ai/playground":          "/agent-platform/studio",
		"/ai/embeddings":          "/agent-platform/studio/embeddings",
		"/ai/local-models":        "/agent-platform/model-garden/local-models",
		"/ai/predict?project=p":   "/agent-platform/online-prediction/endpoints?project=p",
		"/vertex-ai":              "/agent-platform/overview",
		"/vertex-ai/model-garden": "/agent-platform/model-garden",
	} {
		resp, err := client.Get(srv.URL + from)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != to {
			t.Errorf("GET %s = %d to %q; want 301 to %q", from, resp.StatusCode, resp.Header.Get("Location"), to)
		}
	}
	resp, err := client.Get(srv.URL + "/agent-platform/model-garden")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the new address answers %d, want the console shell", resp.StatusCode)
	}
}

type gardenView struct {
	Models []struct {
		ID, Local, Unavailable string
		Deployable             bool
		LocalState             *localai.ModelState
	}
	Filters []struct {
		Name   string
		Values []string
	}
}

func modelGarden(t *testing.T, s *Server) gardenView {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	code, body := getJSON(t, srv, "/api/ai/model-garden")
	if code != http.StatusOK {
		t.Fatalf("model garden: %d %s", code, body)
	}
	var v gardenView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestModelGardenDeploysOnlyWhatRunsLocally (#1839): Deploy is offered only
// for a card whose local model is pinned and downloadable here; every other
// card says why not, and with local model management off nothing deploys.
func TestModelGardenDeploysOnlyWhatRunsLocally(t *testing.T) {
	off := modelGarden(t, New("", func(context.Context) Status { return Status{} }))
	if len(off.Models) < 10 {
		t.Fatalf("%d cards", len(off.Models))
	}
	for _, m := range off.Models {
		if m.Deployable || m.Unavailable == "" {
			t.Errorf("with local models off, %s is deployable=%v, reason %q", m.ID, m.Deployable, m.Unavailable)
		}
	}
	names := []string{}
	for _, f := range off.Filters {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "Model collections,Tasks,Providers" {
		t.Errorf("filters %v; want Google's documented Model collections, Tasks and Providers", names)
	}

	s := New("", func(context.Context) Status { return Status{} })
	s.SetLocalModels(&fakeModels{states: []localai.ModelState{
		{ID: "gemma-4-e2b-it-community", Pinned: true, Bytes: 9, State: "absent"},
		{ID: "gemma-3n-e2b-it", State: "not-pinned", DownloadNote: "gated: needs a Hugging Face token"},
	}})
	on := modelGarden(t, s)
	seen := map[string]bool{}
	for _, m := range on.Models {
		seen[m.ID] = true
		switch m.ID {
		case "gemma-4":
			if !m.Deployable || m.LocalState == nil || m.LocalState.Bytes != 9 {
				t.Errorf("gemma-4 runs locally and is not deployable: %+v", m)
			}
		case "gemma-3n":
			if m.Deployable || !strings.Contains(m.Unavailable, "gated") {
				t.Errorf("gated gemma-3n reads deployable=%v %q", m.Deployable, m.Unavailable)
			}
		default:
			if m.Deployable {
				t.Errorf("%s is deployable with no local runtime", m.ID)
			}
			if m.Local == "" && m.Unavailable != NotLocal {
				t.Errorf("%s says %q; want the not-available-locally note", m.ID, m.Unavailable)
			}
		}
	}
	if !seen["gemma-4"] || !seen["gemma-3n"] {
		t.Error("the Gemma cards are missing")
	}
}

// TestAgentGardenLinksEachSampleToItsSource (#1839): every Agent Garden card
// links to a public source repository, and the page says Deploy is
// disabled because Agent Runtime is not served.
func TestAgentGardenLinksEachSampleToItsSource(t *testing.T) {
	s := New("", func(context.Context) Status { return Status{} })
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	code, body := getJSON(t, srv, "/api/ai/agent-garden")
	if code != http.StatusOK || !strings.Contains(body, "reasoningEngines") {
		t.Fatalf("agent garden: %d %s", code, body)
	}
	for _, a := range gardenAgents {
		if !strings.HasPrefix(a.Source, "https://github.com/") || a.Kind == "" || a.Name == "" {
			t.Errorf("agent %+v has no public source", a)
		}
	}
}
