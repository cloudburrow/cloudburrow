package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The Instance page (#801): `cloudburrow state save|load`, `reset` and
// `seed`, from the console.
//
// Like the fault screen (#800) and the diagnose download (#802), it holds no
// way around the admin token (#553). The console's Host and same-origin
// guards stop a web page but not a workload: on Docker Desktop a pod reaches
// the host's loopback as host.docker.internal, which the Host check accepts,
// and a client that is not a browser sends no Origin and no fetch metadata.
// So every endpoint here passes the request's own `Authorization: Bearer`
// header to the admin API, called in process, and the admin API's token check
// decides. The page asks for the token once per tab.

// InstanceSource performs the Instance page's actions through the admin API.
// cmd/cloudburrow implements it with the code `state save` and `state load`
// use, so the archive the page downloads is the one the command writes.
// authorization is the request's Authorization header, passed on as it is;
// a refusal is an *InstanceRefusal with the admin API's status and message.
type InstanceSource interface {
	// Info is GET /admin/instance: what reset, seed and the archive accept.
	Info(ctx context.Context, authorization string) (json.RawMessage, error)
	// SaveState exports the archive to a file the caller removes.
	SaveState(ctx context.Context, authorization string) (StateArchive, error)
	// LoadState imports an archive, replacing the state it captures.
	LoadState(ctx context.Context, authorization string, archive io.Reader) (json.RawMessage, error)
	// Reset is POST /admin/reset with q (service, project, reseed).
	Reset(ctx context.Context, authorization string, q url.Values) (json.RawMessage, error)
	// Seed is POST /admin/seed with doc as the body.
	Seed(ctx context.Context, authorization string, doc []byte, ifNotExists bool) (json.RawMessage, error)
}

// StateArchive is an exported archive on disk.
type StateArchive struct {
	// Path is the owner-only file holding it; the caller removes it.
	Path string
	// Name is the file name offered to the browser.
	Name string
}

// InstanceRefusal is the admin API's answer to a request it refused.
type InstanceRefusal struct {
	Status  int
	Message string
	// Body is the admin API's JSON answer, relayed with its fields (a
	// reset's per-component failures, a seed's components seeded so far).
	Body json.RawMessage
}

func (e *InstanceRefusal) Error() string { return e.Message }

// SetInstance attaches the Instance page's source and the path of the
// instance's admin-token file, which the page names when it asks for the
// token. The path is not the token. Without a source the page says the
// console is not attached to an instance.
func (s *Server) SetInstance(src InstanceSource, tokenFile string) {
	s.instanceSrc = src
	s.instanceTokenFile = tokenFile
}

// seedLimit is the largest seed document, as POST /admin/seed reads it.
const seedLimit = 4 << 20

// instanceBudget bounds a save, a load or a reset: `state save` and `state
// load` allow thirty minutes, and a page has less patience than a terminal.
const instanceBudget = 10 * time.Minute

func (s *Server) instanceWired(w http.ResponseWriter) bool {
	if s.instanceSrc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the Instance page is not available on this console: it is not attached to a running instance's admin API"})
		return false
	}
	return true
}

// relayInstanceError answers with the admin API's own status and message,
// and the fields of its answer. A 401 also names the token file.
func (s *Server) relayInstanceError(w http.ResponseWriter, err error, opID string) {
	var refused *InstanceRefusal
	if !errors.As(err, &refused) {
		out := map[string]string{"error": userMessage(err)}
		if opID != "" {
			out["operation"] = opID
		}
		writeJSON(w, http.StatusBadGateway, out)
		return
	}
	out := map[string]any{}
	_ = json.Unmarshal(refused.Body, &out)
	out["error"] = refused.Message
	if refused.Status == http.StatusUnauthorized {
		out["token_required"] = true
		if s.instanceTokenFile != "" {
			out["token_file"] = s.instanceTokenFile
		}
	}
	if opID != "" {
		out["operation"] = opID
	}
	writeJSON(w, refused.Status, out)
}

// instanceAuthorized asks the admin API whether the request's token is
// accepted before an action is recorded, so a request without it is refused
// with the admin API's 401 and leaves no entry in the operations ledger.
func (s *Server) instanceAuthorized(w http.ResponseWriter, r *http.Request) bool {
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()
	if _, err := s.instanceSrc.Info(ctx, r.Header.Get("Authorization")); err != nil {
		s.relayInstanceError(w, err, "")
		return false
	}
	return true
}

// instanceOperation records an action's outcome in the operations ledger,
// never its payload: no archive, no seed document, only what was done.
func (s *Server) instanceOperation(kind, resource, project string) func(err error, detail string) string {
	id := s.logs.StartOperation(kind, resource, project)
	return func(err error, detail string) string {
		if err != nil {
			s.logs.FinishOperation(id, OperationFailed, userMessage(err))
			s.logs.Log(Entry{Severity: SeverityError, Source: "instance", Project: project, Resource: resource,
				OperationID: id, Message: kind + " failed: " + userMessage(err)})
			return id
		}
		s.logs.FinishOperation(id, OperationSucceeded, "")
		s.logs.Log(Entry{Severity: SeverityInfo, Source: "instance", Project: project, Resource: resource,
			OperationID: id, Message: kind + ": " + detail})
		return id
	}
}

// handleInstance is GET /api/instance.
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	if !s.instanceWired(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()
	info, err := s.instanceSrc.Info(ctx, r.Header.Get("Authorization"))
	if err != nil {
		s.relayInstanceError(w, err, "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(info)
}

// handleStateSave is POST /api/instance/save: the archive, as a download.
func (s *Server) handleStateSave(w http.ResponseWriter, r *http.Request) {
	if !s.instanceWired(w) || !s.instanceAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), instanceBudget)
	defer cancel()
	finish := s.instanceOperation("Save state", "", "")
	archive, err := s.instanceSrc.SaveState(ctx, r.Header.Get("Authorization"))
	if err != nil {
		s.relayInstanceError(w, err, finish(err, ""))
		return
	}
	defer os.Remove(archive.Path)
	f, err := os.Open(archive.Path)
	if err != nil {
		s.relayInstanceError(w, err, finish(err, ""))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.relayInstanceError(w, err, finish(err, ""))
		return
	}
	id := finish(nil, archive.Name)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", archive.Name))
	w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
	w.Header().Set("X-Cloudburrow-Operation", id)
	_, _ = io.Copy(w, f)
}

// handleStateLoad is POST /api/instance/load: the body is the archive,
// bounded by the console's upload limit.
func (s *Server) handleStateLoad(w http.ResponseWriter, r *http.Request) {
	if !s.instanceWired(w) || !s.instanceAuthorized(w, r) {
		return
	}
	limit := s.settings.UploadLimit()
	if r.ContentLength > limit {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": errTooLarge{limit}.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), instanceBudget)
	defer cancel()
	finish := s.instanceOperation("Load state", "", "")
	body := &boundedBody{r: http.MaxBytesReader(w, r.Body, limit)}
	res, err := s.instanceSrc.LoadState(ctx, r.Header.Get("Authorization"), body)
	if body.tooLarge {
		err = errTooLarge{limit}
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error(), "operation": finish(err, "")})
		return
	}
	if err != nil {
		s.relayInstanceError(w, err, finish(err, ""))
		return
	}
	var loaded struct {
		Loaded []string `json:"loaded"`
	}
	_ = json.Unmarshal(res, &loaded)
	id := finish(nil, "loaded "+strings.Join(loaded.Loaded, ", "))
	s.logs.NameOperation(id, strings.Join(loaded.Loaded, ", "))
	writeWithOperation(w, res, id)
}

// handleInstanceReset is POST /api/instance/reset?service=&project=&reseed=,
// as POST /admin/reset takes them.
func (s *Server) handleInstanceReset(w http.ResponseWriter, r *http.Request) {
	if !s.instanceWired(w) || !s.instanceAuthorized(w, r) {
		return
	}
	in := r.URL.Query()
	q := url.Values{}
	var services []string
	for _, v := range in["service"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				services = append(services, name)
				q.Add("service", name)
			}
		}
	}
	project := strings.TrimSpace(in.Get("project"))
	if project != "" {
		q.Set("project", project)
	}
	if in.Get("reseed") == "true" {
		q.Set("reseed", "true")
	}
	scope := "every service"
	if len(services) > 0 {
		scope = strings.Join(services, ", ")
	}
	ctx, cancel := context.WithTimeout(r.Context(), instanceBudget)
	defer cancel()
	finish := s.instanceOperation("Reset", scope, project)
	res, err := s.instanceSrc.Reset(ctx, r.Header.Get("Authorization"), q)
	if err != nil {
		s.relayInstanceError(w, err, finish(err, ""))
		return
	}
	var done struct {
		Reset    []string `json:"reset"`
		Reseeded []string `json:"reseeded"`
	}
	_ = json.Unmarshal(res, &done)
	detail := "reset " + strings.Join(done.Reset, ", ")
	if len(done.Reseeded) > 0 {
		detail += "; reseeded " + strings.Join(done.Reseeded, ", ")
	}
	writeWithOperation(w, res, finish(nil, detail))
}

// handleInstanceSeed is POST /api/instance/seed[?ifNotExists=true]: the body
// is a seed document, as POST /admin/seed takes it.
func (s *Server) handleInstanceSeed(w http.ResponseWriter, r *http.Request) {
	if !s.instanceWired(w) || !s.instanceAuthorized(w, r) {
		return
	}
	doc, err := io.ReadAll(http.MaxBytesReader(w, r.Body, seedLimit))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed seed document: " + err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), instanceBudget)
	defer cancel()
	finish := s.instanceOperation("Seed", "", "")
	res, err := s.instanceSrc.Seed(ctx, r.Header.Get("Authorization"), doc, r.URL.Query().Get("ifNotExists") == "true")
	if err != nil {
		s.relayInstanceError(w, err, finish(err, ""))
		return
	}
	var seeded struct {
		Seeded []string `json:"seeded"`
	}
	_ = json.Unmarshal(res, &seeded)
	id := finish(nil, "seeded "+strings.Join(seeded.Seeded, ", "))
	s.logs.NameOperation(id, strings.Join(seeded.Seeded, ", "))
	writeWithOperation(w, res, id)
}

// writeWithOperation answers with the admin API's JSON object and the
// ledger entry's id beside its fields.
func writeWithOperation(w http.ResponseWriter, res json.RawMessage, opID string) {
	out := map[string]any{}
	_ = json.Unmarshal(res, &out)
	out["operation"] = opID
	writeJSON(w, http.StatusOK, out)
}

// boundedBody notes when the upload limit cut an archive short, which the
// admin API can only see as an unreadable archive.
type boundedBody struct {
	r        io.Reader
	tooLarge bool
}

func (b *boundedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		b.tooLarge = true
	}
	return n, err
}
