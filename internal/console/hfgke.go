package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
)

// Model Garden's GKE deployment environment (#2148): the deploy form's
// second choice beside Vertex AI. It lists the project's Kubernetes Engine
// clusters, shows the manifests Model Garden generates for the model (and,
// for a gated model, the hf-secret command), and its Deploy applies them to
// the chosen cluster, then opens Kubernetes Engine's Workloads page, as
// Google's console does.

// GKETarget is a Kubernetes Engine cluster a model can be deployed to.
type GKETarget struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	Status   string `json:"status"`
	// Kubeconfig reaches the cluster; Arch is its nodes' architecture.
	Kubeconfig string `json:"-"`
	Arch       string `json:"-"`
}

// HFGKE lists a project's Kubernetes Engine clusters and applies a model's
// GKE manifests to one (modelgarden.ApplyGKE).
type HFGKE interface {
	GKEClusters(ctx context.Context, project string) []GKETarget
	ApplyGKE(ctx context.Context, t GKETarget, plan modelgarden.GKEPlan, token string) error
}

// SetHFGKE offers the GKE deployment environment; nil offers none.
func (s *Server) SetHFGKE(g HFGKE) { s.hfGKE = g }

func (s *Server) gkeTargets(ctx context.Context) []GKETarget {
	if s.hfGKE == nil {
		return nil
	}
	out := s.hfGKE.GKEClusters(ctx, s.hfProject)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func pickTarget(ts []GKETarget, name, location string) (GKETarget, bool) {
	for _, t := range ts {
		if t.Name == name && (location == "" || t.Location == location) {
			return t, true
		}
	}
	return GKETarget{}, false
}

func (s *Server) handleHFGKEPlan(w http.ResponseWriter, r *http.Request) {
	if s.garden == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Deploying Hugging Face models is not available on this instance"})
		return
	}
	q := r.URL.Query()
	ts := s.gkeTargets(r.Context())
	clusters := []GKETarget{}
	clusters = append(clusters, ts...)
	out := map[string]any{"clusters": clusters, "served": s.hfGKE != nil, "project": s.hfProject}
	arch := "amd64"
	if t, ok := pickTarget(ts, q.Get("cluster"), q.Get("location")); ok && t.Arch != "" {
		arch = t.Arch
	} else if len(ts) > 0 && ts[0].Arch != "" {
		arch = ts[0].Arch
	}
	plan, err := s.garden.PlanGKE(r.Context(), q.Get("id"), arch)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	out["plan"] = plan
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHFGKEDeploy(w http.ResponseWriter, r *http.Request) {
	if s.garden == nil || s.hfGKE == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Deploying to GKE needs the Kubernetes Engine API: start the instance with gke among --services"})
		return
	}
	var req struct {
		ID       string `json:"id"`
		Cluster  string `json:"cluster"`
		Location string `json:"location"`
		// Token is a gated model's access token: it goes into the
		// cluster's hf-secret only, and is never logged, kept or echoed.
		Token      string `json:"hfToken"`
		AcceptEULA bool   `json:"acceptEula"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if json.Unmarshal(b, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request is not JSON"})
		return
	}
	token := req.Token
	req.Token = ""
	t, ok := pickTarget(s.gkeTargets(r.Context()), req.Cluster, req.Location)
	if !ok {
		writeAPIError(w, apierror.NotFound("cluster %s is not one of project %s's Kubernetes Engine clusters", req.Cluster, s.hfProject))
		return
	}
	if t.Status != "RUNNING" {
		writeAPIError(w, apierror.FailedPrecondition("cluster %s is %s, not running", t.Name, t.Status))
		return
	}
	plan, err := s.garden.PlanGKE(r.Context(), req.ID, t.Arch)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if plan.Gated && !req.AcceptEULA {
		writeAPIError(w, apierror.FailedPrecondition("%s is gated on Hugging Face and needs its licence accepted", req.ID))
		return
	}
	if err := s.hfGKE.ApplyGKE(r.Context(), t, plan, token); err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deployment": plan.Name, "cluster": t.Name, "location": t.Location,
		"workloads": "/kubernetes/workloads"})
}
