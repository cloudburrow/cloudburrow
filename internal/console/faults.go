package console

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Fault injection (#800): the console's screen for /admin/faults.
//
// The console does not hold a way around the admin token (#553). Its own
// guards, the Host check and the same-origin check, stop a web page, but not a
// workload: on Docker Desktop a pod reaches the host's loopback through
// host.docker.internal, a name the Host check accepts, and a client that is
// not a browser sends no fetch metadata and no Origin at all. So these
// endpoints pass the caller's own `Authorization: Bearer` header to the admin
// API, which is called in process (the same handlers the control port
// serves), and the admin API's own token check decides. The page asks the
// developer for the token once per tab; a request without it is refused with
// the admin API's 401, whatever headers it carries.

// SetFaults attaches the admin API's handler for /admin/faults and
// /admin/events, and the path of the instance's admin-token file, which the
// page names when it asks for the token. The path is not the token.
func (s *Server) SetFaults(admin http.Handler, tokenFile string) {
	s.faultsAdmin = admin
	s.faultsTokenFile = tokenFile
}

// FaultEvent is one injected fault, from the recorder's `fault` events, with
// only the fields the page shows.
type FaultEvent struct {
	Time      time.Time `json:"time"`
	Service   string    `json:"service"`
	Method    string    `json:"method"`
	Rule      string    `json:"rule"`
	Code      string    `json:"code,omitempty"`
	LatencyMS string    `json:"latency_ms,omitempty"`
}

// recentFaults bounds the Recent faults section.
const recentFaults = 50

// captured is a response written by the in-process admin handler.
type captured struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *captured) Header() http.Header { return c.header }
func (c *captured) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *captured) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

// adminCall sends one request to the admin API in process, carrying only the
// caller's Authorization header, and returns its status and body.
func (s *Server) adminCall(r *http.Request, method, target string, body []byte) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, target, rdr)
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return http.StatusInternalServerError, b
	}
	if a := r.Header.Get("Authorization"); a != "" {
		req.Header.Set("Authorization", a)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	out := &captured{header: http.Header{}}
	s.faultsAdmin.ServeHTTP(out, req)
	if out.status == 0 {
		out.status = http.StatusOK
	}
	return out.status, out.body.Bytes()
}

// relayAdminError answers with the admin API's own status and message. A
// 401 also names the token file, so the page can say where the token is.
func (s *Server) relayAdminError(w http.ResponseWriter, status int, body []byte) {
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Error == "" {
		parsed.Error = string(bytes.TrimSpace(body))
	}
	out := map[string]any{"error": parsed.Error}
	if status == http.StatusUnauthorized {
		out["token_required"] = true
		if s.faultsTokenFile != "" {
			out["token_file"] = s.faultsTokenFile
		}
	}
	writeJSON(w, status, out)
}

func (s *Server) faultsWired(w http.ResponseWriter) bool {
	if s.faultsAdmin == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "fault injection is not available on this console: it is not attached to an instance's admin API"})
		return false
	}
	return true
}

// handleFaults is GET /api/faults: the rules, what a rule may name, and the
// recent injected faults, each read from the admin API.
func (s *Server) handleFaults(w http.ResponseWriter, r *http.Request) {
	if !s.faultsWired(w) {
		return
	}
	status, body := s.adminCall(r, http.MethodGet, "/admin/faults", nil)
	if status != http.StatusOK {
		s.relayAdminError(w, status, body)
		return
	}
	var list map[string]json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the admin API's fault list is not JSON: " + err.Error()})
		return
	}
	status, body = s.adminCall(r, http.MethodGet, "/admin/events?kind=fault&limit="+strconv.Itoa(recentFaults), nil)
	if status != http.StatusOK {
		s.relayAdminError(w, status, body)
		return
	}
	var events struct {
		Events []struct {
			Time    time.Time         `json:"time"`
			Service string            `json:"service"`
			Target  string            `json:"target"`
			Detail  map[string]string `json:"detail"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the admin API's events are not JSON: " + err.Error()})
		return
	}
	recent := make([]FaultEvent, 0, len(events.Events))
	for _, e := range events.Events {
		// Field by field, so nothing else an event carries reaches the page.
		recent = append(recent, FaultEvent{Time: e.Time, Service: e.Service, Method: e.Target,
			Rule: e.Detail["rule"], Code: e.Detail["code"], LatencyMS: e.Detail["latency_ms"]})
	}
	out := map[string]any{"recent": recent}
	for _, k := range []string{"faults", "interposed", "refused", "codes", "httpStatuses"} {
		if v, ok := list[k]; ok {
			out[k] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAddFault is POST /api/faults: the body is a rule, as POST
// /admin/faults takes it, and the answer is the admin API's.
func (s *Server) handleAddFault(w http.ResponseWriter, r *http.Request) {
	if !s.faultsWired(w) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed rule: " + err.Error()})
		return
	}
	status, out := s.adminCall(r, http.MethodPost, "/admin/faults", body)
	if status != http.StatusCreated {
		s.relayAdminError(w, status, out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// handleDeleteFault is DELETE /api/faults?id= for one rule, or ?all=true for
// every rule. Neither is refused, so a request that lost its id cannot clear
// every rule by accident.
func (s *Server) handleDeleteFault(w http.ResponseWriter, r *http.Request) {
	if !s.faultsWired(w) {
		return
	}
	q := r.URL.Query()
	target := "/admin/faults"
	switch id := q.Get("id"); {
	case id != "":
		target += "?id=" + url.QueryEscape(id)
	case q.Get("all") == "true":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "name the rule to delete with ?id=, or pass ?all=true to delete every rule"})
		return
	}
	status, out := s.adminCall(r, http.MethodDelete, target, nil)
	if status != http.StatusOK {
		s.relayAdminError(w, status, out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}
