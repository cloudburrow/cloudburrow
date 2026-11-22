package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
)

// Deploying a downloaded Hugging Face model from Model Garden (#1840): the
// Deploy button runs ModelGardenService.Deploy, the Endpoints page shows the
// deployment's progress through to Ready, and its test panel sends
// :predict and chat/completions to it.

// HFLocation is the location the console deploys to, Model Garden's default.
const HFLocation = "us-central1"

// HFDeployer runs ModelGardenService.Deploy for a Hugging Face model,
// returning the operation and the endpoint's names. token is the deploy
// form's Hugging Face access token (a gated model's), which the deployer
// puts in the workload's Secret and nowhere else; runtime is "" (the
// instance's cluster) or modelgarden.RuntimeDocker (a downloaded model).
type HFDeployer interface {
	DeployHF(ctx context.Context, project, location, hfID string, acceptEULA bool, token, runtime string) (op, endpoint string, err error)
}

// SetModelGarden offers Deploy on downloaded Hugging Face models; a nil
// manager offers none.
func (s *Server) SetModelGarden(m *modelgarden.Manager, d HFDeployer, project string) {
	if m == nil || d == nil {
		return
	}
	s.garden, s.hfDeploy, s.hfProject = m, d, project
}

type hfDeploymentView struct {
	modelgarden.Endpoint
	Name  string             `json:"name"`
	Model *modelgarden.Model `json:"model,omitempty"`
}

func (s *Server) handleHFDeployments(w http.ResponseWriter, _ *http.Request) {
	if s.garden == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "endpoints": []any{},
			"note": "Deploying Hugging Face models needs the local AI endpoint: start the instance with a local AI model " +
				"(--local-ai-model or --local-ai-embeddings) or with --services including run."})
		return
	}
	out := []hfDeploymentView{}
	for _, e := range s.garden.AllEndpoints() {
		v := hfDeploymentView{Endpoint: e, Name: e.Name()}
		if e.Deployed != nil {
			if m, err := s.garden.Model(e.Project, e.Location, e.Deployed.ModelID); err == nil {
				v.Model = &m
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "endpoints": out, "project": s.hfProject, "location": HFLocation})
}

func parseEndpointName(name string) (project, location, id string, ok bool) {
	p := strings.Split(name, "/")
	if len(p) != 6 || p[0] != "projects" || p[2] != "locations" || p[4] != "endpoints" {
		return "", "", "", false
	}
	return p[1], p[3], p[5], true
}

func writeAPIError(w http.ResponseWriter, err error) {
	e := apierror.From(err)
	writeJSON(w, e.HTTPStatus(), map[string]string{"error": e.Message})
}

func (s *Server) handleHFDeploymentAction(w http.ResponseWriter, r *http.Request) {
	if s.garden == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Deploying Hugging Face models is not available on this instance"})
		return
	}
	var req struct {
		ID         string `json:"id"`
		AcceptEULA bool   `json:"acceptEula"`
		// Token is the deploy form's Hugging Face access token. It is
		// passed to the deployer and dropped: never logged, kept or echoed.
		Token string `json:"hfToken"`
		// Local deploys a model downloaded to the host (#1840) in Docker.
		Local    bool            `json:"local"`
		Endpoint string          `json:"endpoint"`
		Mode     string          `json:"mode"`
		Body     json.RawMessage `json:"body"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if json.Unmarshal(b, &req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request is not JSON"})
		return
	}
	action := r.PathValue("action")
	if action == "deploy" {
		runtime := ""
		if req.Local {
			runtime = modelgarden.RuntimeDocker
		}
		op, ep, err := s.hfDeploy.DeployHF(r.Context(), s.hfProject, HFLocation, req.ID, req.AcceptEULA, req.Token, runtime)
		req.Token = ""
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"operation": op, "endpoint": ep})
		return
	}
	project, location, id, ok := parseEndpointName(req.Endpoint)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `want {"endpoint": "projects/{p}/locations/{l}/endpoints/{id}"}`})
		return
	}
	switch action {
	case "undeploy":
		if err := s.garden.Undeploy(r.Context(), project, location, id, ""); err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"undeployed": req.Endpoint})
	case "delete":
		// The console's Delete removes the deployment, its endpoint and its
		// model; the downloaded files stay until Delete files.
		e, err := s.garden.Endpoint(project, location, id)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if e.Deployed != nil {
			if err := s.garden.Undeploy(r.Context(), project, location, id, ""); err != nil {
				writeAPIError(w, err)
				return
			}
			if err := s.garden.DeleteModel(project, location, e.Deployed.ModelID); err != nil {
				writeAPIError(w, err)
				return
			}
		}
		if err := s.garden.DeleteEndpoint(project, location, id); err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": req.Endpoint})
	case "test":
		var out []byte
		var err error
		switch req.Mode {
		case "predict":
			out, err = s.garden.Predict(r.Context(), project, location, id, req.Body)
		case "chat":
			err = s.garden.Stream(r.Context(), project, location, id, req.Body, false, func(_ string, data []byte) error {
				out = append(out, data...)
				return nil
			})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `mode is "predict" or "chat"`})
			return
		}
		if err != nil {
			writeAPIError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such action"})
	}
}
