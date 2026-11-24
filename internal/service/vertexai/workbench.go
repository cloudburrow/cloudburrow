package vertexai

// Vertex AI Workbench instances (#2173): google.cloud.notebooks.v2's
// NotebookService (notebooks.googleapis.com v2), over gRPC and REST, on
// the local Vertex AI endpoint. An instance is a notebook runtime of the
// instance's (internal/nbruntime), as a Colab Enterprise runtime is: a
// kernel container on an internal Docker network whose one peer relays to
// the instance's BigQuery. ACTIVE is that kernel connected; Stop deletes
// the container and keeps the instance STOPPED; Start makes a new one;
// Reset is a stop and a start ("a reset forcibly wipes the memory
// contents").
//
// Documented behaviour (https://docs.cloud.google.com/vertex-ai/docs/workbench/reference/rest/v2/projects.locations.instances,
// .../workbench/instances/create, create-console-quickstart, idle-shutdown;
// read 2026-11-22):
//   - an instance's ID "must start with a letter followed by up to 62
//     lowercase letters, numbers, or hyphens (-), and cannot end with a
//     hyphen";
//   - instance_owners "Currently supports one owner only";
//     gce_setup.service_accounts "Currently supports only one service
//     account"; data_disks "Currently supports only one data disk";
//   - boot_disk.disk_size_gb "defaults to the recommended value of
//     150GB", data_disks[].disk_size_gb "defaults to 100", each "up to a
//     maximum of 64000 GB";
//   - idle shutdown "is enabled and set to shut down your instance after
//     180 inactive minutes by default", set with the metadata key
//     idle-timeout-seconds, 10 to 1440 minutes;
//   - vm_image's project defaults to cloud-notebooks-managed (gcloud's
//     --vm-image-project);
//   - "The Open JupyterLab link is disabled while the instance
//     provisions".
//
// JupyterLab (#2198): an ACTIVE instance runs the real Jupyter Server and
// JupyterLab in its container (internal/nbruntime's jupyter.go), Python 3
// kernels with Google's client libraries and %%bigquery, its service
// account's identity through its own metadata server, /home/jupyter on
// its data disk (a Docker volume kept across stop and start and removed
// with the instance). proxy_uri is "The proxy endpoint that is used to
// access the Jupyter notebook", as Google's
// HASH-dot-REGION.notebooks.googleusercontent.com is: here
// HASH-dot-REGION.notebooks.localhost:PORT, the console's port, whose proxy
// (internal/console's jupyterproxy.go) serves the instance's JupyterLab
// to a browser the console opened it for. Idle shutdown is the metadata
// key idle-timeout-seconds: 180 minutes when absent, off when present and
// empty ("gcloud workbench instances update INSTANCE_NAME
// --metadata=idle-timeout-seconds=", idle-shutdown, read 2026-11-23), the
// instance STOPPED after that long with no kernel or terminal activity.
//
// Local differences: the instance runs on this machine's CPU, so one with
// accelerator_configs is refused (FAILED_PRECONDITION); it has no route to
// the internet whatever disable_public_ip says; container_image instances
// are refused (FAILED_PRECONDITION: the image is the instance's notebook
// runtime image).
//
// UNVERIFIED (re-read 2026-11-22, the same pages): the default
// machine_type (n1-standard-4, the console's) and disk types
// (PD_STANDARD, gcloud's "Defaults to standard persistent disk"); the
// default vm_image family (workbench-instances); which update_mask paths
// are accepted (labels, gce_setup.metadata, gce_setup.tags and, while
// STOPPED, gce_setup.machine_type); starting an ACTIVE instance and
// stopping a STOPPED one succeed without a change; CheckInstanceUpgradability
// answers upgradeable false (the runtime image is the instance's) and
// UpgradeInstance FAILED_PRECONDITION; the generated id is a UUID.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	nbpb "cloud.google.com/go/notebooks/apiv2/notebookspb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	workbenchMachineType = "n1-standard-4"
	workbenchBootDisk    = 150
	workbenchDataDisk    = 100
	workbenchMaxDisk     = 64000
	workbenchIdleKey     = "idle-timeout-seconds"
)

var (
	workbenchIDRE   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	workbenchMasks  = map[string]bool{"labels": true, "gce_setup.metadata": true, "gce_setup.tags": true, "gce_setup.machine_type": true}
	workbenchFilter = append([]aipfilter.Option{aipfilter.Fields("name", "state", "health_state", "creator", "labels", "id")},
		bothCases(nil, map[string]string{"machine_type": "gce_setup.machine_type"})...)
	workbenchOrder = aipfilter.Fields("name", "create_time", "update_time")
)

// JupyterHost is a NotebookHost that runs a Workbench instance's
// JupyterLab (NotebookRuntimeOptions.Jupyter).
type JupyterHost interface {
	// JupyterEndpoint is the loopback address and server token of the
	// running instance key's JupyterLab.
	JupyterEndpoint(key string) (addr, token string, ok bool)
	// DeleteDisk deletes an instance's data disk.
	DeleteDisk(volume string) error
}

// WorkbenchJupyter is a Workbench instance's JupyterLab setup.
type WorkbenchJupyter struct {
	// Volume is the data disk, mounted at /home/jupyter.
	Volume string
	// Account is the instance's service account, "" for the Compute
	// Engine default service account.
	Account string
	// Mint mints an access token for the account, nil for none.
	Mint func(account string) (string, error)
	// Name, Zone and ID are the instance's; Attributes its metadata.
	Name, Zone, ID string
	Attributes     map[string]string
}

type workbenchEntry struct {
	I *nbpb.Instance
	// Running is whether the instance should be running; Message why it
	// failed.
	Running bool
	Message string
}

type workbenchStore struct {
	mu        sync.Mutex
	file      string
	host      NotebookHost
	instances map[string]*workbenchEntry
	now       func() time.Time
	// console is the console's host:port, which proxy_uri names; "" when
	// the console is not running.
	console func() string
}

func (s *Server) workbench() *workbenchStore {
	s.notebooks() // the operations store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wb == nil {
		s.wb = &workbenchStore{instances: map[string]*workbenchEntry{}, now: time.Now}
	}
	return s.wb
}

func workbenchKey(name string) string { return "workbench/" + name }

// SetWorkbench runs Workbench instances on host and keeps them in file. An
// instance ACTIVE when CloudBurrow stopped is STOPPED: its container went
// with it.
func (s *Server) SetWorkbench(host NotebookHost, file string) {
	st := s.workbench()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.host, st.file = host, file
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var saved struct {
		Instances []json.RawMessage `json:"instances"`
	}
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for _, raw := range saved.Instances {
		in := &nbpb.Instance{}
		if lenient.Unmarshal(raw, in) == nil && in.GetName() != "" {
			in.State, in.HealthState = nbpb.State_STOPPED, nbpb.HealthState_HEALTH_STATE_UNSPECIFIED
			st.instances[in.GetName()] = &workbenchEntry{I: in}
		}
	}
}

// save writes the store; the caller holds st.mu.
func (st *workbenchStore) save() {
	if st.file == "" {
		return
	}
	out := struct {
		Instances []json.RawMessage `json:"instances"`
	}{Instances: []json.RawMessage{}}
	for _, n := range sortedNames(st.instances) {
		if b, err := protojson.Marshal(st.instances[n].I); err == nil {
			out.Instances = append(out.Instances, b)
		}
	}
	b, _ := json.Marshal(out)
	_ = os.MkdirAll(filepath.Dir(st.file), 0o755)
	tmp := st.file + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, st.file)
	}
}

func workbenchUnavailable() error {
	return apierror.FailedPrecondition("Workbench instances are not served on this instance: they run on the instance's notebook runtime, which needs Docker and BigQuery (start with --services including run and bigquery)")
}

// refresh brings e's state up to date with the host; the caller holds
// st.mu.
func (st *workbenchStore) refresh(e *workbenchEntry) {
	in := e.I
	if !e.Running {
		in.State, in.HealthState = nbpb.State_STOPPED, nbpb.HealthState_HEALTH_STATE_UNSPECIFIED
		return
	}
	state, msg := "DISCONNECTED", ""
	if st.host != nil {
		state, msg = st.host.State(workbenchKey(in.GetName()))
	}
	switch state {
	case "CONNECTED":
		in.State, in.HealthState = nbpb.State_ACTIVE, nbpb.HealthState_HEALTHY
		in.HealthInfo = nil
		if in.ProxyUri == "" {
			in.ProxyUri = st.proxyURI(in)
		}
	case "STARTING", "BUILDING":
		in.State, in.HealthState = nbpb.State_PROVISIONING, nbpb.HealthState_HEALTH_STATE_UNSPECIFIED
	case "FAILED":
		e.Running, e.Message = false, msg
		in.State, in.HealthState = nbpb.State_STOPPED, nbpb.HealthState_UNHEALTHY
		in.HealthInfo = map[string]string{"error": msg}
		st.save()
	default:
		// Its idle shutdown stopped it.
		e.Running = false
		in.State, in.HealthState = nbpb.State_STOPPED, nbpb.HealthState_HEALTH_STATE_UNSPECIFIED
		st.save()
	}
}

func (st *workbenchStore) view(e *workbenchEntry) *nbpb.Instance {
	st.refresh(e)
	return proto.Clone(e.I).(*nbpb.Instance)
}

func (st *workbenchStore) instance(name string) (*workbenchEntry, error) {
	if _, _, _, err := parseChild("name", "instances", name); err != nil {
		return nil, err
	}
	e, ok := st.instances[name]
	if !ok {
		return nil, apierror.NotFound("Instance %s not found", name)
	}
	return e, nil
}

// workbenchIdle is the idle shutdown the metadata sets: the default when
// the key is absent, 0 (off) when it is empty.
func workbenchIdle(md map[string]string) (time.Duration, error) {
	v, ok := md[workbenchIdleKey]
	if !ok {
		return defaultIdleTimeout, nil
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 600 || n > 86400 {
		return 0, apierror.InvalidArgument("metadata %s must be a number of seconds from 600 to 86400 (10 to 1440 minutes), not %q", workbenchIdleKey, v)
	}
	return time.Duration(n) * time.Second, nil
}

func checkWorkbenchSetup(g *nbpb.GceSetup) error {
	if g == nil {
		return nil
	}
	if len(g.GetServiceAccounts()) > 1 {
		return apierror.InvalidArgument("gce_setup.service_accounts: Currently supports only one service account")
	}
	for _, sa := range g.GetServiceAccounts() {
		if sa.GetEmail() != "" && !strings.Contains(sa.GetEmail(), "@") {
			return apierror.InvalidArgument("gce_setup.service_accounts[0].email %q is not a service account email", sa.GetEmail())
		}
	}
	if len(g.GetDataDisks()) > 1 {
		return apierror.InvalidArgument("gce_setup.data_disks: Currently supports only one data disk")
	}
	if n := g.GetBootDisk().GetDiskSizeGb(); n < 0 || n > workbenchMaxDisk {
		return apierror.InvalidArgument("gce_setup.boot_disk.disk_size_gb must be at most %d GB", workbenchMaxDisk)
	}
	for _, d := range g.GetDataDisks() {
		if n := d.GetDiskSizeGb(); n < 0 || n > workbenchMaxDisk {
			return apierror.InvalidArgument("gce_setup.data_disks[0].disk_size_gb must be at most %d GB", workbenchMaxDisk)
		}
	}
	if g.GetMachineType() != "" && !machineTypeRE.MatchString(g.GetMachineType()) {
		return apierror.InvalidArgument("gce_setup.machine_type %q is not a machine type", g.GetMachineType())
	}
	if _, err := workbenchIdle(g.GetMetadata()); err != nil {
		return err
	}
	return nil
}

// runnableWorkbench refuses what this machine cannot run.
func runnableWorkbench(g *nbpb.GceSetup) error {
	if len(g.GetAcceleratorConfigs()) > 0 {
		return apierror.FailedPrecondition("this instance runs Workbench instances on the machine's CPU: an instance with accelerators cannot start")
	}
	if g.GetContainerImage() != nil {
		return apierror.FailedPrecondition("this instance runs Workbench instances on its notebook runtime image: a container_image instance cannot start")
	}
	return nil
}

// workbenchDefaults fills Google's defaults in.
func workbenchDefaults(in *nbpb.Instance) {
	g := in.GetGceSetup()
	if g == nil {
		g = &nbpb.GceSetup{}
		in.Infrastructure = &nbpb.Instance_GceSetup{GceSetup: g}
	}
	if g.MachineType == "" {
		g.MachineType = workbenchMachineType
	}
	if g.GetVmImage() == nil && g.GetContainerImage() == nil {
		g.Image = &nbpb.GceSetup_VmImage{VmImage: &nbpb.VmImage{Project: "cloud-notebooks-managed", Image: &nbpb.VmImage_Family{Family: "workbench-instances"}}}
	} else if vm := g.GetVmImage(); vm != nil && vm.Project == "" {
		vm.Project = "cloud-notebooks-managed"
	}
	if g.BootDisk == nil {
		g.BootDisk = &nbpb.BootDisk{}
	}
	if g.BootDisk.DiskSizeGb == 0 {
		g.BootDisk.DiskSizeGb = workbenchBootDisk
	}
	if g.BootDisk.DiskType == nbpb.DiskType_DISK_TYPE_UNSPECIFIED {
		g.BootDisk.DiskType = nbpb.DiskType_PD_STANDARD
	}
	if len(g.DataDisks) == 0 {
		g.DataDisks = []*nbpb.DataDisk{{}}
	}
	if g.DataDisks[0].DiskSizeGb == 0 {
		g.DataDisks[0].DiskSizeGb = workbenchDataDisk
	}
	if g.DataDisks[0].DiskType == nbpb.DiskType_DISK_TYPE_UNSPECIFIED {
		g.DataDisks[0].DiskType = nbpb.DiskType_PD_STANDARD
	}
}

type workbenchService struct {
	nbpb.UnimplementedNotebookServiceServer
	s *Server
}

func workbenchMeta(target, verb string) *nbpb.OperationMetadata {
	return &nbpb.OperationMetadata{CreateTime: timestamppb.Now(), Target: target, Verb: verb, ApiVersion: "v2"}
}

// workbenchOp is a new operation of target's location, as Google names
// them (projects/P/locations/L/operations/ID).
func (s *Server) workbenchOp(target, verb string) string {
	p := strings.Split(target, "/")
	op := s.newOp(strings.Join(p[:4], "/"))
	_ = s.ops.SetMetadata(op, workbenchMeta(target, verb))
	return op
}

func (s *Server) workbenchDone(op string, resp proto.Message, err error) {
	if o, e := s.ops.Get(op); e == nil {
		if m, ok := o.Metadata.(*nbpb.OperationMetadata); ok {
			m = proto.Clone(m).(*nbpb.OperationMetadata)
			m.EndTime = timestamppb.Now()
			_ = s.ops.SetMetadata(op, m)
		}
	}
	if err != nil {
		_ = s.ops.Fail(op, err)
		return
	}
	_ = s.ops.Succeed(op, resp)
}

// workbenchVolume is an instance's data disk's Docker volume.
func workbenchVolume(in *nbpb.Instance) string { return "cb-workbench-" + in.GetId() }

// startLocked starts e on the host; the caller holds st.mu.
func (s *Server) startLocked(st *workbenchStore, e *workbenchEntry, project string) {
	g := e.I.GetGceSetup()
	idle, _ := workbenchIdle(g.GetMetadata())
	e.Running, e.Message = true, ""
	account := ""
	if sa := g.GetServiceAccounts(); len(sa) == 1 {
		account = sa[0].GetEmail()
	}
	o := NotebookRuntimeOptions{IdleTimeout: idle, IdleDisabled: idle == 0, Jupyter: &WorkbenchJupyter{
		Volume: workbenchVolume(e.I), Account: account, Mint: s.runAsToken,
		Name: lastSegment(e.I.GetName()), Zone: workbenchZone(e.I.GetName()), ID: e.I.GetId(), Attributes: g.GetMetadata(),
	}}
	st.host.Start(workbenchKey(e.I.GetName()), project, o)
}

func workbenchZone(name string) string {
	if p := strings.Split(name, "/"); len(p) >= 4 {
		return p[3]
	}
	return ""
}

// SetWorkbenchConsole names the console's host:port, the host proxy_uri
// is served on.
func (s *Server) SetWorkbenchConsole(addr func() string) {
	st := s.workbench()
	st.mu.Lock()
	st.console = addr
	st.mu.Unlock()
}

// workbenchProxyLabel is the first label of an instance's proxy host, as
// Google's is a hash of the instance's: HASH-dot-REGION.
func workbenchProxyLabel(in *nbpb.Instance) string {
	sum := sha256.Sum256([]byte(in.GetName() + "/" + in.GetId()))
	zone := workbenchZone(in.GetName())
	region := zone
	if i := strings.LastIndex(zone, "-"); i > 0 {
		region = zone[:i]
	}
	return hex.EncodeToString(sum[:8]) + "-dot-" + region
}

// WorkbenchProxyDomain is the domain the proxy hosts are under.
const WorkbenchProxyDomain = "notebooks.localhost"

// proxyURI is in's proxy_uri, "" while the console is not running.
func (st *workbenchStore) proxyURI(in *nbpb.Instance) string {
	if st.console == nil {
		return ""
	}
	addr := st.console()
	_, port, err := net.SplitHostPort(addr)
	if addr == "" || err != nil {
		return ""
	}
	return workbenchProxyLabel(in) + "." + WorkbenchProxyDomain + ":" + port
}

// WorkbenchJupyterLab is the JupyterLab behind a proxy host's first label:
// its loopback address and token, the instance's name, or an error when
// no ACTIVE instance has that proxy host.
func (s *Server) WorkbenchJupyterLab(label string) (addr, token, name string, err error) {
	st := s.workbench()
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, n := range sortedNames(st.instances) {
		e := st.instances[n]
		if workbenchProxyLabel(e.I) != label {
			continue
		}
		st.refresh(e)
		if e.I.GetState() != nbpb.State_ACTIVE {
			return "", "", n, apierror.FailedPrecondition("Instance %s is %s: start it to open JupyterLab", lastSegment(n), e.I.GetState())
		}
		h, ok := st.host.(JupyterHost)
		if !ok {
			return "", "", n, apierror.FailedPrecondition("this instance's notebook runtime does not serve JupyterLab")
		}
		a, t, ok := h.JupyterEndpoint(workbenchKey(n))
		if !ok {
			return "", "", n, apierror.FailedPrecondition("Instance %s's JupyterLab is not running", lastSegment(n))
		}
		return a, t, n, nil
	}
	return "", "", "", apierror.NotFound("no Workbench instance has the proxy host %s", label)
}

// awaitActive completes op with the instance when it is ACTIVE, or fails
// it.
func (s *Server) awaitActive(op string, e *workbenchEntry) {
	st := s.workbench()
	key := workbenchKey(e.I.GetName())
	deadline := time.Now().Add(runtimeStartWait)
	for {
		st.mu.Lock()
		state, msg := st.host.State(key)
		gone := st.instances[e.I.GetName()] != e || !e.Running
		st.mu.Unlock()
		if gone {
			s.workbenchDone(op, nil, apierror.Aborted("the instance was stopped or deleted before it started"))
			return
		}
		if state == "CONNECTED" {
			break
		}
		if state == "FAILED" || state == "DISCONNECTED" || time.Now().After(deadline) {
			if msg == "" {
				msg = "the instance did not start"
			}
			st.mu.Lock()
			e.Running, e.Message = false, msg
			e.I.HealthState, e.I.HealthInfo = nbpb.HealthState_UNHEALTHY, map[string]string{"error": msg}
			st.save()
			st.mu.Unlock()
			s.workbenchDone(op, nil, apierror.FailedPrecondition("%s", msg))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	st.mu.Lock()
	e.I.UpdateTime = timestamppb.New(st.now())
	v := st.view(e)
	st.save()
	st.mu.Unlock()
	s.workbenchDone(op, v, nil)
}

func (w *workbenchService) CreateInstance(ctx context.Context, req *nbpb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	project, _, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	id := req.GetInstanceId()
	if !workbenchIDRE.MatchString(id) {
		return nil, grpcError(apierror.InvalidArgument("instance_id %q must start with a letter followed by up to 62 lowercase letters, numbers, or hyphens (-), and cannot end with a hyphen", id))
	}
	if req.GetInstance() == nil {
		return nil, grpcError(apierror.InvalidArgument("instance is required"))
	}
	in := proto.Clone(req.GetInstance()).(*nbpb.Instance)
	if len(in.GetInstanceOwners()) > 1 {
		return nil, grpcError(apierror.InvalidArgument("instance_owners: Currently supports one owner only"))
	}
	if err := checkNotebookLabels(in.GetLabels()); err != nil {
		return nil, grpcError(err)
	}
	if err := checkWorkbenchSetup(in.GetGceSetup()); err != nil {
		return nil, grpcError(err)
	}
	if err := runnableWorkbench(in.GetGceSetup()); err != nil {
		return nil, grpcError(err)
	}
	if sa := in.GetGceSetup().GetServiceAccounts(); len(sa) == 1 {
		if err := w.s.checkActAs(ctx, project, sa[0].GetEmail()); err != nil {
			return nil, grpcError(err)
		}
	}
	st := w.s.workbench()
	st.mu.Lock()
	defer st.mu.Unlock()
	name := req.GetParent() + "/instances/" + id
	if _, ok := st.instances[name]; ok {
		return nil, grpcError(apierror.AlreadyExists("Instance %s already exists", name))
	}
	if st.host == nil {
		return nil, grpcError(workbenchUnavailable())
	}
	workbenchDefaults(in)
	now := st.now()
	in.Name, in.Id, in.Creator, in.ProxyUri = name, uuid.NewString(), w.s.callerOf(ctx), ""
	in.CreateTime, in.UpdateTime = timestamppb.New(now), timestamppb.New(now)
	in.State, in.HealthState, in.HealthInfo, in.UpgradeHistory = nbpb.State_PROVISIONING, nbpb.HealthState_HEALTH_STATE_UNSPECIFIED, nil, nil
	e := &workbenchEntry{I: in}
	st.instances[name] = e
	w.s.startLocked(st, e, project)
	st.save()
	op := w.s.workbenchOp(name, "create")
	go w.s.awaitActive(op, e)
	return w.s.opView(op)
}

func (w *workbenchService) GetInstance(_ context.Context, req *nbpb.GetInstanceRequest) (*nbpb.Instance, error) {
	st := w.s.workbench()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.instance(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return st.view(e), nil
}

func (w *workbenchService) ListInstances(_ context.Context, req *nbpb.ListInstancesRequest) (*nbpb.ListInstancesResponse, error) {
	project, location, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	st := w.s.workbench()
	st.mu.Lock()
	var all []*nbpb.Instance
	for _, k := range sortedNames(st.instances) {
		// "-" is every location (AIP-159).
		if strings.HasPrefix(k, req.GetParent()+"/") || location == "-" && strings.HasPrefix(k, "projects/"+project+"/locations/") {
			all = append(all, st.view(st.instances[k]))
		}
	}
	st.mu.Unlock()
	all, err = aipfilter.Apply(req.GetFilter(), all, workbenchFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, workbenchOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("workbench:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &nbpb.ListInstancesResponse{Instances: items, NextPageToken: next}, nil
}

func (w *workbenchService) UpdateInstance(_ context.Context, req *nbpb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	in := req.GetInstance()
	if in == nil {
		return nil, grpcError(apierror.InvalidArgument("instance is required"))
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcError(apierror.InvalidArgument("update_mask is required"))
	}
	for i, p := range paths {
		p = camelToSnake(p)
		paths[i] = p
		if !workbenchMasks[p] {
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated: the paths are labels, gce_setup.metadata, gce_setup.tags and gce_setup.machine_type", p))
		}
	}
	st := w.s.workbench()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.instance(in.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	st.refresh(e)
	next := proto.Clone(e.I).(*nbpb.Instance)
	g := next.GetGceSetup()
	for _, p := range paths {
		switch p {
		case "labels":
			if err := checkNotebookLabels(in.GetLabels()); err != nil {
				return nil, grpcError(err)
			}
			next.Labels = in.GetLabels()
		case "gce_setup.metadata":
			if _, err := workbenchIdle(in.GetGceSetup().GetMetadata()); err != nil {
				return nil, grpcError(err)
			}
			g.Metadata = in.GetGceSetup().GetMetadata()
		case "gce_setup.tags":
			g.Tags = in.GetGceSetup().GetTags()
		case "gce_setup.machine_type":
			mt := in.GetGceSetup().GetMachineType()
			if !machineTypeRE.MatchString(mt) {
				return nil, grpcError(apierror.InvalidArgument("gce_setup.machine_type %q is not a machine type", mt))
			}
			if e.I.GetState() != nbpb.State_STOPPED {
				return nil, grpcError(apierror.FailedPrecondition("the instance must be STOPPED to change its machine type"))
			}
			g.MachineType = mt
		}
	}
	next.UpdateTime = timestamppb.New(st.now())
	e.I = next
	st.save()
	op := w.s.workbenchOp(next.GetName(), "update")
	w.s.workbenchDone(op, st.view(e), nil)
	return w.s.opView(op)
}

func (w *workbenchService) DeleteInstance(_ context.Context, req *nbpb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	st := w.s.workbench()
	st.mu.Lock()
	e, err := st.instance(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	e.Running = false
	delete(st.instances, req.GetName())
	st.save()
	host := st.host
	st.mu.Unlock()
	if host != nil {
		_ = host.Stop(workbenchKey(req.GetName()))
		// The data disk goes with the instance.
		if h, ok := host.(JupyterHost); ok {
			_ = h.DeleteDisk(workbenchVolume(e.I))
		}
	}
	op := w.s.workbenchOp(req.GetName(), "delete")
	w.s.workbenchDone(op, &emptypb.Empty{}, nil)
	return w.s.opView(op)
}

func (w *workbenchService) StartInstance(_ context.Context, req *nbpb.StartInstanceRequest) (*longrunningpb.Operation, error) {
	return w.start(req.GetName(), "start", false)
}

func (w *workbenchService) ResetInstance(_ context.Context, req *nbpb.ResetInstanceRequest) (*longrunningpb.Operation, error) {
	return w.start(req.GetName(), "reset", true)
}

// start starts the instance, stopping it first when reset.
func (w *workbenchService) start(name, verb string, reset bool) (*longrunningpb.Operation, error) {
	st := w.s.workbench()
	st.mu.Lock()
	e, err := st.instance(name)
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	if st.host == nil {
		st.mu.Unlock()
		return nil, grpcError(workbenchUnavailable())
	}
	st.refresh(e)
	if reset && !e.Running {
		st.mu.Unlock()
		return nil, grpcError(apierror.FailedPrecondition("Instance %s is STOPPED: reset needs a running instance", name))
	}
	if err := runnableWorkbench(e.I.GetGceSetup()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	if reset {
		_ = st.host.Stop(workbenchKey(name))
		e.Running = false
	}
	if !e.Running {
		project, _, _, _ := parseChild("name", "instances", name)
		w.s.startLocked(st, e, project)
		e.I.UpdateTime = timestamppb.New(st.now())
		st.save()
	}
	st.mu.Unlock()
	op := w.s.workbenchOp(name, verb)
	go w.s.awaitActive(op, e)
	return w.s.opView(op)
}

func (w *workbenchService) StopInstance(_ context.Context, req *nbpb.StopInstanceRequest) (*longrunningpb.Operation, error) {
	st := w.s.workbench()
	st.mu.Lock()
	e, err := st.instance(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	e.Running, e.Message = false, ""
	e.I.UpdateTime = timestamppb.New(st.now())
	st.save()
	host := st.host
	st.mu.Unlock()
	if host != nil {
		_ = host.Stop(workbenchKey(req.GetName()))
	}
	st.mu.Lock()
	v := st.view(e)
	st.mu.Unlock()
	op := w.s.workbenchOp(req.GetName(), "stop")
	w.s.workbenchDone(op, v, nil)
	return w.s.opView(op)
}

func (w *workbenchService) CheckInstanceUpgradability(_ context.Context, req *nbpb.CheckInstanceUpgradabilityRequest) (*nbpb.CheckInstanceUpgradabilityResponse, error) {
	st := w.s.workbench()
	st.mu.Lock()
	_, err := st.instance(req.GetNotebookInstance())
	st.mu.Unlock()
	if err != nil {
		return nil, grpcError(err)
	}
	return &nbpb.CheckInstanceUpgradabilityResponse{Upgradeable: false, UpgradeInfo: "The instance is up to date: it runs this CloudBurrow instance's notebook runtime image."}, nil
}

func (w *workbenchService) UpgradeInstance(_ context.Context, req *nbpb.UpgradeInstanceRequest) (*longrunningpb.Operation, error) {
	st := w.s.workbench()
	st.mu.Lock()
	_, err := st.instance(req.GetName())
	st.mu.Unlock()
	if err != nil {
		return nil, grpcError(err)
	}
	return nil, grpcError(apierror.FailedPrecondition("Instance %s is not upgradeable: it runs this CloudBurrow instance's notebook runtime image (CheckInstanceUpgradability)", req.GetName()))
}

// WorkbenchInstance is the instance name, as GetInstance answers it.
func (s *Server) WorkbenchInstance(name string) (*nbpb.Instance, error) {
	st := s.workbench()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.instance(name)
	if err != nil {
		return nil, err
	}
	return st.view(e), nil
}
