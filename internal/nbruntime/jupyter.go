package nbruntime

// A Vertex AI Workbench instance's JupyterLab (#2198). Where a Colab
// runtime's container runs kernel.py, an instance's runs the real,
// unmodified Jupyter Server and JupyterLab of the same image, as
// Workbench's VM does: its file browser, launcher, notebooks, consoles,
// text editor and Terminal (a shell in the instance's container), and
// real ipykernel kernels (Python 3), with Google's client libraries and
// %%bigquery loaded.
//
// Egress is the Colab runtime's: the container is on a network created
// --internal, and its one peer, the relay, carries these ports, as the
// instance's VM reaches Google's APIs:
//
//	host.docker.internal:8080  the instance's BigQuery REST port
//	host.docker.internal:8081  its Dataproc API (gRPC), when served
//	host.docker.internal:8082  the instance's metadata server (below)
//	host.docker.internal:8083  its Cloud Storage JSON API, when served
//
// and one inbound port, 8888, published on the host's loopback alone and
// relayed to JupyterLab's 8080 on the internal network: the console's
// proxy (internal/console's jupyterproxy.go) is its only client, and sends
// the server's token, which nothing else is given.
//
// The metadata server is the instance's own, as a GCE VM's is: it names
// the instance's service account (the Compute Engine default one when
// none is set) and mints that account's access tokens, and answers the
// instance's project, zone, name, ID and metadata attributes. Google's
// clients in the container (google.auth.default, GCE_METADATA_HOST) take
// their credentials from it; cloudburrow_endpoints.py gives them the
// relay's ports as their default endpoints.
//
// Files: /home/jupyter is a Docker volume, the instance's data disk
// ("Local files in the /home/jupyter directory | Preserved",
// https://docs.cloud.google.com/vertex-ai/docs/workbench/instances/upgrade,
// read 2026-11-23). It is kept when the instance stops, and DeleteDisk
// removes it when the instance is deleted.
//
// Idle shutdown (https://docs.cloud.google.com/vertex-ai/docs/workbench/instances/idle-shutdown,
// read 2026-11-23): the instance shuts down when there is "no kernel
// activity for the specified time period". Running a cell or new output
// is activity; a JupyterLab page left open is not. The Jupyter Server's
// /api/kernels and /api/terminals report each kernel's and terminal's last
// activity, which the manager reads, as Workbench's agent reads port 8080.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Jupyter is a Workbench instance's JupyterLab setup (Options.Jupyter).
type Jupyter struct {
	// Volume is the Docker volume of the instance's data disk, mounted at
	// /home/jupyter; it is created when first used.
	Volume string
	// Account is the instance's service account, "" for the project's
	// Compute Engine default service account.
	Account string
	// Mint is a fresh access token for an account, nil for none.
	Mint func(account string) (string, error)
	// Name, Zone and ID are the instance's, as its metadata server
	// answers them.
	Name, Zone, ID string
	// Attributes are the instance's metadata (gce_setup.metadata).
	Attributes map[string]string
}

// The container's ports: JupyterLab's (Workbench's 8080) and the relay's
// inbound one.
const (
	jupyterPort      = "8080"
	jupyterRelayPort = "8888"
	metadataPort     = "8082"
	storagePort      = "8083"
)

// IdlePoll is how often an instance's kernel activity is read.
var IdlePoll = 30 * time.Second

// account is the instance's service account email.
func (j *Jupyter) account(project string) string {
	if j.Account != "" {
		return j.Account
	}
	return strconv.FormatInt(resource.ProjectNumber(project), 10) + "-compute@developer.gserviceaccount.com"
}

// JupyterEndpoint is the address on the host loopback that reaches the
// instance key's JupyterLab, and the server's token, when it is running.
func (m *Manager) JupyterEndpoint(key string) (addr, token string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[key]
	if s == nil || s.status.State != StateConnected || s.jupyterAddr == "" {
		return "", "", false
	}
	return s.jupyterAddr, s.jupyterToken, true
}

// DeleteDisk removes a data disk volume (the instance was deleted).
func (m *Manager) DeleteDisk(volume string) error {
	if volume == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := m.docker(ctx, "volume", "rm", "-f", volume); err != nil {
		return errors.New("delete the data disk: " + out)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// launchJupyter is launch for a Workbench instance.
func (m *Manager) launchJupyter(ctx context.Context, s *session) error {
	j := s.opts.Jupyter
	rest := ""
	if m.BigQuery != nil {
		rest = m.BigQuery()
	}
	if rest == "" {
		return errors.New("BigQuery is not served by this instance, so a Workbench instance has nothing to query")
	}
	var stops []func()
	stopAll := func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
	s.stop = stopAll
	relayTo := func(port, hostAddr string) (string, error) {
		if hostAddr == "" {
			return "", nil
		}
		t, stop, err := m.target(ctx, hostAddr)
		if err != nil {
			return "", err
		}
		stops = append(stops, stop)
		return port + "=" + t, nil
	}
	// The instance's metadata server, on the host's loopback.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	meta := &http.Server{Handler: hostguard.Wrap(m.metadataHandler(s), "metadata.google.internal"), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = meta.Serve(ln) }()
	stops = append(stops, func() { _ = meta.Close() })

	var relays []string
	for _, r := range []struct{ port, addr string }{
		{relayPort, rest}, {"8081", call(m.Dataproc)}, {metadataPort, ln.Addr().String()}, {storagePort, call(m.Storage)},
	} {
		a, err := relayTo(r.port, r.addr)
		if err != nil {
			return err
		}
		if a != "" {
			relays = append(relays, a)
		}
	}

	suffix := nameSafe.ReplaceAllString(strings.ToLower(s.key), "-")
	if len(suffix) > 30 {
		suffix = suffix[len(suffix)-30:]
	}
	suffix = strings.Trim(suffix, "-") + fmt.Sprintf("-%d", time.Now().UnixNano()%1e6)
	network, relay, kernel := "cb-nb-net-"+suffix, "cb-nb-relay-"+suffix, "cb-nb-"+suffix
	label := "io.cloudburrow.instance=" + m.Instance // sparkproc.InstanceLabel: swept by stop
	image := Tag()

	if out, err := m.docker(ctx, "network", "create", "--internal", "--label", label, network); err != nil {
		return errors.New("create the instance's internal network: " + out)
	}
	m.setNames(s, network)
	relayArgs := []string{"run", "-d", "--name", relay, "--label", label, "-p", "127.0.0.1::" + jupyterRelayPort, "--entrypoint", "python3"}
	if !m.desktop {
		relayArgs = append(relayArgs, "--add-host", "host.docker.internal:host-gateway")
	}
	relayArgs = append(append(relayArgs, image, "-I", "/opt/cloudburrow/relay.py"), relays...)
	if out, err := m.docker(ctx, relayArgs...); err != nil {
		return errors.New("start the instance's relay: " + out)
	}
	m.setNames(s, relay, network)
	if err := m.waitReady(ctx, relay); err != nil {
		return err
	}
	if out, err := m.docker(ctx, "network", "connect", "--alias", relayAlias, network, relay); err != nil {
		return errors.New("connect the instance's relay: " + out)
	}
	if out, err := m.docker(ctx, "volume", "create", "--label", label, "--label", "cloudburrow.workbench-disk=1", j.Volume); err != nil {
		return errors.New("create the instance's data disk: " + out)
	}

	token := randomHex(24)
	host := j.Name
	if host == "" {
		host = "workbench"
	}
	ep := "http://" + relayAlias + ":"
	runArgs := []string{"run", "-d", "--init", "--name", kernel, "--label", label, "--network", network,
		"--hostname", host, "--user", "1001:1001", "-w", "/home/jupyter", "-v", j.Volume + ":/home/jupyter",
		"-e", "HOME=/home/jupyter", "-e", "USER=jupyter", "-e", "SHELL=/bin/bash", "-e", "JUPYTER_TOKEN",
		"-e", "GOOGLE_CLOUD_PROJECT=" + s.project, "-e", "CB_WORKBENCH=1",
		"-e", "CB_BIGQUERY_ENDPOINT=" + ep + relayPort,
		"-e", "GCE_METADATA_HOST=" + relayAlias + ":" + metadataPort, "-e", "GCE_METADATA_IP=" + relayAlias + ":" + metadataPort}
	if call(m.Storage) != "" {
		runArgs = append(runArgs, "-e", "CB_STORAGE_ENDPOINT="+ep+storagePort)
	}
	if call(m.Dataproc) != "" {
		region := m.Region
		if region == "" {
			region = "us-central1"
		}
		runArgs = append(runArgs, "-e", "CB_DATAPROC_ENDPOINT="+dataprocEndpoint,
			"-e", "GOOGLE_CLOUD_DATAPROC_API_ENDPOINT="+dataprocEndpoint, "-e", "GOOGLE_CLOUD_REGION="+region)
	}
	for _, k := range sortedKeys(s.opts.Env) {
		runArgs = append(runArgs, "-e", k+"="+s.opts.Env[k])
	}
	runArgs = append(runArgs, "--entrypoint", "jupyter", image, "lab")
	bin := m.Docker
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, runArgs...)
	// Passed by name, so the token is not on docker's command line.
	cmd.Env = append(os.Environ(), "JUPYTER_TOKEN="+token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New("start the instance's JupyterLab: " + strings.TrimSpace(string(out)))
	}
	m.setNames(s, kernel, relay, network)
	ip, err := m.docker(ctx, "inspect", "--format", `{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`, kernel)
	if err != nil || net.ParseIP(ip) == nil {
		return errors.New("find the instance's address on its network: " + ip)
	}
	if out, err := m.docker(ctx, "exec", "-d", relay, "python3", "-I", "/opt/cloudburrow/relay.py", "in", jupyterRelayPort+"="+ip+":"+jupyterPort); err != nil {
		return errors.New("relay the instance's JupyterLab: " + out)
	}
	pub, err := m.docker(ctx, "port", relay, jupyterRelayPort+"/tcp")
	if err != nil {
		return errors.New("find the instance's JupyterLab port: " + pub)
	}
	addr := ""
	for _, line := range strings.Split(pub, "\n") {
		if strings.HasPrefix(line, "127.0.0.1:") {
			addr = strings.TrimSpace(line)
		}
	}
	if addr == "" {
		return errors.New("the instance's JupyterLab port is not on the loopback: " + pub)
	}
	if err := waitJupyter(ctx, addr, token, 3*time.Minute); err != nil {
		logs, _ := m.docker(ctx, "logs", "--tail", "40", kernel)
		return fmt.Errorf("%v: %s", err, strings.ReplaceAll(logs, token, "<token>"))
	}
	m.mu.Lock()
	idle := s.idleTimeout()
	s.jupyterAddr, s.jupyterToken = addr, token
	s.status = Status{State: StateConnected, Python: "3.12", Project: s.project, Image: image, Started: time.Now().UTC()}
	if idle > 0 {
		s.status.Idle = idle.String()
	}
	s.last = time.Now()
	m.mu.Unlock()
	if idle > 0 {
		done := make(chan struct{})
		stops = append(stops, func() { close(done) })
		go m.watchActivity(s, done)
	}
	m.logf("workbench instance %s: JupyterLab up (%s)", s.key, kernel)
	return nil
}

func call(f func() string) string {
	if f == nil {
		return ""
	}
	return f()
}

func (m *Manager) setNames(s *session, names ...string) {
	m.mu.Lock()
	s.names = names
	m.mu.Unlock()
}

// jupyterGet is a GET of the server's API with its token.
func jupyterGet(ctx context.Context, addr, token, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func waitJupyter(ctx context.Context, addr, token string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := jupyterGet(ctx, addr, token, "/api/status", nil)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("JupyterLab did not start in %s: %v", wait, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// activity is the server's last kernel or terminal activity, and whether
// a kernel is running a cell now.
func activity(ctx context.Context, addr, token string) (time.Time, bool, error) {
	var kernels []struct {
		LastActivity   time.Time `json:"last_activity"`
		ExecutionState string    `json:"execution_state"`
	}
	if err := jupyterGet(ctx, addr, token, "/api/kernels", &kernels); err != nil {
		return time.Time{}, false, err
	}
	var terminals []struct {
		LastActivity time.Time `json:"last_activity"`
	}
	_ = jupyterGet(ctx, addr, token, "/api/terminals", &terminals)
	var last time.Time
	busy := false
	for _, k := range kernels {
		if k.LastActivity.After(last) {
			last = k.LastActivity
		}
		busy = busy || k.ExecutionState == "busy"
	}
	for _, t := range terminals {
		if t.LastActivity.After(last) {
			last = t.LastActivity
		}
	}
	return last, busy, nil
}

// watchActivity shuts the instance down after its idle timeout with no
// kernel or terminal activity.
func (m *Manager) watchActivity(s *session, done chan struct{}) {
	t := time.NewTicker(IdlePoll)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		m.mu.Lock()
		addr, token, idle := s.jupyterAddr, s.jupyterToken, s.idleTimeout()
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		last, busy, err := activity(ctx, addr, token)
		cancel()
		if err != nil {
			continue
		}
		m.mu.Lock()
		if busy {
			s.last = time.Now()
		} else if last.After(s.last) {
			s.last = last
		}
		quiet := idle > 0 && time.Since(s.last) >= idle
		m.mu.Unlock()
		if quiet {
			m.logf("workbench instance %s: idle shutdown", s.key)
			go func() { _ = m.Disconnect(s.key) }()
			return
		}
	}
}

// Touch records activity on a runtime now (a test's or the console's).
func (m *Manager) Touch(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[key]; s != nil {
		s.last = time.Now()
	}
}

// metadataHandler is the instance's metadata server.
func (m *Manager) metadataHandler(s *session) http.Handler {
	j := s.opts.Jupyter
	project := s.project
	number := strconv.FormatInt(resource.ProjectNumber(project), 10)
	email := j.account(project)
	zone := j.Zone
	if zone == "" {
		zone = "us-central1-a"
	}
	text := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/text")
		_, _ = w.Write([]byte(body))
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	scopes := []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email"}
	attrs := func() []string {
		keys := make([]string, 0, len(j.Attributes))
		for k := range j.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	const v1 = "/computeMetadata/v1/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "Missing Metadata-Flavor:Google header.", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" {
			text(w, "computeMetadata/\n")
			return
		}
		if !strings.HasPrefix(r.URL.Path, v1) {
			http.NotFound(w, r)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, v1)
		switch p {
		case "":
			text(w, "instance/\nproject/\nuniverse/\n")
			return
		case "project/project-id":
			text(w, project)
			return
		case "project/numeric-project-id":
			text(w, number)
			return
		case "universe/universe-domain":
			text(w, "googleapis.com")
			return
		case "instance/id":
			text(w, j.ID)
			return
		case "instance/name":
			text(w, j.Name)
			return
		case "instance/hostname":
			text(w, j.Name+"."+zone+".c."+project+".internal")
			return
		case "instance/zone":
			text(w, "projects/"+number+"/zones/"+zone)
			return
		case "instance/attributes/":
			if r.URL.Query().Get("recursive") == "true" {
				writeJSON(w, j.Attributes)
				return
			}
			var b strings.Builder
			for _, k := range attrs() {
				b.WriteString(k + "\n")
			}
			text(w, b.String())
			return
		case "instance/service-accounts/":
			text(w, "default/\n"+email+"/\n")
			return
		}
		if k, ok := strings.CutPrefix(p, "instance/attributes/"); ok {
			v, found := j.Attributes[k]
			if !found {
				http.NotFound(w, r)
				return
			}
			text(w, v)
			return
		}
		rest, ok := strings.CutPrefix(p, "instance/service-accounts/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		acct, field, _ := strings.Cut(rest, "/")
		if acct != "default" && acct != email {
			http.Error(w, "service account not found", http.StatusNotFound)
			return
		}
		switch field {
		case "":
			if r.URL.Query().Get("recursive") == "true" {
				writeJSON(w, map[string]any{"aliases": []string{"default"}, "email": email, "scopes": scopes})
				return
			}
			text(w, "aliases\nemail\nidentity\nscopes\ntoken\n")
		case "email":
			text(w, email)
		case "aliases":
			text(w, "default")
		case "scopes":
			text(w, strings.Join(scopes, "\n")+"\n")
		case "token":
			tok := "cloudburrow-local"
			if j.Mint != nil {
				t, err := j.Mint(email)
				if err != nil {
					http.Error(w, err.Error(), http.StatusForbidden)
					return
				}
				if t != "" {
					tok = t
				}
			}
			writeJSON(w, map[string]any{"access_token": tok, "expires_in": 3599, "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	})
}
