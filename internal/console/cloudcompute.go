package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// Settings > Compute and Compute capacity (#2209 phases 2 and 3): connect an
// AWS account or an Azure subscription, create the VM that runs the
// instance's compute there, then resize, start, stop and delete it.
//
// Every endpoint needs the instance's admin token, as the Instance page's
// do: a workload in the cluster can reach the console, and these act on the
// user's own cloud account with credentials from the OS keychain. The
// console holds no credential itself and never answers with one.

// ComputeAuthorizer accepts or refuses a request's Authorization header; a
// refusal is an *InstanceRefusal (401 asks for the token).
type ComputeAuthorizer func(ctx context.Context, authorization string) error

// SetCompute attaches bring your own cloud. instance is the name the Create
// wizard offers; authorize checks the admin token. Without a manager the
// pages say the console is not attached to one.
func (s *Server) SetCloudCompute(m *byoc.Manager, instance string, authorize ComputeAuthorizer) {
	s.cloud, s.cloudInstance, s.cloudAuth = m, instance, authorize
}

// computeBudget bounds a request that reads the cloud (regions, prices,
// the permission check); long actions run as operations.
const computeBudget = 60 * time.Second

func (s *Server) computeReady(w http.ResponseWriter, r *http.Request) bool {
	if s.cloud == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "Compute is not available on this console: it is not attached to a running instance"})
		return false
	}
	authorize := s.cloudAuth
	if authorize == nil && s.instanceSrc != nil {
		authorize = func(ctx context.Context, authz string) error {
			_, err := s.instanceSrc.Info(ctx, authz)
			return err
		}
	}
	if authorize == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Compute needs the admin API to check the admin token"})
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()
	if err := authorize(ctx, r.Header.Get("Authorization")); err != nil {
		s.relayInstanceError(w, err, "")
		return false
	}
	return true
}

// computeError answers with the status a byoc error deserves and its kind.
func computeError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, byoc.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, byoc.ErrUnsupported):
		status = http.StatusBadRequest
	}
	switch byoc.ErrorKind(err) {
	case "permission":
		status = http.StatusForbidden
	case "quota":
		status = http.StatusTooManyRequests
	case "auth":
		status = http.StatusUnauthorized
	}
	out := map[string]string{"error": userMessage(err)}
	if k := byoc.ErrorKind(err); k != "" {
		out["error_kind"] = k
	}
	writeJSON(w, status, out)
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func readJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		badRequest(w, "malformed request: "+err.Error())
		return false
	}
	return true
}

// computeLog records an action in the operations ledger: what was done and
// to what, never a credential.
func (s *Server) computeLog(kind, resource string, err error) {
	id := s.logs.StartOperation(kind, resource, "")
	if err != nil {
		s.logs.FinishOperation(id, OperationFailed, userMessage(err))
		s.logs.Log(Entry{Severity: SeverityError, Source: "compute", Resource: resource, OperationID: id, Message: kind + " failed: " + userMessage(err)})
		return
	}
	s.logs.FinishOperation(id, OperationSucceeded, "")
	s.logs.Log(Entry{Severity: SeverityInfo, Source: "compute", Resource: resource, OperationID: id, Message: kind})
}

// computeOverview is GET /api/compute.
type computeOverview struct {
	Clouds      []byoc.Cloud      `json:"clouds"`
	Connections []byoc.Connection `json:"connections"`
	Machines    []byoc.Machine    `json:"machines"`
	Operations  []byoc.Operation  `json:"operations"`
	Instance    string            `json:"instance"`
	Presets     []byoc.Preset     `json:"presets"`
	Engine      byoc.RemoteStatus `json:"engine"`
}

func (s *Server) handleComputeOverview(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	conns, err := s.cloud.Connections()
	if err != nil {
		computeError(w, err)
		return
	}
	machines, err := s.cloud.Machines()
	if err != nil {
		computeError(w, err)
		return
	}
	if conns == nil {
		conns = []byoc.Connection{}
	}
	if machines == nil {
		machines = []byoc.Machine{}
	}
	ops := s.cloud.Operations()
	if ops == nil {
		ops = []byoc.Operation{}
	}
	eng, _ := s.cloud.Engine().Status(r.Context(), s.cloudInstance)
	writeJSON(w, http.StatusOK, computeOverview{Clouds: s.cloud.Clouds(), Connections: conns, Machines: machines,
		Operations: ops, Instance: s.cloudInstance, Presets: byoc.Presets, Engine: eng})
}

func (s *Server) handleComputeSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	var in struct {
		Cloud    string `json:"cloud"`
		StartURL string `json:"start_url"`
		Region   string `json:"region"`
		Tenant   string `json:"tenant"`
	}
	if !readJSONBody(w, r, &in) {
		return
	}
	c, err := byoc.ParseCloud(in.Cloud)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if c == byoc.AWS {
		if !strings.HasPrefix(in.StartURL, "https://") {
			badRequest(w, "the AWS access portal URL starts with https:// (IAM Identity Center > Settings > AWS access portal URL)")
			return
		}
		if in.Region == "" {
			badRequest(w, "name the region IAM Identity Center is in")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
	defer cancel()
	st, err := s.cloud.BeginSignIn(ctx, c, byoc.SignInOptions{StartURL: in.StartURL, Region: in.Region, Tenant: in.Tenant})
	if err != nil {
		s.computeLog("Connect "+c.Title(), "", err)
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleComputeSignInStatus(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	st, err := s.cloud.SignInStatusOf(r.PathValue("id"))
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleComputeSignInCancel(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	s.cloud.CancelSignIn(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

func (s *Server) handleComputeChoose(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	var in struct {
		Choice string `json:"choice"`
	}
	if !readJSONBody(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
	defer cancel()
	conn, err := s.cloud.ChooseAccount(ctx, r.PathValue("id"), in.Choice)
	s.computeLog("Connect cloud account", in.Choice, err)
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

func (s *Server) handleComputeAccessKey(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	var in struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SessionToken    string `json:"session_token"`
	}
	if !readJSONBody(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.AccessKeyID) == "" || strings.TrimSpace(in.SecretAccessKey) == "" {
		badRequest(w, "paste both the access key ID and the secret access key")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
	defer cancel()
	conn, err := s.cloud.ConnectKey(ctx, byoc.AWS, byoc.AccessKey{AccessKeyID: strings.TrimSpace(in.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(in.SecretAccessKey), SessionToken: strings.TrimSpace(in.SessionToken)})
	s.computeLog("Connect AWS with an access key", conn.ID, err)
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

func (s *Server) handleComputeDisconnect(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	id := r.PathValue("id")
	err := s.cloud.Disconnect(id, r.URL.Query().Get("force") == "true")
	s.computeLog("Disconnect", id, err)
	if err != nil {
		if errors.Is(err, byoc.ErrNotFound) {
			computeError(w, err)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"disconnected": true})
}

func (s *Server) handleComputeRegions(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
	defer cancel()
	rc, err := s.cloud.Regions(ctx, r.PathValue("id"))
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rc)
}

func (s *Server) handleComputeSizes(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	region := r.URL.Query().Get("region")
	if region == "" {
		badRequest(w, "name a region")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
	defer cancel()
	sizes, err := s.cloud.Sizes(ctx, r.PathValue("id"), region)
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sizes": sizes, "region": region})
}

func (s *Server) handleComputePermissions(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
	defer cancel()
	rep, err := s.cloud.CheckPermissions(ctx, r.PathValue("id"), r.URL.Query().Get("region"))
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": rep, "summary": rep.Summary(), "ok": rep.OK()})
}

func (s *Server) handleComputeCreate(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	var req byoc.CreateRequest
	if !readJSONBody(w, r, &req) {
		return
	}
	op, err := s.cloud.Create(r.Context(), req, false, nil)
	s.computeLog("Create compute VM", req.Instance, err)
	if err != nil {
		if errors.Is(err, byoc.ErrNotFound) {
			computeError(w, err)
			return
		}
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) handleComputeMachine(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	instance := r.PathValue("instance")
	if r.URL.Query().Get("refresh") == "true" {
		ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
		defer cancel()
		mc, err := s.cloud.Refresh(ctx, instance)
		if err != nil && !errors.Is(err, byoc.ErrNotFound) && mc.Instance != "" {
			writeJSON(w, http.StatusOK, map[string]any{"machine": mc, "refresh_error": userMessage(err)})
			return
		}
		if err != nil {
			computeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"machine": mc})
		return
	}
	mc, err := s.cloud.Machine(instance)
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"machine": mc})
}

func (s *Server) handleComputeAction(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	instance, action := r.PathValue("instance"), r.PathValue("action")
	var (
		op  byoc.Operation
		err error
	)
	switch action {
	case "scale":
		var in struct {
			Preset string `json:"preset"`
			Size   string `json:"size"`
		}
		if !readJSONBody(w, r, &in) {
			return
		}
		var p byoc.Preset
		if in.Size == "" {
			if p, err = byoc.ParsePreset(in.Preset); err != nil {
				badRequest(w, err.Error())
				return
			}
		}
		op, err = s.cloud.Scale(r.Context(), instance, p, in.Size, false, nil)
	case "start":
		op, err = s.cloud.Start(r.Context(), instance, false, nil)
	case "stop":
		op, err = s.cloud.Stop(r.Context(), instance, false, nil)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no action " + action})
		return
	}
	s.computeLog(strings.ToUpper(action[:1])+action[1:]+" compute VM", instance, err)
	if err != nil {
		if errors.Is(err, byoc.ErrNotFound) {
			computeError(w, err)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) handleComputeDelete(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	instance := r.PathValue("instance")
	op, err := s.cloud.Delete(r.Context(), instance, false, nil)
	s.computeLog("Delete compute VM", instance, err)
	if err != nil {
		if errors.Is(err, byoc.ErrNotFound) {
			computeError(w, err)
			return
		}
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) handleComputeResources(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), computeBudget)
	defer cancel()
	rs, err := s.cloud.Resources(ctx, r.PathValue("instance"))
	if err != nil {
		computeError(w, err)
		return
	}
	if rs == nil {
		rs = []byoc.Resource{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": rs, "tag": byoc.TagKey + "=" + r.PathValue("instance")})
}

func (s *Server) handleComputeOperation(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	op, err := s.cloud.Operation(r.PathValue("id"))
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) cloudRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/compute", s.handleComputeOverview)
	mux.HandleFunc("POST /api/compute/signin", s.handleComputeSignIn)
	mux.HandleFunc("GET /api/compute/signin/{id}", s.handleComputeSignInStatus)
	mux.HandleFunc("DELETE /api/compute/signin/{id}", s.handleComputeSignInCancel)
	mux.HandleFunc("POST /api/compute/signin/{id}/choose", s.handleComputeChoose)
	mux.HandleFunc("POST /api/compute/accesskey", s.handleComputeAccessKey)
	mux.HandleFunc("DELETE /api/compute/connections/{id}", s.handleComputeDisconnect)
	mux.HandleFunc("GET /api/compute/connections/{id}/regions", s.handleComputeRegions)
	mux.HandleFunc("GET /api/compute/connections/{id}/sizes", s.handleComputeSizes)
	mux.HandleFunc("GET /api/compute/connections/{id}/permissions", s.handleComputePermissions)
	mux.HandleFunc("POST /api/compute/machines", s.handleComputeCreate)
	mux.HandleFunc("GET /api/compute/machines/{instance}", s.handleComputeMachine)
	mux.HandleFunc("DELETE /api/compute/machines/{instance}", s.handleComputeDelete)
	mux.HandleFunc("POST /api/compute/machines/{instance}/{action}", s.handleComputeAction)
	mux.HandleFunc("GET /api/compute/machines/{instance}/resources", s.handleComputeResources)
	mux.HandleFunc("GET /api/compute/operations/{id}", s.handleComputeOperation)
}
