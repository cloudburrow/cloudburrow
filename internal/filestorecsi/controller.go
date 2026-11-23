package filestorecsi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// API is the served Filestore API the controller provisions through.
type API interface {
	CreateInstance(context.Context, *filestorepb.CreateInstanceRequest) (*longrunningpb.Operation, error)
	GetInstance(context.Context, *filestorepb.GetInstanceRequest) (*filestorepb.Instance, error)
	DeleteInstance(context.Context, *filestorepb.DeleteInstanceRequest) (*longrunningpb.Operation, error)
	WaitDone(ctx context.Context, name string)
	Operation(name string) (*longrunningpb.Operation, error)
}

// Cluster is a Kubernetes Engine cluster whose add-on is enabled.
type Cluster struct {
	// Kind is its kind cluster, whose nodes are containers labelled
	// io.x-k8s.kind.cluster=Kind; Kubeconfig reaches its API server.
	Kind, Kubeconfig           string
	Project, Location, Network string
}

// Runner runs a program (kubectl, docker); injected for tests.
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error
}

// Controller installs the driver in each cluster and provisions its
// volumes.
type Controller struct {
	API      func() API
	Clusters func() []Cluster
	// Binary is the Linux cloudburrow-storage build for arch, which runs
	// the node plugin.
	Binary func(arch string) ([]byte, error)
	Runner Runner
	Logf   func(format string, args ...any)
	// Every is how often each cluster is looked at (3s when zero).
	Every time.Duration

	mu        sync.Mutex
	installed map[string]time.Time
	nodes     map[string]time.Time
	failed    map[string]string
}

func (c *Controller) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Controller) run(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	var out strings.Builder
	var in io.Reader
	if stdin != "" {
		in = strings.NewReader(stdin)
	}
	err := c.Runner.Run(ctx, in, &out, name, args...)
	return out.String(), err
}

func (c *Controller) kubectl(ctx context.Context, cl Cluster, stdin string, args ...string) (string, error) {
	return c.run(ctx, stdin, "kubectl", append([]string{"--kubeconfig", cl.Kubeconfig}, args...)...)
}

// Run looks at every cluster every Every until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	every := c.Every
	if every <= 0 {
		every = 3 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, cl := range c.Clusters() {
			c.Sync(ctx, cl)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync installs the driver in cl, starts its node plugins and provisions
// and deletes its volumes.
func (c *Controller) Sync(ctx context.Context, cl Cluster) {
	c.mu.Lock()
	if c.installed == nil {
		c.installed, c.nodes, c.failed = map[string]time.Time{}, map[string]time.Time{}, map[string]string{}
	}
	fresh := time.Since(c.installed[cl.Kind]) < time.Minute
	c.mu.Unlock()
	if !fresh {
		if _, err := c.kubectl(ctx, cl, Manifest(cl.Network), "apply", "-f", "-"); err != nil {
			c.logf("filestore CSI: install in %s: %v", cl.Kind, err)
			return
		}
		c.mu.Lock()
		c.installed[cl.Kind] = time.Now()
		c.mu.Unlock()
	}
	c.syncNodes(ctx, cl)
	if err := c.provision(ctx, cl); err != nil {
		c.logf("filestore CSI: %s: %v", cl.Kind, err)
	}
}

// StorageClasses are the classes GKE installs, by name: the tier each
// makes.
var StorageClasses = map[string]string{
	"standard-rwx":   "standard",
	"premium-rwx":    "premium",
	"zonal-rwx":      "zonal",
	"regional-rwx":   "regional",
	"enterprise-rwx": "enterprise",
}

// Manifest is the driver's CSIDriver and StorageClasses.
func Manifest(network string) string {
	var b strings.Builder
	b.WriteString(`apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: ` + Driver + `
  labels: {addonmanager.kubernetes.io/mode: Reconcile, k8s-app: gcp-filestore-csi-driver}
spec:
  attachRequired: false
  podInfoOnMount: false
  volumeLifecycleModes: [Persistent]
`)
	names := make([]string, 0, len(StorageClasses))
	for n := range StorageClasses {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, `---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
  labels: {addonmanager.kubernetes.io/mode: EnsureExists, k8s-app: gcp-filestore-csi-driver}
provisioner: %s
parameters: {tier: %s, network: %s}
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
`, n, Driver, StorageClasses[n], network)
	}
	return b.String()
}

// nodePath is where the node plugin's binary is put in a node.
const nodePath = "/usr/local/bin/cloudburrow-filestore-csi"

// nodeScript says "running" when the plugin runs and is registered.
const nodeScript = `test -S ` + RegistrationDir + `/` + Driver + `-reg.sock && pgrep -f 'filestore-csi-node' >/dev/null && echo running`

// syncNodes starts the node plugin in each node that does not run it.
func (c *Controller) syncNodes(ctx context.Context, cl Cluster) {
	out, err := c.run(ctx, "", "docker", "ps", "--filter", "label=io.x-k8s.kind.cluster="+cl.Kind, "--format", "{{.Names}}")
	if err != nil {
		return
	}
	for _, node := range strings.Fields(out) {
		c.mu.Lock()
		recent := time.Since(c.nodes[node]) < 15*time.Second
		c.mu.Unlock()
		if recent {
			continue
		}
		if got, _ := c.run(ctx, "", "docker", "exec", node, "sh", "-c", nodeScript); strings.TrimSpace(got) == "running" {
			c.mu.Lock()
			c.nodes[node] = time.Now()
			c.mu.Unlock()
			continue
		}
		if err := c.startNode(ctx, node); err != nil {
			c.logf("filestore CSI: node %s: %v", node, err)
			continue
		}
		c.mu.Lock()
		c.nodes[node] = time.Now()
		c.mu.Unlock()
	}
}

func (c *Controller) startNode(ctx context.Context, node string) error {
	arch, err := c.run(ctx, "", "docker", "exec", node, "uname", "-m")
	if err != nil {
		return err
	}
	goarch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(arch)]
	if goarch == "" {
		return fmt.Errorf("unknown machine %q", strings.TrimSpace(arch))
	}
	bin, err := c.Binary(goarch)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "cloudburrow-filestore-csi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "cloudburrow-filestore-csi")
	if err := os.WriteFile(path, bin, 0o755); err != nil {
		return err
	}
	if _, err := c.run(ctx, "", "docker", "cp", path, node+":"+nodePath); err != nil {
		return err
	}
	_, err = c.run(ctx, "", "docker", "exec", "-d", node, "sh", "-c",
		"exec "+nodePath+" filestore-csi-node --node-id "+node+" >>/var/log/filestore-csi-node.log 2>&1")
	if err == nil {
		c.logf("filestore CSI: started the node plugin in %s", node)
	}
	return err
}

// objects as the controller reads them.
type objMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	UID         string            `json:"uid,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type storageClass struct {
	Metadata      objMeta           `json:"metadata"`
	Provisioner   string            `json:"provisioner"`
	Parameters    map[string]string `json:"parameters"`
	ReclaimPolicy string            `json:"reclaimPolicy"`
	MountOptions  []string          `json:"mountOptions"`
}

type pvc struct {
	Metadata objMeta `json:"metadata"`
	Spec     struct {
		StorageClassName *string  `json:"storageClassName"`
		AccessModes      []string `json:"accessModes"`
		VolumeName       string   `json:"volumeName"`
		Resources        struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type pv struct {
	Metadata objMeta `json:"metadata"`
	Spec     struct {
		PersistentVolumeReclaimPolicy string `json:"persistentVolumeReclaimPolicy"`
		CSI                           *struct {
			Driver       string `json:"driver"`
			VolumeHandle string `json:"volumeHandle"`
		} `json:"csi"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type list[T any] struct {
	Items []T `json:"items"`
}

func getList[T any](ctx context.Context, c *Controller, cl Cluster, args ...string) ([]T, error) {
	out, err := c.kubectl(ctx, cl, "", append([]string{"get"}, append(args, "-o", "json")...)...)
	if err != nil {
		return nil, err
	}
	var l list[T]
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// provision makes the instances and volumes of the driver's pending
// claims, and deletes the released volumes of reclaim policy Delete.
func (c *Controller) provision(ctx context.Context, cl Cluster) error {
	classes, err := getList[storageClass](ctx, c, cl, "storageclass")
	if err != nil {
		return err
	}
	mine := map[string]storageClass{}
	for _, sc := range classes {
		if sc.Provisioner == Driver {
			mine[sc.Metadata.Name] = sc
		}
	}
	claims, err := getList[pvc](ctx, c, cl, "pvc", "-A")
	if err != nil {
		return err
	}
	for _, p := range claims {
		if p.Status.Phase != "Pending" || p.Spec.VolumeName != "" || p.Spec.StorageClassName == nil {
			continue
		}
		sc, ok := mine[*p.Spec.StorageClassName]
		if !ok {
			continue
		}
		// WaitForFirstConsumer: the scheduler names the node first.
		if p.Metadata.Annotations["volume.kubernetes.io/selected-node"] == "" {
			continue
		}
		if err := c.provisionOne(ctx, cl, sc, p); err != nil {
			c.event(ctx, cl, p, err)
		}
	}
	vols, err := getList[pv](ctx, c, cl, "pv")
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Spec.CSI == nil || v.Spec.CSI.Driver != Driver || v.Status.Phase != "Released" || v.Spec.PersistentVolumeReclaimPolicy != "Delete" {
			continue
		}
		if v.Metadata.Annotations["pv.kubernetes.io/provisioned-by"] != Driver {
			continue
		}
		if err := c.deleteOne(ctx, cl, v.Metadata.Name, v.Spec.CSI.VolumeHandle); err != nil {
			c.logf("filestore CSI: delete %s: %v", v.Metadata.Name, err)
		}
	}
	return nil
}

// event reports a provisioning failure on the claim, once per message.
func (c *Controller) event(ctx context.Context, cl Cluster, p pvc, err error) {
	key := cl.Kind + "/" + p.Metadata.Namespace + "/" + p.Metadata.Name
	c.mu.Lock()
	same := c.failed[key] == err.Error()
	c.failed[key] = err.Error()
	c.mu.Unlock()
	if same {
		return
	}
	c.logf("filestore CSI: provision %s: %v", key, err)
	ev := fmt.Sprintf(`apiVersion: v1
kind: Event
metadata: {generateName: %s., namespace: %s}
involvedObject: {kind: PersistentVolumeClaim, name: %s, namespace: %s, uid: %s}
reason: ProvisioningFailed
type: Warning
source: {component: %s}
message: %q
`, p.Metadata.Name, p.Metadata.Namespace, p.Metadata.Name, p.Metadata.Namespace, p.Metadata.UID, Driver, "failed to provision volume with StorageClass: "+err.Error())
	_, _ = c.kubectl(ctx, cl, ev, "create", "-f", "-")
}

// tierOf is a StorageClass's tier parameter as a tier, with its minimum
// capacity in GiB.
func tierOf(param string) (filestorepb.Instance_Tier, int64, error) {
	switch strings.ToLower(param) {
	case "", "standard", "basic_hdd":
		return filestorepb.Instance_BASIC_HDD, 1024, nil
	case "premium", "basic_ssd":
		return filestorepb.Instance_BASIC_SSD, 2560, nil
	case "zonal":
		return filestorepb.Instance_ZONAL, 1024, nil
	case "regional":
		return filestorepb.Instance_REGIONAL, 1024, nil
	case "enterprise":
		return filestorepb.Instance_ENTERPRISE, 1024, nil
	case "high_scale_ssd", "high-scale-ssd":
		return filestorepb.Instance_HIGH_SCALE_SSD, 10240, nil
	}
	return 0, 0, fmt.Errorf("parameter tier %q is not a Filestore tier", param)
}

// GiB is a quantity in GiB, rounded up.
func GiB(q string) (int64, error) {
	units := []struct {
		suffix string
		bytes  float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}}
	mult := 1.0
	num := q
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			mult, num = u.bytes, strings.TrimSuffix(q, u.suffix)
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("storage request %q is not a quantity", q)
	}
	b := v * mult
	g := int64(b / (1 << 30))
	if float64(g)*(1<<30) < b {
		g++
	}
	return g, nil
}

// instanceID is the instance a claim makes, as GKE names it.
func instanceID(uid string) string { return "pvc-" + uid }

func (c *Controller) provisionOne(ctx context.Context, cl Cluster, sc storageClass, p pvc) error {
	api := c.API()
	if api == nil {
		return fmt.Errorf("the Filestore API is not served")
	}
	tier, minGiB, err := tierOf(sc.Parameters["tier"])
	if err != nil {
		return err
	}
	req, err := GiB(p.Spec.Resources.Requests["storage"])
	if err != nil {
		return err
	}
	capGiB := max(req, minGiB)
	location := cl.Location
	if tier == filestorepb.Instance_REGIONAL || tier == filestorepb.Instance_ENTERPRISE {
		if i := strings.LastIndex(location, "-"); i > 0 && strings.Count(location, "-") == 2 {
			location = location[:i]
		}
	}
	network := sc.Parameters["network"]
	if network == "" {
		network = cl.Network
	}
	protocol := filestorepb.Instance_NFS_V3
	switch strings.ToUpper(sc.Parameters["protocol"]) {
	case "", "NFS_V3":
	case "NFS_V4_1":
		protocol = filestorepb.Instance_NFS_V4_1
	default:
		return fmt.Errorf("parameter protocol %q is not NFS_V3 or NFS_V4_1", sc.Parameters["protocol"])
	}
	nc := &filestorepb.NetworkConfig{Network: network, ReservedIpRange: sc.Parameters["reserved-ipv4-cidr"]}
	if r := sc.Parameters["reserved-ip-range"]; r != "" {
		nc.ReservedIpRange = r
	}
	if strings.EqualFold(sc.Parameters["connect-mode"], "PRIVATE_SERVICE_ACCESS") {
		nc.ConnectMode = filestorepb.NetworkConfig_PRIVATE_SERVICE_ACCESS
	}
	pvName := instanceID(p.Metadata.UID)
	labels := map[string]string{
		"kubernetes_io_created-for_pv_name":       pvName,
		"kubernetes_io_created-for_pvc_name":      labelValue(p.Metadata.Name),
		"kubernetes_io_created-for_pvc_namespace": labelValue(p.Metadata.Namespace),
		"storage_gke_io_created-by":               "filestore_csi_storage_gke_io",
	}
	if strings.EqualFold(sc.Parameters["instance-storageclass-label"], "true") {
		labels["storage_gke_io_storage-class-name"] = labelValue(sc.Metadata.Name)
	}
	for _, kv := range strings.Split(sc.Parameters["labels"], ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			labels[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	share := &filestorepb.FileShareConfig{Name: "vol1", CapacityGb: capGiB}
	if raw := sc.Parameters["nfs-export-options-on-create"]; raw != "" {
		var opts []struct {
			AccessMode string   `json:"accessMode"`
			IPRanges   []string `json:"ipRanges"`
			SquashMode string   `json:"squashMode"`
			AnonUID    int64    `json:"anonUid"`
			AnonGID    int64    `json:"anonGid"`
		}
		if err := json.Unmarshal([]byte(raw), &opts); err != nil {
			return fmt.Errorf("parameter nfs-export-options-on-create: %v", err)
		}
		for _, o := range opts {
			share.NfsExportOptions = append(share.NfsExportOptions, &filestorepb.NfsExportOptions{IpRanges: o.IPRanges,
				AccessMode: filestorepb.NfsExportOptions_AccessMode(filestorepb.NfsExportOptions_AccessMode_value[o.AccessMode]),
				SquashMode: filestorepb.NfsExportOptions_SquashMode(filestorepb.NfsExportOptions_SquashMode_value[o.SquashMode]),
				AnonUid:    o.AnonUID, AnonGid: o.AnonGID})
		}
	}
	parent := "projects/" + cl.Project + "/locations/" + location
	name := parent + "/instances/" + pvName
	in, err := api.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: name})
	if status.Code(err) == codes.NotFound {
		op, err := api.CreateInstance(ctx, &filestorepb.CreateInstanceRequest{Parent: parent, InstanceId: pvName, Instance: &filestorepb.Instance{
			Tier: tier, Protocol: protocol, FileShares: []*filestorepb.FileShareConfig{share}, Networks: []*filestorepb.NetworkConfig{nc},
			Labels: labels, Description: "Filestore instance of PersistentVolumeClaim " + p.Metadata.Namespace + "/" + p.Metadata.Name}})
		if err != nil {
			return err
		}
		api.WaitDone(ctx, op.GetName())
		done, err := api.Operation(op.GetName())
		if err != nil {
			return err
		}
		if e := done.GetError(); e != nil {
			return fmt.Errorf("%s", e.GetMessage())
		}
		in, err = api.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: name})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if in.GetState() != filestorepb.Instance_READY {
		return fmt.Errorf("instance %s is %s", name, in.GetState())
	}
	ip := in.GetNetworks()[0].GetIpAddresses()[0]
	reclaim := sc.ReclaimPolicy
	if reclaim == "" {
		reclaim = "Delete"
	}
	modes := p.Spec.AccessModes
	if len(modes) == 0 {
		modes = []string{"ReadWriteMany"}
	}
	attrs := map[string]string{"ip": ip, "volume": "vol1"}
	if protocol == filestorepb.Instance_NFS_V4_1 {
		attrs["protocol"] = "NFS_V4_1"
	}
	vol := map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": map[string]any{"name": pvName, "annotations": map[string]string{"pv.kubernetes.io/provisioned-by": Driver}},
		"spec": map[string]any{
			"capacity":                      map[string]string{"storage": strconv.FormatInt(capGiB, 10) + "Gi"},
			"accessModes":                   modes,
			"persistentVolumeReclaimPolicy": reclaim,
			"storageClassName":              sc.Metadata.Name,
			"mountOptions":                  sc.MountOptions,
			"claimRef":                      map[string]string{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "namespace": p.Metadata.Namespace, "name": p.Metadata.Name, "uid": p.Metadata.UID},
			"csi": map[string]any{"driver": Driver, "volumeHandle": "modeInstance/" + location + "/" + pvName + "/vol1",
				"volumeAttributes": attrs},
		},
	}
	b, _ := json.Marshal(vol)
	if _, err := c.kubectl(ctx, cl, string(b), "create", "-f", "-"); err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
		return err
	}
	c.logf("filestore CSI: provisioned %s/%s as %s (%s, %d GiB, %s)", p.Metadata.Namespace, p.Metadata.Name, name, tier, capGiB, ip)
	return nil
}

// labelValue is s as a label value: lowercase letters, digits, - and _.
func labelValue(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

// deleteOne deletes a released volume's instance, then the volume.
func (c *Controller) deleteOne(ctx context.Context, cl Cluster, pvName, handle string) error {
	parts := strings.Split(handle, "/")
	if len(parts) != 4 || parts[0] != "modeInstance" {
		return fmt.Errorf("volume handle %q is not modeInstance/LOCATION/INSTANCE/SHARE", handle)
	}
	api := c.API()
	if api == nil {
		return fmt.Errorf("the Filestore API is not served")
	}
	name := "projects/" + cl.Project + "/locations/" + parts[1] + "/instances/" + parts[2]
	op, err := api.DeleteInstance(ctx, &filestorepb.DeleteInstanceRequest{Name: name, Force: true})
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		return err
	default:
		api.WaitDone(ctx, op.GetName())
		done, err := api.Operation(op.GetName())
		if err != nil {
			return err
		}
		if e := done.GetError(); e != nil {
			return fmt.Errorf("%s", e.GetMessage())
		}
	}
	if _, err := c.kubectl(ctx, cl, "", "delete", "pv", pvName, "--ignore-not-found", "--wait=false"); err != nil {
		return err
	}
	c.logf("filestore CSI: deleted %s and its instance %s", pvName, name)
	return nil
}
