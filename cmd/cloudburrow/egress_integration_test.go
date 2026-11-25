//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/localclient"
)

// egressGuard is added to the standard library's net package of the binary
// TestNoGoogleEgress runs (go build -overlay), so that binary itself refuses
// and records every TCP connection to an address that is not loopback, and
// every name it would resolve that is not localhost.
//
// It is in the binary rather than around it because nothing around it is
// both portable and complete: a proxy sees only the clients that honour
// HTTPS_PROXY, and metadata.OnGCE dials 169.254.169.254 with a transport that
// does not (cloud.google.com/go/compute/metadata v0.10.0,
// newDefaultHTTPClient); a firewall needs root, and a sandbox profile is
// macOS-only. Every TCP dial in Go reaches sysDialer.dialTCP, which consults
// testHookDialTCP, and every name a dial resolves passes testHookLookupIP
// (net/hook.go, the same in Go 1.26 and 1.27). What it does not see is a
// lookup made with Resolver.LookupHost directly, which is why the TCP side
// matters: metadata.OnGCE's DNS probe runs beside an HTTP probe it does see.
const egressGuard = `package net

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
)

var errCloudBurrowEgress = errors.New("refused by the CloudBurrow egress guard: not loopback")

func init() {
	path := os.Getenv("CLOUDBURROW_EGRESS_LOG")
	if path == "" {
		return
	}
	var mu sync.Mutex
	record := func(what string) {
		buf := make([]byte, 16<<10)
		buf = buf[:runtime.Stack(buf, false)]
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		f.WriteString("EGRESS pid " + cloudBurrowDecimal(os.Getpid()) + ": " + what + "\n" + string(buf) + "\n\n")
	}
	testHookDialTCP = func(ctx context.Context, network string, laddr, raddr *TCPAddr) (*TCPConn, error) {
		if raddr == nil || !raddr.IP.IsLoopback() {
			record("dial " + network + " " + raddr.String())
			return nil, &OpError{Op: "dial", Net: network, Addr: raddr, Err: errCloudBurrowEgress}
		}
		sd := &sysDialer{network: network, address: raddr.String()}
		return sd.doDialTCP(ctx, laddr, raddr)
	}
	lookup := testHookLookupIP
	testHookLookupIP = func(ctx context.Context, fn func(context.Context, string, string) ([]IPAddr, error), network, host string) ([]IPAddr, error) {
		h := strings.TrimSuffix(strings.ToLower(host), ".")
		if h != "localhost" && !strings.HasSuffix(h, ".localhost") && ParseIP(h) == nil {
			record("lookup " + host)
			return nil, &DNSError{Err: errCloudBurrowEgress.Error(), Name: host, IsNotFound: true}
		}
		return lookup(ctx, fn, network, host)
	}
}

func cloudBurrowDecimal(n int) string {
	if n <= 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
`

// googleEgressSwitches are the environment variables, in the module versions
// go.mod names, that turn on a path to Google in a client library the CLI
// links, each with the value that turns it on (#1828). TestNoGoogleEgress
// runs the CLI with all of them set. Where the library reads one is noted
// beside it; why each leaves the product's clients alone is in the comment
// after the list.
var googleEgressSwitches = []struct{ name, value string }{
	// cloud.google.com/go/bigtable v1.58.0: Direct Access, which overrides
	// ClientConfig.DisableDirectAccess (client.go:228, 440-443;
	// internal/transport/channel_pool_factory.go:223-226), and the classic
	// pool's own (internal/option/option.go:222).
	{"CBT_ENABLE_DIRECTPATH", "true"},
	{"CBT_BIGTABLE_CONN_POOL", "true"},
	// cloud.google.com/go/spanner v1.96.1: Direct Access (client.go:725,
	// 1001), the DirectPath fallback pool (client.go:758), the location API
	// (location_router.go:45), end-to-end tracing (client.go:779), the
	// built-in metrics and where they go (client.go:710, 721-736;
	// metrics.go:227).
	{"GOOGLE_SPANNER_ENABLE_DIRECT_ACCESS", "true"},
	{"GOOGLE_SPANNER_ENABLE_GCP_FALLBACK", "true"},
	{"GOOGLE_SPANNER_EXPERIMENTAL_LOCATION_API", "true"},
	{"SPANNER_ENABLE_END_TO_END_TRACING", "true"},
	{"SPANNER_DISABLE_BUILTIN_METRICS", "false"},
	{"SPANNER_DISABLE_AFE_SERVER_TIMING", "false"},
	{"SPANNER_DISABLE_DIRECT_ACCESS_GRPC_BUILTIN_METRICS", "false"},
	{"SPANNER_MONITORING_HOST", "monitoring.googleapis.com:443"},
	// cloud.google.com/go/storage v1.70.0: client metrics to Cloud
	// Monitoring (metrics.go:95, 112) and tracing (trace.go:63).
	{"GCP_STORAGE_GO_ENABLE_OTEL_METRICS", "true"},
	{"GCP_STORAGE_GO_ENABLE_OTEL_DEBUG_METRICS", "true"},
	{"GO_STORAGE_DEV_OTEL_TRACING", "true"},
	// cloud.google.com/go/auth v0.24.1 and google.golang.org/api v0.314.0,
	// every gRPC and HTTP client: DirectPath over xDS (auth
	// grpctransport/directpath.go:99, 194; api transport/grpc/dial.go:456),
	// S2A, whose address comes from the metadata server (auth
	// internal/transport/s2a.go:133; api internal/cba.go:293), mTLS endpoints
	// and client certificates (auth internal/transport/cba.go:294, 354; api
	// internal/cba.go:146, 204), regional access boundaries (auth
	// internal/regionalaccessboundary/regional_access_boundary.go:85).
	{"GOOGLE_CLOUD_ENABLE_DIRECT_PATH_XDS", "true"},
	{"GOOGLE_CLOUD_ENABLE_DIRECT_PATH_XDS_OVER_INTERCONNECT", "true"},
	{"EXPERIMENTAL_GOOGLE_API_USE_S2A", "true"},
	{"GOOGLE_API_USE_MTLS_ENDPOINT", "always"},
	{"GOOGLE_API_USE_CLIENT_CERTIFICATE", "true"},
	{"GOOGLE_AUTH_TRUST_BOUNDARY_ENABLED", "true"},
	// cloud.google.com/go/compute/metadata v0.10.0: says the process runs on
	// GCE (metadata.go:458), so every metadata-gated path goes on to query
	// it, by a name the guard records (metadata.go:519).
	{"GCE_METADATA_HOST", "metadata.google.internal"},
}

// Why the product's clients ignore each of googleEgressSwitches:
//
//   - Bigtable: the console's data clients run on a connection the console
//     dials (newConsoleBigtableClient), so the library builds no channel
//     pool and runs no Direct Access check; its admin clients dial no Direct
//     Access at all.
//   - Spanner: SPANNER_EMULATOR_HOST, which newConsoleSpannerClient sets
//     when unset, turns the built-in metrics and the location detector off,
//     and its endpoint is the local one.
//   - Storage: WithDisabledClientMetrics on every storage.Client.
//   - DirectPath, S2A and the mTLS endpoint: every client passes
//     option.WithoutAuthentication, which skips DirectPath in both
//     transports (auth grpctransport.go:333-386, api transport/grpc
//     dial.go:309), and an explicit endpoint, which skips S2A and the mTLS
//     endpoint (auth internal/transport/s2a.go shouldUseS2A, cba.go
//     getEndpoint). No client asks for credentials, so neither do the trust
//     boundaries nor the metadata server.

// TestNoGoogleEgress is #1825's proof that nothing CloudBurrow's own process
// builds reaches Google, or anywhere else off the machine: not a client
// library's built-in metrics exporter (Spanner's and Bigtable's send to
// monitoring.googleapis.com whatever endpoint the client was given), not
// OpenTelemetry's GCP resource detector or any other metadata-server probe,
// not a token refresh or an ADC lookup, not DirectPath.
//
// It builds the CLI with egressGuard, prefetches, starts every service
// offline with the guard on, makes a Spanner database and a Bigtable table
// to open, walks every console screen it can reach from each service's list
// (every list, every detail, three levels down), runs a Spanner query and a write and a BigQuery query
// through the console, saves and loads the state, resets, and then reads the
// guard's log: any line in it fails the test, with the stack that made it.
//
// The processes it covers are the CLI's own (`up`, detached, and each
// command); the components in the cluster are covered by the offline test's
// cut node egress (TestPrefetchThenUpOfflineWithNoEgress).
//
// It starts every service, which takes minutes and the images of all of
// them, so it runs only when asked. CLOUDBURROW_TEST_EGRESS_NAME,
// _PORT_BASE and _STATE_DIR choose the instance:
//
//	CLOUDBURROW_TEST_EGRESS=1 go test -tags=integration -run TestNoGoogleEgress -timeout 60m ./cmd/cloudburrow/
func TestNoGoogleEgress(t *testing.T) {
	if os.Getenv("CLOUDBURROW_TEST_EGRESS") == "" {
		t.Skip("starts every service; set CLOUDBURROW_TEST_EGRESS=1 to run")
	}
	tmp := t.TempDir()
	bin := buildEgressGuarded(t, tmp)
	logPath := filepath.Join(tmp, "egress.log")

	name := os.Getenv("CLOUDBURROW_TEST_EGRESS_NAME")
	if name == "" {
		name = fmt.Sprintf("egress-%d", time.Now().UnixNano()%1e6)
	}
	base := os.Getenv("CLOUDBURROW_TEST_EGRESS_PORT_BASE")
	if base == "" {
		base = strconv.Itoa(21000 + int(time.Now().UnixNano()%110)*100)
	}
	stateDir := os.Getenv("CLOUDBURROW_TEST_EGRESS_STATE_DIR")
	if stateDir == "" {
		stateDir = filepath.Join(tmp, "state")
	}
	var services []string
	for _, s := range config.KnownServices() {
		services = append(services, string(s))
	}
	flags := []string{"--name", name, "--state-dir", stateDir, "--port-base", base,
		"--services", strings.Join(services, ","), "--mode", "ephemeral"}

	// Nothing in the environment may stand in for a fix: an emulator
	// variable turns some of the libraries' exporters off by itself, and a
	// metadata host or a credential would change which paths run.
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasSuffix(k, "_EMULATOR_HOST") || strings.HasSuffix(k, "_EMULATOR_HOST_GRPC") ||
			k == "GCE_METADATA_HOST" || k == "GCE_METADATA_IP" || k == "GOOGLE_APPLICATION_CREDENTIALS" ||
			strings.HasPrefix(k, "CLOUDSDK_") {
			continue
		}
		env = append(env, kv)
	}
	// And every switch that turns a Google network path on in the client
	// libraries is on (#1828): the product's own clients must not follow
	// them.
	for _, s := range googleEgressSwitches {
		env = append(env, s.name+"="+s.value)
	}
	env = append(env, "CLOUDBURROW_EGRESS_LOG="+logPath)

	run := func(timeout time.Duration, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append(args, flags...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s\n%s", strings.Join(args, " "), err, tailLines(string(out), 40), egressReport(logPath))
		}
		return string(out)
	}
	t.Cleanup(func() {
		cmd := exec.Command(bin, append([]string{"stop"}, flags...)...)
		cmd.Env = env
		_ = cmd.Run()
		_ = exec.Command("kind", "delete", "cluster", "--name", "cloudburrow-"+name).Run()
	})

	// What `up` downloads (Knative's manifests, images, binaries) is
	// prefetched first, unguarded: those downloads are the product's
	// deliberate egress, which --offline then rules out, so the guard sees
	// only what nobody asked for.
	prefetch := exec.Command(bin, append([]string{"prefetch"}, flags...)...)
	prefetch.Env = env[:len(env)-1]
	if out, err := prefetch.CombinedOutput(); err != nil {
		t.Fatalf("cloudburrow prefetch: %v\n%s", err, tailLines(string(out), 40))
	}
	t.Logf("up: %s", tailLines(run(40*time.Minute, "up", "--offline", "--detach", "--detach-timeout", "35m"), 5))

	cfg, err := config.Load(config.Options{Args: flags})
	if err != nil {
		t.Fatal(err)
	}
	project := cfg.DefaultProject()
	vars := map[string]string{}
	if err := json.Unmarshal([]byte(run(time.Minute, "env", "--format", "json")), &vars); err != nil {
		t.Fatalf("env --format json is not JSON: %v", err)
	}
	console := "127.0.0.1:" + strconv.Itoa(cfg.Endpoints.Console)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	seedSpannerAndBigtable(t, ctx, vars, project)

	// The console, through the clients it builds for each screen.
	c := consoleCrawler{t: t, addr: console, project: project}
	var listed []string
	for _, svc := range c.services() {
		c.crawl(ctx, svc)
		listed = append(listed, svc)
	}
	t.Logf("crawled %d console services: %s (%d requests)", len(listed), strings.Join(listed, ","), c.requests)
	for _, q := range []struct{ service, mode, statement string }{
		{"spanner", "read-write", "INSERT INTO Egress (Id, Name) VALUES (2, 'two')"},
		{"spanner", "", "SELECT Id, Name FROM Egress"},
		{"bigquery", "", "SELECT 1 AS one"},
	} {
		path := []string{"egress", "egress"}
		if q.service == "bigquery" {
			path = []string{project}
		}
		body, _ := json.Marshal(map[string]any{"Path": path, "Statement": q.statement, "Mode": q.mode})
		code, out := c.do(ctx, http.MethodPost, "/api/query/"+q.service+"?project="+url.QueryEscape(project), string(body))
		t.Logf("console %s %q: %d %s", q.service, q.statement, code, firstLogLine(out))
	}

	// The CLI's own commands that reach the services.
	statePath := filepath.Join(tmp, "state.tar")
	run(10*time.Minute, "state", "save", statePath)
	run(10*time.Minute, "state", "load", statePath)
	run(10*time.Minute, "reset")
	run(time.Minute, "status")

	// The Bigtable client's Direct Access investigation and a Spanner
	// client's exporter run after the call that made them returns.
	time.Sleep(10 * time.Second)
	if r := egressReport(logPath); r != "" {
		t.Errorf("CloudBurrow's process connected, or tried to, off the machine:\n%s", r)
	}
}

// buildEgressGuarded builds this package with egressGuard in its net package.
func buildEgressGuarded(t *testing.T, dir string) string {
	t.Helper()
	guard := filepath.Join(dir, "egressguard.go")
	if err := os.WriteFile(guard, []byte(egressGuard), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, _ := json.Marshal(map[string]map[string]string{"Replace": {
		filepath.Join(runtime.GOROOT(), "src", "net", "zz_cloudburrow_egressguard.go"): guard,
	}})
	overlayPath := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "cloudburrow")
	if out, err := exec.Command("go", "build", "-overlay", overlayPath, "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build with the egress guard: %v\n%s", err, out)
	}
	// The guard is in force with this toolchain: a program built the same
	// way records, and is refused, a dial to TEST-NET-1 (RFC 5737), which
	// nothing routes.
	probeDir := filepath.Join(dir, "probe")
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	probe := "package main\n\nimport (\"net\"; \"os\")\n\nfunc main() {\n" +
		"\tif c, err := net.Dial(\"tcp\", \"192.0.2.1:9\"); err == nil {\n\t\tc.Close()\n\t\tos.Exit(3)\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(probeDir, "main.go"), []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, "go.mod"), []byte("module probe\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeBin := filepath.Join(dir, "probe-bin")
	build := exec.Command("go", "build", "-overlay", overlayPath, "-o", probeBin, ".")
	build.Dir = probeDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build the guard's probe: %v\n%s", err, out)
	}
	log := filepath.Join(dir, "selfcheck.log")
	cmd := exec.Command(probeBin)
	cmd.Env = append(os.Environ(), "CLOUDBURROW_EGRESS_LOG="+log)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the guard's probe: %v\n%s", err, out)
	}
	if r := egressReport(log); !strings.Contains(r, "dial tcp 192.0.2.1:9") {
		t.Fatalf("the egress guard is not in force: its probe recorded %q", firstLogLine(r))
	}
	return bin
}

// egressReport is the guard's log, or "" when it recorded nothing.
func egressReport(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// seedSpannerAndBigtable makes what the console's data clients need to be
// built: a Spanner database with a table, and a Bigtable table with a row.
// These clients run in the test, not the guarded binary.
func seedSpannerAndBigtable(t *testing.T, ctx context.Context, vars map[string]string, project string) {
	t.Helper()
	opts := func(addr string) []option.ClientOption {
		if addr == "" {
			t.Fatal("cloudburrow env exported no endpoint for a service the test seeds")
		}
		return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	}
	sp := opts(vars["SPANNER_EMULATOR_HOST"])
	ia, err := instance.NewInstanceAdminClient(ctx, sp...)
	if err != nil {
		t.Fatal(err)
	}
	defer ia.Close()
	op, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent: "projects/" + project, InstanceId: "egress",
		Instance: &instancepb.Instance{Config: "projects/" + project + "/instanceConfigs/emulator-config",
			DisplayName: "Egress", NodeCount: 1},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateInstance: %v", err)
	}
	if err == nil {
		if _, err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	da, err := database.NewDatabaseAdminClient(ctx, sp...)
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent: "projects/" + project + "/instances/egress", CreateStatement: "CREATE DATABASE egress",
		ExtraStatements: []string{"CREATE TABLE Egress (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)"},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if err == nil {
		if _, err := dop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}

	bt := opts(vars["BIGTABLE_EMULATOR_HOST"])
	admin, err := bigtable.NewAdminClient(ctx, project, "cloudburrow", bt...)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.CreateTable(ctx, "egress"); err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := admin.CreateColumnFamily(ctx, "egress", "cf"); err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateColumnFamily: %v", err)
	}
	data, err := localclient.BigtableWithConfig(ctx, project, "cloudburrow", bigtable.ClientConfig{
		DisableSession: true, DisableDirectAccess: true, MetricsProvider: bigtable.NoopMetricsProvider{}}, bt...)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	m := bigtable.NewMutation()
	m.Set("cf", "c", bigtable.Now(), []byte("v"))
	if err := data.Open("egress").Apply(ctx, "r1", m); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// consoleCrawler opens console screens the way the web client does.
type consoleCrawler struct {
	t        *testing.T
	addr     string
	project  string
	requests int
}

func (c *consoleCrawler) do(ctx context.Context, method, path, body string) (int, string) {
	c.requests++
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.addr+path, rdr)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// services are the console's service IDs.
func (c *consoleCrawler) services() []string {
	code, out := c.do(context.Background(), http.MethodGet, "/api/services", "")
	if code != http.StatusOK {
		c.t.Fatalf("GET /api/services = %d: %s", code, out)
	}
	var list struct {
		Services []struct {
			ID string `json:"id"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		c.t.Fatalf("/api/services is not JSON: %v: %s", err, firstLogLine(out))
	}
	var ids []string
	for _, s := range list.Services {
		ids = append(ids, s.ID)
	}
	if len(ids) == 0 {
		c.t.Fatalf("/api/services lists none: %s", firstLogLine(out))
	}
	return ids
}

type crawlListing struct {
	RowsOpenable bool `json:"rowsOpenable"`
	Items        []struct {
		Name  string   `json:"name"`
		Opens []string `json:"opens"`
	} `json:"items"`
}

type crawlDetail struct {
	Sections []struct {
		Listing crawlListing `json:"listing"`
	} `json:"sections"`
}

// crawl opens service's list, and the detail of up to three rows of every
// listing it meets, three levels deep.
func (c *consoleCrawler) crawl(ctx context.Context, service string) {
	q := url.Values{"project": {c.project}}
	_, out := c.do(ctx, http.MethodGet, "/api/resources/"+service+"?"+q.Encode(), "")
	var l crawlListing
	_ = json.Unmarshal([]byte(out), &l)
	c.open(ctx, service, nil, l, 0)
	c.do(ctx, http.MethodGet, "/api/page/"+service+"?"+q.Encode(), "")
}

func (c *consoleCrawler) open(ctx context.Context, service string, parent []string, l crawlListing, depth int) {
	if depth >= 3 {
		return
	}
	for i, it := range l.Items {
		if i == 3 {
			break
		}
		path := it.Opens
		if len(path) == 0 {
			if !l.RowsOpenable && depth > 0 {
				continue
			}
			path = append(append([]string(nil), parent...), it.Name)
		}
		q := url.Values{"project": {c.project}, "name": path}
		_, out := c.do(ctx, http.MethodGet, "/api/detail/"+service+"?"+q.Encode(), "")
		var d crawlDetail
		if json.Unmarshal([]byte(out), &d) != nil {
			continue
		}
		for _, s := range d.Sections {
			c.open(ctx, service, path, s.Listing, depth+1)
		}
	}
}

func firstLogLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
