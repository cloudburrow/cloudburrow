package dataproc

// The kernel gateway (#2162): Jupyter kernels on Serverless for Apache
// Spark sessions, as Google's kernel gateway serves them to JupyterLab.
// Google's dataproc-jupyter-plugin (Apache-2.0, read 2026-11-23:
// dataproc_jupyter_plugin/handlers.py's configure_gateway_client_url)
// sets Jupyter Server's GatewayClient.url to
// https://PROJECT_NUMBER-dot-REGION.kernels.googleusercontent.com, a
// Jupyter Kernel Gateway whose kernelspecs are the project's session
// templates of Jupyter sessions, and whose kernels each run in a session
// of their own. Here the gateway is at /kernels/PROJECT-dot-REGION/ on
// this API's port (PROJECT the project's ID or number), the GatewayClient
// URL a JupyterLab is given:
//
//   - GET api/kernelspecs: each session template with a jupyter_session in
//     the region, named by its ID, its display name the template's
//     jupyter_session.display_name (else its ID), its language its kernel's;
//   - POST api/kernels {"name": TEMPLATE}: creates a session from the
//     template (SessionController.CreateSession), waits for it to be
//     ACTIVE, and starts the template's kernel (PYTHON: PySpark, SCALA:
//     Almond) on the session's kernel server (internal/sparkproc,
//     kernelhost.go), its `spark` the session's Spark Connect session;
//   - GET api/kernels, GET api/kernels/ID; POST api/kernels/ID/interrupt
//     and /restart; the api/kernels/ID/channels websocket: the kernel
//     server's, relayed;
//   - DELETE api/kernels/ID: shuts the kernel down and terminates its
//     session.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
)

// GatewayBase is the kernel gateway's path prefix on this API's port.
const GatewayBase = "/kernels/"

var gatewayHostRE = regexp.MustCompile(`^/kernels/([a-z0-9-]+)-dot-([a-z]+-[a-z]+[0-9]+)(/.*)?$`)

// gatewayKernel is a kernel the gateway started: its session and its id
// on the session's kernel server.
type gatewayKernel struct {
	session, spec string
}

// kernelHost is a Conn with a kernel server (*sparkproc.Connect).
type kernelHost interface{ KernelHost() string }

func gatewayError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": fmt.Sprintf(format, args...), "reason": http.StatusText(code)})
}

func gatewayJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// projectOf resolves a gateway host's project, by its ID or its number
// (Google's gateway host names the project by number).
func (s *Server) projectOf(p string) string {
	if !isDigits(p) || s.opts.ProjectNumber == nil {
		return p
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.templates {
		if parts := strings.Split(name, "/"); len(parts) > 1 && s.opts.ProjectNumber(parts[1]) == p {
			return parts[1]
		}
	}
	return p
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func kernelLanguage(k dataprocpb.JupyterConfig_Kernel) (string, string) {
	if k == dataprocpb.JupyterConfig_SCALA {
		return "scala", "scala"
	}
	return "python", "python3"
}

// serveGateway serves the kernel gateway's REST and websocket API.
func (s *Server) serveGateway(w http.ResponseWriter, r *http.Request) {
	m := gatewayHostRE.FindStringSubmatch(r.URL.Path)
	if m == nil {
		gatewayError(w, http.StatusNotFound, "not a kernel gateway path: /kernels/PROJECT-dot-REGION/api/...")
		return
	}
	project, region, rest := s.projectOf(m[1]), m[2], strings.TrimPrefix(m[3], "/")
	parent := "projects/" + project + "/locations/" + region
	switch {
	case rest == "api" || rest == "api/":
		gatewayJSON(w, http.StatusOK, map[string]string{"version": "2.14.2"})
	case rest == "api/kernelspecs" && r.Method == http.MethodGet:
		gatewayJSON(w, http.StatusOK, s.kernelspecs(parent))
	case rest == "api/kernels" && r.Method == http.MethodGet:
		s.listKernels(w, r, parent)
	case rest == "api/kernels" && r.Method == http.MethodPost:
		s.startKernel(w, r, parent)
	case strings.HasPrefix(rest, "api/kernels/"):
		id, tail, _ := strings.Cut(strings.TrimPrefix(rest, "api/kernels/"), "/")
		s.kernelCall(w, r, parent, id, tail)
	default:
		gatewayError(w, http.StatusNotFound, "%s is not a kernel gateway resource", rest)
	}
}

// kernelspecs are the region's Jupyter session templates as kernelspecs.
func (s *Server) kernelspecs(parent string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	specs := map[string]any{}
	var names []string
	for name, t := range s.templates {
		js := t.GetJupyterSession()
		if js == nil || !strings.HasPrefix(name, parent+"/sessionTemplates/") {
			continue
		}
		id := name[strings.LastIndex(name, "/")+1:]
		display := js.GetDisplayName()
		if display == "" {
			display = id
		}
		lang, _ := kernelLanguage(js.GetKernel())
		specs[id] = map[string]any{"name": id, "resources": map[string]string{},
			"spec": map[string]any{"display_name": display, "language": lang, "argv": []string{}, "env": map[string]string{},
				"metadata": map[string]any{"session_template": name}}}
		names = append(names, id)
	}
	sort.Strings(names)
	def := ""
	if len(names) > 0 {
		def = names[0]
	}
	return map[string]any{"default": def, "kernelspecs": specs}
}

// kernelServer is the kernel server of the kernel id's session, or "".
func (s *Server) kernelServer(parent, id string) (gatewayKernel, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.kernels[id]
	if !ok || !strings.HasPrefix(k.session, parent+"/") {
		return gatewayKernel{}, ""
	}
	if c, ok := s.connects[k.session].(kernelHost); ok {
		return k, c.KernelHost()
	}
	return k, ""
}

// kernelModel is the kernel server's model of a kernel, named by its spec.
func kernelModel(b []byte, spec string) map[string]any {
	var model map[string]any
	if json.Unmarshal(b, &model) != nil {
		return nil
	}
	model["name"] = spec
	return model
}

func (s *Server) listKernels(w http.ResponseWriter, r *http.Request, parent string) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.kernels))
	for id, k := range s.kernels {
		if strings.HasPrefix(k.session, parent+"/") {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(ids)
	out := []any{}
	for _, id := range ids {
		k, addr := s.kernelServer(parent, id)
		if addr == "" {
			continue
		}
		b, code, err := kernelHTTP(r.Context(), http.MethodGet, "http://"+addr+"/api/kernels/"+id, nil)
		if err == nil && code == http.StatusOK {
			if m := kernelModel(b, k.spec); m != nil {
				out = append(out, m)
			}
		}
	}
	gatewayJSON(w, http.StatusOK, out)
}

func kernelHTTP(ctx context.Context, method, u string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return b, resp.StatusCode, err
}

// startKernel starts a kernel of a session template in a session of its
// own.
func (s *Server) startKernel(w http.ResponseWriter, r *http.Request, parent string) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Name == "" {
		gatewayError(w, http.StatusBadRequest, "the request needs a kernelspec name")
		return
	}
	tn := parent + "/sessionTemplates/" + req.Name
	s.mu.Lock()
	t, ok := s.templates[tn]
	s.mu.Unlock()
	if !ok || t.GetJupyterSession() == nil {
		gatewayError(w, http.StatusNotFound, "No such kernel named %s", req.Name)
		return
	}
	_, kernelName := kernelLanguage(t.GetJupyterSession().GetKernel())
	sid := "kernel-" + strings.ReplaceAll(newUUID(), "-", "")[:20]
	ctx := r.Context()
	if _, err := s.CreateSession(ctx, &dataprocpb.CreateSessionRequest{Parent: parent, SessionId: sid,
		Session: &dataprocpb.Session{SessionTemplate: tn}}); err != nil {
		gatewayError(w, http.StatusInternalServerError, "create the kernel's session: %v", err)
		return
	}
	name := parent + "/sessions/" + sid
	var addr string
	for addr == "" {
		s.mu.Lock()
		x := s.sessions[name]
		var st dataprocpb.Session_State
		var msg string
		if x != nil {
			st, msg = x.GetState(), x.GetStateMessage()
		}
		if c, ok := s.connects[name].(kernelHost); ok && st == dataprocpb.Session_ACTIVE {
			addr = c.KernelHost()
		}
		s.mu.Unlock()
		if x == nil || sessionDone(st) {
			gatewayError(w, http.StatusInternalServerError, "the kernel's session %s did not start: %s", name, msg)
			return
		}
		if addr == "" {
			if st == dataprocpb.Session_ACTIVE {
				_, _ = s.TerminateSession(context.WithoutCancel(ctx), &dataprocpb.TerminateSessionRequest{Name: name})
				gatewayError(w, http.StatusInternalServerError, "the kernel's session %s has no kernels: its runtime has no kernel image (CloudBurrow serves a Jupyter session's kernels on runtimes 1.2, 2.2 and 2.3)", name)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
	body, _ := json.Marshal(map[string]string{"name": kernelName})
	b, code, err := kernelHTTP(ctx, http.MethodPost, "http://"+addr+"/api/kernels", body)
	if err != nil || code != http.StatusCreated && code != http.StatusOK {
		gatewayError(w, http.StatusInternalServerError, "start the %s kernel: %v %s", kernelName, err, b)
		return
	}
	model := kernelModel(b, req.Name)
	id, _ := model["id"].(string)
	if id == "" {
		gatewayError(w, http.StatusInternalServerError, "the kernel server answered %s", b)
		return
	}
	s.mu.Lock()
	s.kernels[id] = gatewayKernel{session: name, spec: req.Name}
	s.mu.Unlock()
	s.logf("dataproc: kernel %s (%s, %s) started on session %s", id, req.Name, kernelName, name)
	gatewayJSON(w, http.StatusCreated, model)
}

// kernelCall is a call on one kernel: its model, deletion, interrupt,
// restart and channels websocket.
func (s *Server) kernelCall(w http.ResponseWriter, r *http.Request, parent, id, tail string) {
	k, addr := s.kernelServer(parent, id)
	if addr == "" {
		gatewayError(w, http.StatusNotFound, "Kernel does not exist: %s", id)
		return
	}
	switch {
	case tail == "" && r.Method == http.MethodGet:
		b, code, err := kernelHTTP(r.Context(), http.MethodGet, "http://"+addr+"/api/kernels/"+id, nil)
		if err != nil || code != http.StatusOK {
			gatewayError(w, http.StatusNotFound, "Kernel does not exist: %s", id)
			return
		}
		gatewayJSON(w, http.StatusOK, kernelModel(b, k.spec))
	case tail == "" && r.Method == http.MethodDelete:
		_, _, _ = kernelHTTP(r.Context(), http.MethodDelete, "http://"+addr+"/api/kernels/"+id, nil)
		s.mu.Lock()
		delete(s.kernels, id)
		s.mu.Unlock()
		if _, err := s.TerminateSession(context.WithoutCancel(r.Context()), &dataprocpb.TerminateSessionRequest{Name: k.session}); err != nil {
			s.logf("dataproc: terminate kernel %s's session: %v", id, err)
		}
		w.WriteHeader(http.StatusNoContent)
	case tail == "channels" || (r.Method == http.MethodPost && (tail == "interrupt" || tail == "restart")):
		s.touch(k.session)
		target := &url.URL{Scheme: "http", Host: addr}
		proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/api/kernels/" + id + "/" + tail
			pr.Out.URL.RawPath = ""
			pr.Out.Host = addr
			pr.Out.Header.Del("Origin")
		}}
		proxy.ServeHTTP(w, r)
	default:
		gatewayError(w, http.StatusNotFound, "%s is not a kernel resource", tail)
	}
}
