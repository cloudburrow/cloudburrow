//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	nbpb "cloud.google.com/go/notebooks/apiv2/notebookspb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

// jupyterLab is an HTTP client of one instance's JupyterLab on its
// proxy_uri, holding the console session Open JupyterLab starts, as a
// browser does: every proxy host is dialed at the console's loopback
// port, as a browser resolves *.localhost.
type jupyterLab struct {
	t      *testing.T
	host   string // the proxy_uri, host:port
	port   string
	client *http.Client
	jar    *cookiejar.Jar
}

var proxyURIRE = regexp.MustCompile(`^[0-9a-f]{16}-dot-([a-z]+-[a-z]+[0-9]+)\.notebooks\.localhost:([0-9]+)$`)

// openJupyterLab is the console's Open JupyterLab for the instance name:
// the one-time link it answers, followed to /lab.
func openJupyterLab(t *testing.T, proxyURI, name string) *jupyterLab {
	t.Helper()
	m := proxyURIRE.FindStringSubmatch(proxyURI)
	if m == nil {
		t.Fatalf("proxy_uri %q is not HASH-dot-REGION.notebooks.localhost:PORT", proxyURI)
	}
	jar, _ := cookiejar.New(nil)
	j := &jupyterLab{t: t, host: proxyURI, port: m[2], jar: jar}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	j.client = &http.Client{Jar: jar, Timeout: 2 * time.Minute, Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, "127.0.0.1:"+j.port)
		}}}
	code, body := consoleDo(t, "127.0.0.1:"+j.port, http.MethodPost, "/api/workbench/open?name="+url.QueryEscape(name), "")
	var link struct{ URL string }
	if code != http.StatusOK || json.Unmarshal([]byte(body), &link) != nil {
		t.Fatalf("Open JupyterLab = %d %s", code, body)
	}
	resp, err := j.client.Get(link.URL)
	if err != nil {
		t.Fatalf("follow Open JupyterLab's link: %v", err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/lab" || !strings.Contains(string(page), "JupyterLab") {
		t.Fatalf("JupyterLab's page = %d at %s: %.300s", resp.StatusCode, resp.Request.URL, page)
	}
	return j
}

func (j *jupyterLab) do(method, path string, body any) (int, []byte) {
	j.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+j.host+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := j.client.Do(req)
	if err != nil {
		j.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// socket opens a WebSocket on the proxy host with the session cookie.
func (j *jupyterLab) socket(path string) *websocket.Conn {
	j.t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+j.port, 10*time.Second)
	if err != nil {
		j.t.Fatal(err)
	}
	cfg, err := websocket.NewConfig("ws://"+j.host+path, "http://"+j.host)
	if err != nil {
		j.t.Fatal(err)
	}
	u, _ := url.Parse("http://" + j.host + "/")
	var cookies []string
	for _, c := range j.jar.Cookies(u) {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	cfg.Header = http.Header{"Cookie": {strings.Join(cookies, "; ")}}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		j.t.Fatalf("WebSocket %s through the console: %v", path, err)
	}
	return ws
}

// execute runs code on a new kernel over its channels, Jupyter's
// messaging protocol, and returns its stdout and the reply's status.
func (j *jupyterLab) execute(code string) (string, string) {
	j.t.Helper()
	st, b := j.do(http.MethodPost, "/api/kernels", map[string]string{"name": "python3"})
	var k struct{ ID string }
	if st != http.StatusCreated || json.Unmarshal(b, &k) != nil {
		j.t.Fatalf("start a kernel = %d %s", st, b)
	}
	defer j.do(http.MethodDelete, "/api/kernels/"+k.ID, nil)
	ws := j.socket("/api/kernels/" + k.ID + "/channels")
	defer ws.Close()
	msgID := uuid.NewString()
	req := map[string]any{
		"header":        map[string]any{"msg_id": msgID, "msg_type": "execute_request", "username": "compat", "session": uuid.NewString(), "version": "5.3", "date": time.Now().UTC().Format(time.RFC3339)},
		"parent_header": map[string]any{}, "metadata": map[string]any{}, "channel": "shell",
		"content": map[string]any{"code": code, "silent": false, "store_history": true, "user_expressions": map[string]any{}, "allow_stdin": false, "stop_on_error": true},
	}
	if err := websocket.JSON.Send(ws, req); err != nil {
		j.t.Fatal(err)
	}
	var out strings.Builder
	status := ""
	_ = ws.SetDeadline(time.Now().Add(5 * time.Minute))
	for status == "" {
		var msg struct {
			Channel      string         `json:"channel"`
			Header       map[string]any `json:"header"`
			ParentHeader map[string]any `json:"parent_header"`
			Content      map[string]any `json:"content"`
		}
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			j.t.Fatalf("the kernel's channels: %v (output so far %q)", err, out.String())
		}
		if msg.ParentHeader["msg_id"] != msgID {
			continue
		}
		switch msg.Header["msg_type"] {
		case "stream":
			out.WriteString(fmt.Sprint(msg.Content["text"]))
		case "error":
			out.WriteString(fmt.Sprintf("%v: %v", msg.Content["ename"], msg.Content["evalue"]))
		case "execute_reply":
			status = fmt.Sprint(msg.Content["status"])
		}
	}
	return out.String(), status
}

// covers: google.cloud.notebooks.v2.NotebookService/CreateInstance, google.cloud.notebooks.v2.NotebookService/GetInstance, google.cloud.notebooks.v2.NotebookService/StopInstance, google.cloud.notebooks.v2.NotebookService/StartInstance, google.cloud.notebooks.v2.NotebookService/DeleteInstance
//
// TestWorkbenchJupyterLab (#2198): an instance the official
// cloud.google.com/go/notebooks/apiv2 client creates serves the real
// JupyterLab at its proxy_uri, HASH-dot-REGION.notebooks.localhost:PORT,
// which the console proxies once Open JupyterLab has started its session
// (and not before). Through it: a Python 3 kernel's channels run a cell
// whose google.auth.default() is the instance's service account from its
// metadata server, whose BigQuery client creates a table in the
// instance's BigQuery and queries it, and whose %%bigquery magic returns a
// DataFrame; a cell that reaches the internet fails; JupyterLab's
// Terminal is a shell in the instance whose output lands in its files; a
// file saved in /home/jupyter is there after a stop and a start.
func TestWorkbenchJupyterLab(t *testing.T) {
	h := New(t)
	_, clients := workbenchClients(t, h)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	c := clients["grpc"]
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	// The instance's own project, whose IAM policy Resource Manager
	// keeps: the instance's service account is granted a role on it.
	project := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	parent := fmt.Sprintf("projects/%s/locations/us-central1-a", project)
	id := "wb-lab-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	op, err := c.CreateInstance(ctx, &nbpb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &nbpb.Instance{
		Infrastructure: &nbpb.Instance_GceSetup{GceSetup: &nbpb.GceSetup{Metadata: map[string]string{"idle-timeout-seconds": "3600"}}}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	in, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the create operation: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &nbpb.DeleteInstanceRequest{Name: in.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if m := proxyURIRE.FindStringSubmatch(in.GetProxyUri()); in.GetState() != nbpb.State_ACTIVE || m == nil || m[1] != "us-central1" {
		t.Fatalf("created instance: state %s, proxy_uri %q", in.GetState(), in.GetProxyUri())
	}
	got, err := c.GetInstance(ctx, &nbpb.GetInstanceRequest{Name: in.GetName()})
	if err != nil || got.GetProxyUri() != in.GetProxyUri() {
		t.Fatalf("GetInstance's proxy_uri = %q, %v", got.GetProxyUri(), err)
	}

	// Without the console's session, the proxy host serves nothing.
	m := proxyURIRE.FindStringSubmatch(in.GetProxyUri())
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+m[2]+"/api/contents", nil)
	req.Host = in.GetProxyUri()
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the proxy host without a session = %v, %v", resp, err)
	} else {
		resp.Body.Close()
	}

	lab := openJupyterLab(t, in.GetProxyUri(), in.GetName())
	if st, b := lab.do(http.MethodGet, "/api/kernelspecs", nil); st != http.StatusOK || !strings.Contains(string(b), `"python3"`) {
		t.Fatalf("kernelspecs = %d %s", st, b)
	}
	// The kernel's identity is the instance's service account, the
	// project's Compute Engine default one, from the metadata server.
	out, status := lab.execute(`
import google.auth, google.auth.transport.requests
creds, project = google.auth.default()
creds.refresh(google.auth.transport.requests.Request())
print("identity", getattr(creds, "service_account_email", ""), project)
`)
	idm := regexp.MustCompile(`identity ([0-9]+-compute@developer\.gserviceaccount\.com) ` + regexp.QuoteMeta(project)).FindStringSubmatch(out)
	if status != "ok" || idm == nil {
		t.Fatalf("the kernel's identity = %s: %q, want the project's Compute Engine default service account", status, out)
	}
	// Which, as on Google, needs a BigQuery role to create and query
	// tables (with IAM enforced).
	pc, err := resourcemanager.NewProjectsClient(h.Context(), rmOptionsAt(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_RESOURCEMANAGER", EnvResourceManager))...)
	if err != nil {
		t.Fatalf("resourcemanager.NewProjectsClient: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	grantProject(t, h, pc, project, "roles/bigquery.admin", "serviceAccount:"+idm[1])

	dataset := "wb_lab_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	out, status = lab.execute(fmt.Sprintf(`
from google.cloud import bigquery
client = bigquery.Client()
client.create_dataset(%[1]q)
client.query("CREATE TABLE %[1]s.answers AS SELECT 42 AS answer UNION ALL SELECT 7").result()
rows = list(client.query("SELECT SUM(answer) AS total FROM %[1]s.answers").result())
print("total", rows[0]["total"])
ip = get_ipython()
ip.run_cell_magic("bigquery", "top_df", "SELECT MAX(answer) AS top FROM %[1]s.answers")
print("magic", int(ip.user_ns["top_df"]["top"][0]))
import urllib.request
try:
    urllib.request.urlopen("https://www.google.com", timeout=5)
    print("egress reached the internet")
except Exception as e:
    print("egress blocked", type(e).__name__)
client.delete_dataset(%[1]q, delete_contents=True)
`, dataset))
	if status != "ok" {
		t.Fatalf("the cell = %s: %s", status, out)
	}
	for _, want := range []string{"total 49", "magic 42", "egress blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("the cell's output %q lacks %q", out, want)
		}
	}

	// JupyterLab's Terminal: a shell in the instance.
	st, b := lab.do(http.MethodPost, "/api/terminals", nil)
	var term struct{ Name string }
	if st != http.StatusOK || json.Unmarshal(b, &term) != nil {
		t.Fatalf("new terminal = %d %s", st, b)
	}
	ws := lab.socket("/terminals/websocket/" + term.Name)
	if err := websocket.JSON.Send(ws, []string{"stdin", "echo $((6*7)) $(whoami) $PWD > from-terminal.txt\r"}); err != nil {
		t.Fatal(err)
	}
	var text string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if st, b := lab.do(http.MethodGet, "/api/contents/from-terminal.txt?content=1&type=file&format=text", nil); st == http.StatusOK {
			var f struct{ Content string }
			_ = json.Unmarshal(b, &f)
			if text = strings.TrimSpace(f.Content); text != "" {
				break
			}
		}
	}
	ws.Close()
	lab.do(http.MethodDelete, "/api/terminals/"+term.Name, nil)
	if text != "42 jupyter /home/jupyter" {
		t.Errorf("the terminal wrote %q, want 42 jupyter /home/jupyter", text)
	}

	// /home/jupyter is the data disk: kept across a stop and a start.
	if st, b := lab.do(http.MethodPut, "/api/contents/kept.txt", map[string]string{"type": "file", "format": "text", "content": "on the data disk"}); st != http.StatusCreated && st != http.StatusOK {
		t.Fatalf("save a file = %d %s", st, b)
	}
	stop, err := c.StopInstance(ctx, &nbpb.StopInstanceRequest{Name: in.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := stop.Wait(ctx); err != nil || s.GetState() != nbpb.State_STOPPED {
		t.Fatalf("stop = %v, %v", s, err)
	}
	if st, _ := lab.do(http.MethodGet, "/api/contents", nil); st != http.StatusServiceUnavailable {
		t.Errorf("a stopped instance's JupyterLab = %d, want 503", st)
	}
	if code, body := consoleDo(t, "127.0.0.1:"+lab.port, http.MethodPost, "/api/workbench/open?name="+url.QueryEscape(in.GetName()), ""); code != http.StatusConflict {
		t.Errorf("Open JupyterLab on a stopped instance = %d %s", code, body)
	}
	start, err := c.StartInstance(ctx, &nbpb.StartInstanceRequest{Name: in.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := start.Wait(ctx); err != nil || s.GetState() != nbpb.State_ACTIVE {
		t.Fatalf("start = %v, %v", s, err)
	}
	lab = openJupyterLab(t, in.GetProxyUri(), in.GetName())
	st, b = lab.do(http.MethodGet, "/api/contents/kept.txt?content=1&type=file&format=text", nil)
	var kept struct{ Content string }
	if st != http.StatusOK || json.Unmarshal(b, &kept) != nil || kept.Content != "on the data disk" {
		t.Errorf("after stop and start, kept.txt = %d %s", st, b)
	}
	del, err := c.DeleteInstance(ctx, &nbpb.DeleteInstanceRequest{Name: in.GetName()})
	if err != nil || del.Wait(ctx) != nil {
		t.Fatalf("delete = %v", err)
	}
	if _, err := c.GetInstance(ctx, &nbpb.GetInstanceRequest{Name: in.GetName()}); err == nil {
		t.Error("the deleted instance is still served")
	}
}
