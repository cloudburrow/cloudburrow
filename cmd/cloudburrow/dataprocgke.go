package main

// Dataproc on GKE (#2162): a virtual cluster's Spark on a Kubernetes
// Engine cluster of the instance (a kind cluster). The cluster's
// namespace gets a spark service account that may manage its pods (the
// edit role); each job's driver is a pod in it, on the SPARK_DRIVER (or
// DEFAULT) node pool, running spark-submit in client mode against the
// API server (k8s://https://kubernetes.default.svc), its executors pods
// on the SPARK_EXECUTOR (or DEFAULT) pool. The driver's image is the
// Spark runner's Apache Spark 3.5 image (its R variant for SparkR), which
// the nodes pull by digest. The driver's output, from its pod's log, is
// the job's.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	containerpb "cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/service/dataproc"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// dataprocGKE is the dataproc.VirtualEngine of the instance's Kubernetes
// Engine.
type dataprocGKE struct {
	api func() *gke.Server
	// dir holds each virtual cluster's kubeconfig and staged job files.
	dir string
}

func (d dataprocGKE) StartVirtual(ctx context.Context, spec dataproc.VirtualSpec) (dataproc.Nodes, error) {
	api := d.api()
	if api == nil {
		return nil, &sparkproc.Unavailable{Msg: "this instance serves no Kubernetes Engine"}
	}
	for _, p := range spec.Pools {
		if err := ensurePool(ctx, api, p); err != nil {
			return nil, err
		}
	}
	a, ok := api.ClusterAccess(spec.Target)
	if !ok {
		return nil, fmt.Errorf("the GKE cluster %s is not RUNNING", spec.Target)
	}
	dir := filepath.Join(d.dir, spec.UUID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	n := &virtualNodes{spec: spec, dir: dir, kubeconfig: filepath.Join(dir, "kubeconfig")}
	if err := os.WriteFile(n.kubeconfig, kubeconfigOf(a), 0o600); err != nil {
		return nil, err
	}
	ns := spec.Namespace
	manifest := fmt.Sprintf(`{"apiVersion":"v1","kind":"List","items":[
{"apiVersion":"v1","kind":"Namespace","metadata":{"name":%[1]q,"labels":{"dataproc.googleapis.com/cluster-name":%[2]q,"dataproc.googleapis.com/cluster-uuid":%[3]q}}},
{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"spark","namespace":%[1]q}},
{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"spark-edit","namespace":%[1]q},
 "roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"edit"},
 "subjects":[{"kind":"ServiceAccount","name":"spark","namespace":%[1]q}]}]}`, ns, spec.Name, spec.UUID)
	if out, err := n.kubectl(ctx, []byte(manifest), "apply", "-f", "-"); err != nil {
		return nil, fmt.Errorf("set up namespace %s on %s: %v: %s", ns, spec.Target, err, out)
	}
	return n, nil
}

// ensurePool creates a node pool target that does not exist, from its
// node pool config, as Dataproc does, and waits for it.
func ensurePool(ctx context.Context, api *gke.Server, p dataproc.VirtualPool) error {
	_, err := api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: p.Name})
	if status.Code(err) != codes.NotFound {
		return err
	}
	if p.Config == nil {
		return fmt.Errorf("node pool %s does not exist and its target gives no node_pool_config to create it with", p.Name)
	}
	count := int32(1)
	if m := p.Config.GetAutoscaling().GetMinNodeCount(); m > 0 {
		count = m
	}
	np := &containerpb.NodePool{Name: p.ID, InitialNodeCount: count, Locations: p.Config.GetLocations(),
		Config: &containerpb.NodeConfig{MachineType: p.Config.GetConfig().GetMachineType(), Spot: p.Config.GetConfig().GetSpot(),
			Preemptible: p.Config.GetConfig().GetPreemptible()}}
	if as := p.Config.GetAutoscaling(); as != nil {
		np.Autoscaling = &containerpb.NodePoolAutoscaling{Enabled: true, MinNodeCount: as.GetMinNodeCount(), MaxNodeCount: as.GetMaxNodeCount()}
	}
	parent := strings.TrimSuffix(p.Name, "/nodePools/"+p.ID)
	if _, err := api.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: parent, NodePool: np}); err != nil {
		return fmt.Errorf("create node pool %s: %w", p.Name, err)
	}
	for {
		got, err := api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: p.Name})
		if err != nil {
			return err
		}
		switch got.GetStatus() {
		case containerpb.NodePool_RUNNING:
			return nil
		case containerpb.NodePool_ERROR, containerpb.NodePool_RUNNING_WITH_ERROR:
			return fmt.Errorf("node pool %s: %s", p.Name, got.GetStatusMessage())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// kubeconfigOf is a kubeconfig for a cluster's access.
func kubeconfigOf(a gke.Access) []byte {
	kc := map[string]any{"apiVersion": "v1", "kind": "Config", "current-context": "c",
		"clusters": []any{map[string]any{"name": "c", "cluster": map[string]any{"server": "https://" + a.Endpoint, "certificate-authority-data": a.CACert}}},
		"users":    []any{map[string]any{"name": "u", "user": map[string]any{"client-certificate-data": a.ClientCert, "client-key-data": a.ClientKey}}},
		"contexts": []any{map[string]any{"name": "c", "context": map[string]any{"cluster": "c", "user": "u"}}}}
	b, _ := json.Marshal(kc)
	return b
}

// virtualNodes are a virtual cluster's namespace (dataproc.Nodes).
type virtualNodes struct {
	spec       dataproc.VirtualSpec
	dir        string
	kubeconfig string
}

// kubectl runs kubectl (internal/k8s, the package that execs it) on the
// cluster's kubeconfig.
func (n *virtualNodes) kubectl(ctx context.Context, stdin []byte, args ...string) (string, error) {
	return k8s.Subprocess{}.Run(ctx, string(stdin), append([]string{"--kubeconfig", n.kubeconfig}, args...)...)
}

func (n *virtualNodes) Scale(context.Context, int) error {
	return fmt.Errorf("a Dataproc on GKE cluster has no workers to resize")
}
func (n *virtualNodes) Workers() int { return 0 }
func (n *virtualNodes) Stop(context.Context) error {
	return fmt.Errorf("a Dataproc on GKE cluster cannot be stopped")
}
func (n *virtualNodes) Start(context.Context) error { return nil }
func (n *virtualNodes) Web(context.Context) map[string]string {
	return nil
}
func (n *virtualNodes) HistoryDir() string                                   { return "" }
func (n *virtualNodes) SetNodeGroup(context.Context, string, []string) error { return nil }

// Delete removes the cluster's namespace, its pods with it.
func (n *virtualNodes) Delete() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, _ = n.kubectl(ctx, nil, "delete", "namespace", n.spec.Namespace, "--ignore-not-found", "--wait=false")
	_ = os.RemoveAll(n.dir)
}

// Diagnose is the namespace's pods and events.
func (n *virtualNodes) Diagnose(ctx context.Context, summary string) ([]byte, error) {
	pods, _ := n.kubectl(ctx, nil, "-n", n.spec.Namespace, "get", "pods", "-o", "wide")
	events, _ := n.kubectl(ctx, nil, "-n", n.spec.Namespace, "get", "events")
	return []byte(summary + "\n\n== pods\n" + pods + "\n== events\n" + events), nil
}

// usrLibSpark links the image's Spark at Google's paths, as the Spark
// runner's run.sh does: a job's file:///usr/lib/spark/ URIs name them.
const usrLibSpark = `uls=/tmp/cb-usr-lib-spark
mkdir -p "$uls/examples/jars"
for f in /opt/spark/*; do [ "$(basename "$f")" = examples ] || ln -sfn "$f" "$uls/$(basename "$f")"; done
for f in /opt/spark/examples/jars/*; do ln -sfn "$f" "$uls/examples/jars/$(basename "$f")"; done
for f in /opt/spark/examples/jars/spark-examples_*.jar; do [ -e "$f" ] && ln -sfn "$f" "$uls/examples/jars/spark-examples.jar"; done
`

// virtualPath is a job file's path in the driver pod: a Cloud Storage
// object's copy in /work, or a file:// URI's path (/usr/lib/spark/ the
// image's Spark).
func virtualPath(f sparkproc.File) string {
	if p, ok := strings.CutPrefix(f.URI, "file://"); ok {
		if rest, ok := strings.CutPrefix(p, "/usr/lib/spark/"); ok {
			return "/tmp/cb-usr-lib-spark/" + rest
		}
		return p
	}
	return "/work/" + filepath.Base(f.Base())
}

// sparkSubmit is the driver's spark-submit (spark-sql for a Spark SQL
// job) command line.
func (n *virtualNodes) sparkSubmit(job sparkproc.Job, pod, image string) []string {
	bin := "/opt/spark/bin/spark-submit"
	if job.Kind == sparkproc.KindSparkSQL {
		bin = "/opt/spark/bin/spark-sql"
	}
	args := []string{bin, "--master", "k8s://https://kubernetes.default.svc", "--deploy-mode", "client",
		"--name", job.SparkJobID}
	conf := map[string]string{
		"spark.kubernetes.namespace":                                            n.spec.Namespace,
		"spark.kubernetes.container.image":                                      image,
		"spark.kubernetes.driver.pod.name":                                      pod,
		"spark.kubernetes.authenticate.serviceAccountName":                      "spark",
		"spark.kubernetes.executor.podNamePrefix":                               pod,
		"spark.driver.bindAddress":                                              "0.0.0.0",
		"spark.driver.port":                                                     "7078",
		"spark.blockManager.port":                                               "7079",
		"spark.executor.instances":                                              "2",
		"spark.executor.memory":                                                 "1g",
		"spark.executor.cores":                                                  "1",
		"spark.kubernetes.executor.request.cores":                               "100m",
		"spark.kubernetes.executor.label.dataproc.googleapis.com/cluster-uuid":  n.spec.UUID,
		"spark.kubernetes.node.selector.cloud.google.com/gke-nodepool":          n.spec.PoolFor("SPARK_EXECUTOR"),
		"spark.kubernetes.executor.node.selector.cloud.google.com/gke-nodepool": n.spec.PoolFor("SPARK_EXECUTOR"),
	}
	for k, v := range n.spec.Properties {
		conf[k] = v
	}
	for k, v := range job.Properties {
		conf[k] = v
	}
	for k, v := range conf {
		if v != "" {
			args = append(args, "--conf", k+"="+v)
		}
	}
	args = append(args, "--conf", "spark.driver.host=$(POD_IP)")
	join := func(fs []sparkproc.File) string {
		var p []string
		for _, f := range fs {
			p = append(p, virtualPath(f))
		}
		return strings.Join(p, ",")
	}
	jars := job.Jars
	main := ""
	if job.Main != nil {
		main = virtualPath(*job.Main)
	}
	if job.Kind == sparkproc.KindSpark && main == "" && len(jars) > 0 {
		// A main class in jar_file_uris: the first jar is the
		// application's resource.
		main, jars = virtualPath(jars[0]), jars[1:]
	}
	for flag, fs := range map[string][]sparkproc.File{"--jars": jars, "--py-files": job.PyFiles, "--files": job.Files, "--archives": job.Archives} {
		if len(fs) > 0 {
			args = append(args, flag, join(fs))
		}
	}
	if job.MainClass != "" {
		args = append(args, "--class", job.MainClass)
	}
	if job.Kind == sparkproc.KindSparkSQL {
		return append(args, "-f", "/work/cb-query.sql")
	}
	return append(append(args, main), job.Args...)
}

// Submit runs job's driver in a pod and waits for it to end.
func (n *virtualNodes) Submit(ctx context.Context, job sparkproc.Job) (sparkproc.Result, error) {
	switch job.Kind {
	case sparkproc.KindPySpark, sparkproc.KindSpark, sparkproc.KindSparkR, sparkproc.KindSparkSQL:
	default:
		return sparkproc.Result{}, fmt.Errorf("a Dataproc on GKE cluster does not run %s jobs", job.Kind)
	}
	image := spark35Runtime.image
	if job.Kind == sparkproc.KindSparkR {
		image = spark35Runtime.rImage
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	pod := "dataproc-driver-" + hex.EncodeToString(b)
	stage := filepath.Join(n.dir, pod)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return sparkproc.Result{}, err
	}
	defer os.RemoveAll(stage)
	all := append([]sparkproc.File{}, job.PyFiles...)
	all = append(all, job.Jars...)
	all = append(all, job.Files...)
	all = append(all, job.Archives...)
	if job.Main != nil {
		all = append(all, *job.Main)
	}
	for _, f := range all {
		if strings.HasPrefix(f.URI, "file://") {
			continue
		}
		if err := os.WriteFile(filepath.Join(stage, filepath.Base(f.Base())), f.Data, 0o644); err != nil {
			return sparkproc.Result{}, err
		}
	}
	if job.Kind == sparkproc.KindSparkSQL && job.Main != nil {
		var q strings.Builder
		for k, v := range job.QueryVariables {
			fmt.Fprintf(&q, "SET %s=\"%s\";\n", k, v)
		}
		q.Write(job.Main.Data)
		if err := os.WriteFile(filepath.Join(stage, "cb-query.sql"), []byte(q.String()), 0o644); err != nil {
			return sparkproc.Result{}, err
		}
	}
	script := "set -e\nwhile [ ! -e /work/.cb-go ]; do sleep 0.5; done\ncd /work\n" + usrLibSpark + "exec \"$@\"\n"
	cmd := append([]string{"/bin/bash", "-c", script, "driver"}, n.sparkSubmit(job, pod, image)...)
	manifest := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": pod, "namespace": n.spec.Namespace,
			"labels": map[string]string{"dataproc.googleapis.com/cluster-uuid": n.spec.UUID, "spark-role": "driver"}},
		"spec": map[string]any{"serviceAccountName": "spark", "restartPolicy": "Never",
			"nodeSelector": map[string]string{"cloud.google.com/gke-nodepool": n.spec.PoolFor("SPARK_DRIVER")},
			"volumes":      []any{map[string]any{"name": "work", "emptyDir": map[string]any{}}},
			"containers": []any{map[string]any{"name": "driver", "image": image, "imagePullPolicy": "IfNotPresent",
				"command": cmd, "workingDir": "/work",
				"env": []any{map[string]any{"name": "POD_IP", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "status.podIP"}}},
					map[string]any{"name": "HOME", "value": "/tmp"}, map[string]any{"name": "PYSPARK_PYTHON", "value": "python3"}},
				"resources":    map[string]any{"requests": map[string]string{"cpu": "100m", "memory": "512Mi"}},
				"volumeMounts": []any{map[string]any{"name": "work", "mountPath": "/work"}}}}}}
	body, _ := json.Marshal(manifest)
	ns := n.spec.Namespace
	if out, err := n.kubectl(ctx, body, "apply", "-f", "-"); err != nil {
		return sparkproc.Result{}, fmt.Errorf("create the driver pod: %v: %s", err, out)
	}
	defer func() {
		_, _ = n.kubectl(context.WithoutCancel(ctx), nil, "-n", ns, "delete", "pod", pod, "--ignore-not-found", "--wait=false")
	}()
	// The pod's image is pulled on its first job: up to 20 minutes.
	if out, err := n.kubectl(ctx, nil, "-n", ns, "wait", "--for=condition=Ready", "pod/"+pod, "--timeout=20m"); err != nil {
		desc, _ := n.kubectl(ctx, nil, "-n", ns, "describe", "pod", pod)
		return sparkproc.Result{}, fmt.Errorf("the driver pod did not start: %v: %s\n%s", err, out, desc)
	}
	if out, err := n.kubectl(ctx, nil, "-n", ns, "cp", stage+"/.", pod+":/work", "-c", "driver"); err != nil {
		return sparkproc.Result{}, fmt.Errorf("copy the job's files to the driver: %v: %s", err, out)
	}
	if out, err := n.kubectl(ctx, nil, "-n", ns, "exec", pod, "-c", "driver", "--", "touch", "/work/.cb-go"); err != nil {
		return sparkproc.Result{}, fmt.Errorf("start the driver: %v: %s", err, out)
	}
	logs, _ := n.kubectl(ctx, nil, "-n", ns, "logs", "-f", pod, "-c", "driver")
	code := ""
	for i := 0; i < 60 && code == ""; i++ {
		code, _ = n.kubectl(ctx, nil, "-n", ns, "get", "pod", pod, "-o", "jsonpath={.status.containerStatuses[0].state.terminated.exitCode}")
		code = strings.TrimSpace(code)
		if code == "" {
			time.Sleep(time.Second)
		}
	}
	if len(logs) > sparkproc.MaxDriverOutput {
		logs = logs[len(logs)-sparkproc.MaxDriverOutput:]
	}
	res := sparkproc.Result{DriverOutput: logs}
	if code != "0" {
		res.Failed = true
		res.Message = podTail(logs, 5)
		if code == "" {
			res.Message = "the driver pod did not end: " + res.Message
		}
	}
	return res, nil
}

// podTail is s's last n non-empty lines.
func podTail(s string, n int) string {
	var out []string
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			out = append([]string{l}, out...)
		}
	}
	return strings.Join(out, "\n")
}
