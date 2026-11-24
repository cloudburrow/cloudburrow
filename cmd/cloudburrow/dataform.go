package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostmount"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

const (
	// dataformNodeImage is the Node.js image Dataform core runs in: the
	// official node:24.21.0-alpine, by its index digest (dependencies.json
	// dataformNode).
	dataformNodeImage = "node@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1"
	// dataformCLI is Google's @dataform/cli (Apache-2.0), which compiles a
	// project with the Dataform core version the project names
	// (dependencies.json dataformCLI).
	dataformCLI = "@dataform/cli@3.1.6"
)

// dataformService serves Dataform (#2105) in the CLI process, gRPC and JSON
// on one port. Its compilations and npm installs run in containers of the
// pinned Node image; its invocations run on the instance's BigQuery
// through the official client; its git remotes are the instance's Secure
// Source Manager repositories, whose access tokens it reads from Secret
// Manager.
type dataformService struct {
	cfg       config.Config
	tracing   *telemetry.Tracing
	calls     grpctransport.Observer
	interpose []grpc.UnaryServerInterceptor
	requests  func(rest.Request)
	wrapJSON  func(http.Handler) http.Handler
	bigquery  func() *netfwd.Forwarder
	secrets   *secretsService
	gitBase   func() string
	// runAddr is Cloud Run's port, which serves Developer Connect: a
	// repository's git_repository_link gets its tokens there.
	runAddr func() string
	// keys are the customer-managed keys (the instance's Cloud KMS).
	keys cmek.Keys

	server *grpctransport.Server
	api    *dataform.Server
	cancel context.CancelFunc
	done   chan struct{}
}

func newDataformService(cfg config.Config) *dataformService {
	if !dataformEnabled(cfg) {
		return nil
	}
	return &dataformService{cfg: cfg}
}

// dataformEnabled is whether `up` binds the Dataform port.
func dataformEnabled(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceDataform) && cfg.Endpoints.Dataform >= 0
}

func (s *dataformService) register(coord *lifecycle.Coordinator) {
	if s != nil {
		coord.Register(s)
	}
}

func (s *dataformService) Name() string { return "dataform" }

// API is the server, available after Start.
func (s *dataformService) API() *dataform.Server {
	if s == nil {
		return nil
	}
	return s.api
}

// Addr is the bound address.
func (s *dataformService) Addr() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.Addr()
}

func (s *dataformService) Start(ctx context.Context) error {
	data := filepath.Join(s.cfg.InstanceDir(), "dataform")
	state := ""
	if s.cfg.Mode == config.ModePersistent {
		state = filepath.Join(s.cfg.InstanceDir(), "dataform.json")
	} else {
		// Ephemeral: the files go with the instance.
		_ = os.RemoveAll(filepath.Join(data, "repos"))
		_ = os.RemoveAll(filepath.Join(data, "workspaces"))
	}
	_ = os.RemoveAll(filepath.Join(data, "snapshots"))
	opts := dataform.Options{
		DataDir:   data,
		State:     state,
		Runner:    &dataformDocker{dir: filepath.Join(data, "node")},
		Warehouse: dataformBigQuery{tunnel: s.bigquery},
		GitBase:   s.gitBase,
		Keys:      s.keys,
	}
	if s.runAddr != nil {
		opts.LinkToken = s.linkToken
	}
	if s.secrets != nil {
		opts.AccessSecret = s.accessSecret
	}
	api, err := dataform.NewServer(opts)
	if err != nil {
		return err
	}
	s.api = api
	s.server = grpctransport.New(net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.Endpoints.Dataform)))
	s.server.Observe(s.calls)
	if s.tracing != nil {
		s.server.ServerOptions(s.tracing.ServerOptions()...)
	}
	for _, i := range s.interpose {
		s.server.Interpose(i)
	}
	var jsonAPI http.Handler = dataform.NewRESTHandler(api)
	if s.wrapJSON != nil {
		jsonAPI = s.wrapJSON(jsonAPI)
	}
	if s.requests != nil {
		jsonAPI = rest.Observe(jsonAPI, s.requests)
	}
	s.server.ServeHTTP(jsonAPI)
	if err := s.server.Register(func(g *grpc.Server) { api.Register(g) }); err != nil {
		return err
	}
	if err := s.server.Start(ctx); err != nil {
		return err
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		api.RunSchedules(rctx, 15*time.Second)
	}()
	return nil
}

// linkToken is a Developer Connect git repository link's read-write
// token, fetched as a client would (FetchReadWriteToken) from the
// Developer Connect Cloud Run's port serves.
func (s *dataformService) linkToken(ctx context.Context, link string) (string, error) {
	addr := s.runAddr()
	if addr == "" {
		return "", errors.New("Developer Connect is not served on this instance")
	}
	c, err := developerconnect.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	resp, err := c.FetchReadWriteToken(ctx, &dcpb.FetchReadWriteTokenRequest{GitRepositoryLink: link})
	if err != nil {
		return "", err
	}
	return resp.GetToken(), nil
}

func (s *dataformService) accessSecret(ctx context.Context, name string) ([]byte, error) {
	st := s.secrets.Store()
	if st == nil {
		return nil, errors.New("Secret Manager has not started")
	}
	resp, err := secrets.NewGRPCServer(st).AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name})
	if err != nil {
		return nil, err
	}
	return resp.GetPayload().GetData(), nil
}

func (s *dataformService) Stop(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	if s.api != nil {
		s.api.Close()
	}
	if s.server == nil {
		return nil
	}
	return s.server.Stop(ctx)
}

// dataformResetter deletes the repositories and everything in them.
type dataformResetter struct{ svc *dataformService }

func (r *dataformResetter) Name() string { return "dataform" }

func (r *dataformResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *dataformResetter) ResetProject(_ context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New("Dataform has not started")
	}
	return api.ResetProject(project)
}

// ---- Dataform core in a container ----

// dataformDocker runs @dataform/cli and npm in containers of the pinned
// Node image. The CLI and npm's cache live in dir on the host. Compiling
// runs with no network, as user code runs in it; the npm cache is filled
// beforehand (the CLI installs a workflow_settings.yaml project's
// dataformCoreVersion from it, offline), and only npm installs reach the
// npm registry.
type dataformDocker struct {
	dir string
	mu  sync.Mutex
	// warmed are the core versions already in the cache.
	warmed map[string]bool
}

// dataformMount is one directory of this machine a container mounts. On a
// remote compute host it is a volume there (internal/hostmount): the npm
// cache stays on the host, and a writable directory's changes (the CLI's
// install, a workspace's node_modules) come back after the run.
type dataformMount struct {
	local, dst string
	mode       hostmount.Mode
	scratch    bool
}

func (d *dataformDocker) run(ctx context.Context, network bool, mounts []dataformMount, env []string, args ...string) ([]byte, []byte, error) {
	full := []string{"run", "--rm", "--label", "cloudburrow.dev/owned=true", "-e", "HOME=/tmp", "-e", "npm_config_cache=/npmcache",
		"-e", "npm_config_update_notifier=false", "-e", "npm_config_fund=false", "-e", "npm_config_audit=false"}
	if runtime.GOOS == "linux" && !hostmount.Remote() {
		// The bind mounts' files stay the user's own; on a remote host the
		// volumes are root's, as the image runs.
		full = append(full, "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()))
	}
	if !network {
		full = append(full, "--network", "none", "-e", "npm_config_offline=true")
	}
	all := append([]dataformMount{{local: filepath.Join(d.dir, "npmcache"), dst: "/npmcache", mode: hostmount.Cache}}, mounts...)
	var bound []*hostmount.Mount
	for _, m := range all {
		prep := hostmount.Bind
		if m.scratch {
			prep = hostmount.Scratch
		}
		b, err := prep(ctx, m.local, m.dst, m.mode)
		if err != nil {
			return nil, nil, err
		}
		defer b.Release()
		bound = append(bound, b)
		full = append(full, b.Args()...)
	}
	for _, e := range env {
		full = append(full, "-e", e)
	}
	full = append(full, dataformNodeImage)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	for _, b := range bound {
		if cerr := b.Collect(ctx); cerr != nil && err == nil {
			err = cerr
		}
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

func (d *dataformDocker) cliPath() string {
	return filepath.Join(d.dir, "cli", "node_modules", "@dataform", "cli", "bundle.js")
}

// ensureCLI installs @dataform/cli once.
func (d *dataformDocker) ensureCLI(ctx context.Context) error {
	if _, err := os.Stat(d.cliPath()); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(d.dir, "cli"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(d.dir, "npmcache"), 0o755); err != nil {
		return err
	}
	_, stderr, err := d.run(ctx, true, []dataformMount{{local: filepath.Join(d.dir, "cli"), dst: "/opt/dataform", mode: hostmount.ReadWrite}}, nil,
		"npm", "install", "--prefix", "/opt/dataform", "--ignore-scripts", "--no-save", dataformCLI)
	if err != nil {
		return fmt.Errorf("install %s: %v: %s", dataformCLI, err, dataformTail(stderr, 5))
	}
	return nil
}

var coreVersionRE = regexp.MustCompile(`(?m)^dataformCoreVersion:\s*["']?([0-9A-Za-z.+-]+)["']?\s*$`)

// warmCore puts a core version and its dependencies in the npm cache.
func (d *dataformDocker) warmCore(ctx context.Context, version string) error {
	if d.warmed[version] {
		return nil
	}
	tmp, err := os.MkdirTemp(d.dir, "core-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	_, stderr, err := d.run(ctx, true, []dataformMount{{local: tmp, dst: "/w", mode: hostmount.Cache, scratch: true}}, nil,
		"npm", "install", "--prefix", "/w", "--ignore-scripts", "--no-save", "@dataform/core@"+version)
	if err != nil {
		return fmt.Errorf("install @dataform/core@%s: %v: %s", version, err, dataformTail(stderr, 5))
	}
	if d.warmed == nil {
		d.warmed = map[string]bool{}
	}
	d.warmed[version] = true
	return nil
}

func (d *dataformDocker) Compile(ctx context.Context, dir string, flags []string) ([]byte, error) {
	d.mu.Lock()
	err := d.ensureCLI(ctx)
	if err == nil {
		if b, rerr := os.ReadFile(filepath.Join(dir, "workflow_settings.yaml")); rerr == nil {
			if _, perr := os.Stat(filepath.Join(dir, "package.json")); perr != nil {
				if m := coreVersionRE.FindSubmatch(b); m != nil {
					err = d.warmCore(ctx, string(m[1]))
				}
			}
		}
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	args := append([]string{"node", "/opt/dataform/node_modules/@dataform/cli/bundle.js", "compile", "/ws", "--json"}, flags...)
	stdout, stderr, err := d.run(ctx, false, []dataformMount{
		{local: filepath.Join(d.dir, "cli"), dst: "/opt/dataform", mode: hostmount.ReadOnly},
		{local: dir, dst: "/ws", mode: hostmount.ReadWrite, scratch: true}}, nil, args...)
	// The CLI exits 1 when the project has compilation errors, with the
	// graph and its errors on stdout.
	if bytes.Contains(stdout, []byte(`"graphErrors"`)) || bytes.Contains(stdout, []byte(`"projectConfig"`)) {
		return stdout, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, dataformTail(stderr, 8))
	}
	return stdout, nil
}

func (d *dataformDocker) InstallPackages(ctx context.Context, dir string, env []string) error {
	if err := os.MkdirAll(filepath.Join(d.dir, "npmcache"), 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_, stderr, err := d.run(ctx, true, []dataformMount{{local: dir, dst: "/ws", mode: hostmount.ReadWrite, scratch: true}}, env,
		"sh", "-c", "cd /ws && npm install --ignore-scripts")
	if err != nil {
		return fmt.Errorf("%v: %s", err, dataformTail(stderr, 8))
	}
	return nil
}

func dataformTail(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---- BigQuery ----

// dataformBigQuery runs invocations on the instance's BigQuery with the
// official client, through its tunnel.
type dataformBigQuery struct{ tunnel func() *netfwd.Forwarder }

func (b dataformBigQuery) client(ctx context.Context, project string) (*bigquery.Client, error) {
	var f *netfwd.Forwarder
	if b.tunnel != nil {
		f = b.tunnel()
	}
	if f == nil || f.HostAddr() == "" {
		return nil, errors.New("BigQuery is not served on this instance")
	}
	return bigquery.NewClient(ctx, project, option.WithEndpoint("http://"+f.HostAddr()), option.WithoutAuthentication())
}

func (b dataformBigQuery) EnsureDataset(ctx context.Context, project, dataset, location string) error {
	c, err := b.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Dataset(dataset).Metadata(ctx); err == nil {
		return nil
	}
	err = c.Dataset(dataset).Create(ctx, &bigquery.DatasetMetadata{Location: location})
	var g *googleapi.Error
	if errors.As(err, &g) && g.Code == http.StatusConflict {
		return nil
	}
	return err
}

func (b dataformBigQuery) Table(ctx context.Context, project, dataset, table string) (bool, string, []string, error) {
	c, err := b.client(ctx, project)
	if err != nil {
		return false, "", nil, err
	}
	defer c.Close()
	md, err := c.Dataset(dataset).Table(table).Metadata(ctx)
	var g *googleapi.Error
	if errors.As(err, &g) && g.Code == http.StatusNotFound {
		return false, "", nil, nil
	}
	if err != nil {
		return false, "", nil, err
	}
	var cols []string
	for _, f := range md.Schema {
		cols = append(cols, f.Name)
	}
	kind := "TABLE"
	switch md.Type {
	case bigquery.ViewTable:
		kind = "VIEW"
	case bigquery.MaterializedView:
		kind = "MATERIALIZED_VIEW"
	}
	return true, kind, cols, nil
}

// Modified is a table's lastModifiedTime, for v1beta1's table update
// triggers.
func (b dataformBigQuery) Modified(ctx context.Context, project, dataset, table string) (time.Time, bool, error) {
	c, err := b.client(ctx, project)
	if err != nil {
		return time.Time{}, false, err
	}
	defer c.Close()
	md, err := c.Dataset(dataset).Table(table).Metadata(ctx)
	var g *googleapi.Error
	if errors.As(err, &g) && g.Code == http.StatusNotFound {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return md.LastModifiedTime, true, nil
}

func (b dataformBigQuery) query(ctx context.Context, project, location, sql string) (*bigquery.Job, *bigquery.RowIterator, error) {
	c, err := b.client(ctx, project)
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()
	q := c.Query(sql)
	q.Location = location
	// The job's ID is chosen here, so a cancelled invocation can stop the
	// job even when jobs.insert had not answered yet.
	q.JobID = "dataform_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	stop := func() {
		// CancelWorkflowInvocation: the action's job is stopped too
		// (jobs.cancel), not left running.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if j, err := c.JobFromIDLocation(cctx, q.JobID, location); err == nil {
			_ = j.Cancel(cctx)
			_, _ = j.Wait(cctx)
		}
	}
	job, err := q.Run(ctx)
	if err != nil {
		if ctx.Err() != nil {
			stop()
			job, _ = c.JobFromIDLocation(context.WithoutCancel(ctx), q.JobID, location)
		}
		return job, nil, err
	}
	st, err := job.Wait(ctx)
	if err != nil {
		if ctx.Err() != nil {
			stop()
		}
		return job, nil, err
	}
	if err := st.Err(); err != nil {
		return job, nil, err
	}
	it, err := job.Read(ctx)
	return job, it, err
}

func (b dataformBigQuery) Exec(ctx context.Context, project, location, sql string) (string, error) {
	job, _, err := b.query(ctx, project, location, sql)
	id := ""
	if job != nil {
		id = job.ID()
	}
	return id, err
}

func (b dataformBigQuery) Count(ctx context.Context, project, location, sql string) (string, int64, error) {
	job, it, err := b.query(ctx, project, location, sql)
	id := ""
	if job != nil {
		id = job.ID()
	}
	if err != nil {
		return id, 0, err
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		if errors.Is(err, iterator.Done) {
			return id, 0, nil
		}
		return id, 0, err
	}
	if len(row) == 0 || row[0] == nil {
		return id, 0, nil
	}
	n, _ := row[0].(int64)
	return id, n, nil
}
