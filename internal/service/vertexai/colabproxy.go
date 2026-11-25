package vertexai

// A Colab Enterprise runtime's proxy_uri (#2198): "The proxy endpoint used
// to access the NotebookRuntime" (the v1 NotebookRuntime reference, read
// 2026-11-24). A runtime runs the real Jupyter Server and JupyterLab
// (internal/nbruntime), so a RUNNING runtime's proxy_uri is, like a
// Workbench instance's, HASH-dot-REGION.notebooks.localhost:PORT on the
// console's port, which proxies it whole to the runtime's server
// (internal/console's jupyterproxy.go); it is empty while the runtime is
// not running or the console is not.

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// runtimeProxyLabel is the first label of a runtime's proxy host.
func runtimeProxyLabel(rt *aiplatformpb.NotebookRuntime) string {
	sum := sha256.Sum256([]byte("colab/" + rt.GetName()))
	region := "us-central1"
	if p := strings.Split(rt.GetName(), "/"); len(p) >= 4 {
		region = p[3]
	}
	return hex.EncodeToString(sum[:8]) + "-dot-" + region
}

// runtimeProxyURI is a RUNNING runtime's proxy_uri, "" otherwise; the
// caller holds st.mu.
func (s *Server) runtimeProxyURI(rt *aiplatformpb.NotebookRuntime) string {
	if rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_RUNNING {
		return ""
	}
	wb := s.workbench()
	wb.mu.Lock()
	console := wb.console
	wb.mu.Unlock()
	if console == nil {
		return ""
	}
	addr := console()
	_, port, err := net.SplitHostPort(addr)
	if addr == "" || err != nil {
		return ""
	}
	return runtimeProxyLabel(rt) + "." + WorkbenchProxyDomain + ":" + port
}

// NotebookRuntime is the runtime name as GetNotebookRuntime answers it.
func (s *Server) NotebookRuntime(name string) (*aiplatformpb.NotebookRuntime, error) {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.runtime(name)
	if err != nil {
		return nil, err
	}
	return s.runtimeView(st, e), nil
}

// runtimeView is st.view with the runtime's proxy_uri.
func (s *Server) runtimeView(st *notebookStore, e *runtimeEntry) *aiplatformpb.NotebookRuntime {
	out := st.view(e)
	out.ProxyUri = s.runtimeProxyURI(out)
	return out
}

// ColabJupyterLab is the JupyterLab behind a runtime's proxy host's first
// label: its loopback address and token and the runtime's name, or an
// error when no runtime has that proxy host or it is not running.
func (s *Server) ColabJupyterLab(label string) (addr, token, name string, err error) {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, n := range sortedNames(st.runtimes) {
		e := st.runtimes[n]
		if runtimeProxyLabel(e.RT) != label {
			continue
		}
		st.refresh(e)
		if e.RT.GetRuntimeState() != aiplatformpb.NotebookRuntime_RUNNING {
			return "", "", n, apierror.FailedPrecondition("Runtime %s is %s: start it to open JupyterLab", lastSegment(n), e.RT.GetRuntimeState())
		}
		h, ok := st.host.(JupyterHost)
		if !ok {
			return "", "", n, apierror.FailedPrecondition("this instance's notebook runtime does not serve JupyterLab")
		}
		a, t, ok := h.JupyterEndpoint(runtimeKey(n))
		if !ok {
			return "", "", n, apierror.FailedPrecondition("Runtime %s's JupyterLab is not running", lastSegment(n))
		}
		return a, t, n, nil
	}
	return "", "", "", apierror.NotFound("no runtime has the proxy host %s", label)
}
