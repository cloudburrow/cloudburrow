package nbruntime

// A Vertex AI Workbench instance's JupyterLab (#2198). An instance's
// container runs the real, unmodified Jupyter Server and JupyterLab of the
// runtime image, as Workbench's VM does (and as a Colab runtime's does): its file browser, launcher, notebooks, consoles,
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
//	host.docker.internal:8084  its Resource Manager (REST)
//	host.docker.internal:8085  its Service Usage (REST)
//	host.docker.internal:8086  its Compute Engine API (REST)
//	host.docker.internal:8087  its Cloud KMS (REST)
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// MintID is an identity token for an account with the given claims,
	// signed by the instance's IAM keys; nil for none.
	MintID func(account string, claims map[string]any) (string, error)
	// Name, Zone and ID are the instance's, as its metadata server
	// answers them; Created its creation time (Unix seconds).
	Name, Zone, ID string
	Created        int64
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
	rmPort           = "8084"
	usagePort        = "8085"
	computePort      = "8086"
	kmsPort          = "8087"
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

// accountID is a service account's numeric unique ID, as the identity
// token's sub and azp name it: stable for its email.
func accountID(email string) string {
	sum := sha256.Sum256([]byte(email))
	n := new(big.Int).SetBytes(sum[:9])
	return "1" + fmt.Sprintf("%020s", n.String())[:20]
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// launchJupyter is launch for a Workbench instance.
//
// A Colab Enterprise runtime or a BigQuery Studio notebook (Options.Jupyter
// nil) runs the same Jupyter Server, as colab, in /home/colab, with no data
// disk or metadata server (its clients' credentials are cloudburrow_colab's),
// and the manager starts one Python 3 kernel on it, whose channels carry
// the console's cells (kernel.go).
func (m *Manager) launchJupyter(ctx context.Context, s *session) error {
	j := s.opts.Jupyter
	colab := j == nil
	if colab {
		j = &Jupyter{}
	}
	rest := ""
	if m.BigQuery != nil {
		rest = m.BigQuery()
	}
	if rest == "" {
		if colab {
			return errors.New("BigQuery is not served by this instance, so a notebook runtime has nothing to query")
		}
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
	metaAddr := ""
	if !colab {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		meta := &http.Server{Handler: hostguard.Wrap(m.metadataHandler(s), "metadata.google.internal"), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = meta.Serve(ln) }()
		stops = append(stops, func() { _ = meta.Close() })
		metaAddr = ln.Addr().String()
	}

	var relays []string
	for _, r := range []struct{ port, addr string }{
		{relayPort, rest}, {"8081", call(m.Dataproc)}, {metadataPort, metaAddr}, {storagePort, call(m.Storage)},
		{rmPort, workbenchOnly(colab, m.ResourceManager)}, {usagePort, workbenchOnly(colab, m.ServiceUsage)},
		{computePort, workbenchOnly(colab, m.Compute)}, {kmsPort, workbenchOnly(colab, m.KMS)},
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
	if !colab {
		if out, err := m.docker(ctx, "volume", "create", "--label", label, "--label", "cloudburrow.workbench-disk=1", j.Volume); err != nil {
			return errors.New("create the instance's data disk: " + out)
		}
	}

	token := randomHex(24)
	host := j.Name
	if host == "" {
		host = "workbench"
	}
	ep := "http://" + relayAlias + ":"
	cmdEnv := []string{"JUPYTER_TOKEN=" + token}
	var runArgs []string
	if colab {
		runArgs = []string{"run", "-d", "--init", "--name", kernel, "--label", label, "--network", network,
			"--hostname", "colab", "--user", "1000:1000", "-w", "/home/colab",
			"-e", "HOME=/home/colab", "-e", "USER=colab", "-e", "SHELL=/bin/bash", "-e", "JUPYTER_TOKEN",
			"-e", "GOOGLE_CLOUD_PROJECT=" + s.project, "-e", "CB_COLAB=1", "-e", "JUPYTER_CONFIG_PATH=/etc/cloudburrow/colab", "-e", "NO_GCE_CHECK=True",
			"-e", "CB_BIGQUERY_ENDPOINT=" + endpoint}
		if s.opts.Token != "" {
			// Passed by name, so the token is not on docker's command line.
			runArgs = append(runArgs, "-e", "CB_ACCESS_TOKEN")
			cmdEnv = append(cmdEnv, "CB_ACCESS_TOKEN="+s.opts.Token)
		}
	} else {
		runArgs = []string{"run", "-d", "--init", "--name", kernel, "--label", label, "--network", network,
			"--hostname", host, "--user", "1001:1001", "-w", "/home/jupyter", "-v", j.Volume + ":/home/jupyter",
			"-e", "HOME=/home/jupyter", "-e", "USER=jupyter", "-e", "SHELL=/bin/bash", "-e", "JUPYTER_TOKEN",
			"-e", "GOOGLE_CLOUD_PROJECT=" + s.project, "-e", "CB_WORKBENCH=1",
			"-e", "CB_BIGQUERY_ENDPOINT=" + ep + relayPort,
			"-e", "GCE_METADATA_HOST=" + relayAlias + ":" + metadataPort, "-e", "GCE_METADATA_IP=" + relayAlias + ":" + metadataPort}
	}
	if call(m.Storage) != "" && !colab {
		runArgs = append(runArgs, "-e", "CB_STORAGE_ENDPOINT="+ep+storagePort)
	}
	if !colab {
		runArgs = append(runArgs, m.gcloudEnv(s.project, ep)...)
	}
	if call(m.Dataproc) != "" {
		runArgs = append(runArgs, "-e", "CB_DATAPROC_ENDPOINT="+dataprocEndpoint,
			"-e", "GOOGLE_CLOUD_DATAPROC_API_ENDPOINT="+dataprocEndpoint, "-e", "GOOGLE_CLOUD_REGION="+m.region())
	}
	for _, k := range sortedKeys(s.opts.Env) {
		runArgs = append(runArgs, "-e", k+"="+s.opts.Env[k])
	}
	runArgs = append(runArgs, "--entrypoint", "jupyter", image, "lab")
	if colab {
		runArgs = append(runArgs, "--ServerApp.root_dir=/home/colab")
	}
	bin := m.Docker
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, runArgs...)
	// Passed by name, so the token is not on docker's command line.
	cmd.Env = append(os.Environ(), cmdEnv...)
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
	python, notes := "3.12", []string(nil)
	if colab {
		k, err := startKernel(ctx, addr, token)
		if err != nil {
			logs, _ := m.docker(ctx, "logs", "--tail", "40", kernel)
			return fmt.Errorf("%v: %s", err, strings.ReplaceAll(logs, token, "<token>"))
		}
		stops = append(stops, k.close)
		python, notes = k.python, k.notes
		m.mu.Lock()
		s.kernel = k
		m.mu.Unlock()
	}
	m.mu.Lock()
	idle := s.idleTimeout()
	s.jupyterAddr, s.jupyterToken = addr, token
	s.status = Status{State: StateConnected, Python: python, Project: s.project, Image: image, Started: time.Now().UTC(), Notes: notes}
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
	if colab {
		m.logf("notebook runtime %s: connected (%s)", s.key, kernel)
	} else {
		m.logf("workbench instance %s: JupyterLab up (%s)", s.key, kernel)
	}
	return nil
}

// workbenchOnly is f's address for a Workbench instance, "" for a Colab
// runtime.
func workbenchOnly(colab bool, f func() string) string {
	if colab {
		return ""
	}
	return call(f)
}

func (m *Manager) region() string {
	if m.Region == "" {
		return "us-central1"
	}
	return m.Region
}

// gcloudEnv configures the instance's Google Cloud CLI (gcloud), which
// Google's JupyterLab plugins read their project, region, credentials and
// API endpoints from, as on a Workbench VM: its credentials are the
// metadata server's (GCE_METADATA_ROOT), its endpoints the relay's
// (api_endpoint_overrides, in the form the plugins join paths to), and it
// never checks for updates or reports usage. CB_API_PORTS lists the relay
// ports the plugins' browser side may reach through the instance's
// JupyterLab (cloudburrow_endpoints' /cloudburrow-api/PORT/ route).
func (m *Manager) gcloudEnv(project, ep string) []string {
	env := []string{"-e", "GCE_METADATA_ROOT=" + relayAlias + ":" + metadataPort,
		"-e", "CLOUDSDK_CORE_PROJECT=" + project, "-e", "CLOUDSDK_COMPUTE_REGION=" + m.region(), "-e", "CLOUDSDK_DATAPROC_REGION=" + m.region(),
		"-e", "CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true", "-e", "CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true",
		"-e", "CLOUDSDK_CORE_DISABLE_PROMPTS=1", "-e", "CLOUDSDK_CONFIG=/home/jupyter/.config/gcloud",
		"-e", "CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY=" + ep + relayPort + "/"}
	ports := []string{relayPort}
	for _, o := range []struct {
		f             func() string
		port, env, ov string
	}{
		{m.Storage, storagePort, "", "STORAGE"},
		{m.Dataproc, "8081", "", "DATAPROC"},
		{m.ResourceManager, rmPort, "CB_RESOURCEMANAGER_ENDPOINT", "CLOUDRESOURCEMANAGER"},
		{m.ServiceUsage, usagePort, "CB_SERVICEUSAGE_ENDPOINT", "SERVICEUSAGE"},
		{m.Compute, computePort, "", "COMPUTE"},
		{m.KMS, kmsPort, "", "CLOUDKMS"},
	} {
		if call(o.f) == "" {
			continue
		}
		url := ep + o.port + "/"
		switch o.ov {
		case "STORAGE":
			// Both the Cloud Storage and Dataproc plugins add the path to a
			// bare host (gcs_jupyter_plugin's api_endpoint; the Dataproc
			// plugin's storage/v1/).
			url = ep + o.port
		case "COMPUTE":
			url = ep + o.port + "/compute/v1"
		}
		env = append(env, "-e", "CLOUDSDK_API_ENDPOINT_OVERRIDES_"+o.ov+"="+url)
		if o.env != "" {
			env = append(env, "-e", o.env+"="+ep+o.port)
		}
		ports = append(ports, o.port)
	}
	return append(env, "-e", "CB_API_PORTS="+strings.Join(ports, ","))
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
	// The access tokens this metadata server gave, which its userinfo
	// answers for.
	var mintedMu sync.Mutex
	minted := map[string]bool{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OAuth 2.0's userinfo (www.googleapis.com/oauth2/v3/userinfo),
		// which Google's BigQuery JupyterLab plugin asks for the principal
		// behind its credentials: an access token of the instance's
		// service account is that account (sub, email, email_verified);
		// any other is refused, as Google refuses an invalid token.
		if r.URL.Path == "/oauth2/v3/userinfo" {
			bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			mintedMu.Lock()
			known := ok && minted[strings.TrimSpace(bearer)]
			mintedMu.Unlock()
			if !known {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"Invalid Credentials"}`))
				return
			}
			writeJSON(w, map[string]any{"sub": accountID(email), "email": email, "email_verified": true})
			return
		}
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
		case "identity":
			// https://docs.cloud.google.com/compute/docs/instances/verifying-instance-identity
			// (read 2026-11-24): audience is required; format standard
			// (the default) or full, full adding the instance's
			// google.compute_engine claim and email (licenses=TRUE its
			// license codes, of which a local instance has none).
			q := r.URL.Query()
			audience := q.Get("audience")
			if audience == "" {
				http.Error(w, "non-empty audience parameter required", http.StatusBadRequest)
				return
			}
			format := q.Get("format")
			if format == "" {
				format = "standard"
			}
			if format != "standard" && format != "full" {
				http.Error(w, "format must be standard or full", http.StatusBadRequest)
				return
			}
			if j.MintID == nil {
				http.Error(w, "this instance mints no identity tokens", http.StatusNotFound)
				return
			}
			sub := accountID(email)
			claims := map[string]any{"aud": audience, "azp": sub, "sub": sub}
			if format == "full" {
				claims["email"], claims["email_verified"] = email, true
				ce := map[string]any{"instance_creation_timestamp": j.Created, "instance_id": j.ID, "instance_name": j.Name,
					"project_id": project, "project_number": resource.ProjectNumber(project), "zone": zone}
				if strings.EqualFold(q.Get("licenses"), "TRUE") {
					ce["license_id"] = []string{}
				}
				claims["google"] = map[string]any{"compute_engine": ce}
			}
			tok, err := j.MintID(email, claims)
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			text(w, tok)
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
			mintedMu.Lock()
			minted[tok] = true
			mintedMu.Unlock()
			writeJSON(w, map[string]any{"access_token": tok, "expires_in": 3599, "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	})
}
