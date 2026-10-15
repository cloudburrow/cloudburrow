package run

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Deployment-backed Cloud Run resources: worker pools (#1300) and
// standalone instances (#1299).
//
// Both are containers that run without a request routed to them, at a fixed
// count, which is what an apps/v1 Deployment is: no Knative Service, no
// route and no autoscaler. The resource itself is configuration, kept as
// protojson in a ConfigMap the adapter owns, as a job is (jobs.go), so its
// etag is the ConfigMap's resourceVersion and does not move when the
// Deployment's status does. The pods are rendered with the same container
// mapping as a revision and a job's task: image localisation, env,
// secretKeyRef, resources, probes and volumes.

const (
	resDeployment = "deployments.apps"
	resReplicaSet = "replicasets.apps"
	// labelWorkload names the resource a Deployment's pods belong to; its
	// value is "<kind>-<id>", so a worker pool and an instance of the same ID
	// do not select each other's pods.
	labelWorkload = "cloudburrow.dev/run-workload"
	// labelRevision names a worker pool revision on its pods, and so on the
	// ReplicaSet the Deployment creates for it.
	labelRevision = "cloudburrow.dev/run-revision"
	// annRevisionSpec is the revision's template as protojson, on the pod
	// template, so a ReplicaSet reads back as the revision it runs.
	annRevisionSpec = "cloudburrow.dev/run-revision-template"
	annRevisionGen  = "cloudburrow.dev/run-revision-generation"
	specDataKey     = "spec.json"
)

// kdeployment is the subset of an apps/v1 Deployment read here.
type kdeployment struct {
	Metadata kmeta `json:"metadata"`
	Spec     struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration  int64           `json:"observedGeneration"`
		Replicas            int32           `json:"replicas"`
		ReadyReplicas       int32           `json:"readyReplicas"`
		AvailableReplicas   int32           `json:"availableReplicas"`
		UpdatedReplicas     int32           `json:"updatedReplicas"`
		UnavailableReplicas int32           `json:"unavailableReplicas"`
		Conditions          []ksvcCondition `json:"conditions"`
	} `json:"status"`
}

// kreplicaSet is the subset of an apps/v1 ReplicaSet a worker pool
// revision is read from.
type kreplicaSet struct {
	Metadata kmeta `json:"metadata"`
	Spec     struct {
		Replicas *int32 `json:"replicas"`
		Template struct {
			Metadata kmeta `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas      int32 `json:"replicas"`
		ReadyReplicas int32 `json:"readyReplicas"`
	} `json:"status"`
}

// noExecEnv adapts a template that has no execution environment field to
// podTemplate, so the gaps a revision refuses are refused in the same words.
type noExecEnv struct {
	podFields
}

type podFields interface {
	GetContainers() []*runpb.Container
	GetVolumes() []*runpb.Volume
	GetVpcAccess() *runpb.VpcAccess
	GetServiceAccount() string
	GetEncryptionKey() string
	GetNodeSelector() *runpb.NodeSelector
}

func (noExecEnv) GetExecutionEnvironment() runpb.ExecutionEnvironment {
	return runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_UNSPECIFIED
}

// workload names the objects behind one deployment-backed resource.
type workload struct {
	// kind is "worker-pool" or "instance".
	kind string
	id   string
	// name is the Cloud Run resource name.
	name string
}

func (w workload) objectName() string { return "run-" + w.kind + "-" + w.id }
func (w workload) selector() string   { return labelWorkload + "=" + w.kind + "-" + w.id }

// configMap renders the ConfigMap holding a resource's spec.
func (w workload) configMap(spec proto.Message, namespace, instance, resourceVersion string, extra map[string]string) (string, error) {
	raw, err := protojson.Marshal(spec)
	if err != nil {
		return "", apierror.Internal(err, "encode %s %s", w.kind, w.name)
	}
	meta := map[string]any{
		"name":      w.objectName(),
		"namespace": namespace,
		"labels": map[string]string{
			labelOwned: "true", labelInstance: instance, labelKind: "run-" + w.kind,
		},
		"annotations": map[string]string{annRunName: w.name},
	}
	if resourceVersion != "" {
		meta["resourceVersion"] = resourceVersion
	}
	data := map[string]string{specDataKey: string(raw)}
	for k, v := range extra {
		data[k] = v
	}
	out, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": data})
	return string(out), err
}

// podSpec is what a Deployment's pods run.
type podSpec struct {
	containers []*runpb.Container
	volumes    []*runpb.Volume
	// revision, when set, labels the pods with a worker pool revision and
	// records its template, so its ReplicaSet reads back as the revision.
	revision, revisionName string
	revisionSpec           proto.Message
	generation             int64
}

// deployment renders the Deployment running replicas pods of p.
func (s *Server) deployment(w workload, p podSpec, replicas int32) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
  labels:
    %s: "true"
    %s: %q
    %s: %s
  annotations:
    %s: %q
spec:
  replicas: %d
  revisionHistoryLimit: 1000
  selector:
    matchLabels:
      %s: %s
  template:
    metadata:
      labels:
        %s: %s
`, w.objectName(), s.kn.namespace(), labelOwned, labelInstance, s.instance, labelWorkload, w.kind+"-"+w.id,
		annRunName, w.name, replicas, labelWorkload, w.kind+"-"+w.id, labelWorkload, w.kind+"-"+w.id)
	if p.revision != "" {
		raw, err := protojson.Marshal(p.revisionSpec)
		if err != nil {
			return "", apierror.Internal(err, "encode revision %s", p.revisionName)
		}
		fmt.Fprintf(&b, "        %s: %s\n      annotations:\n        %s: %q\n        %s: %q\n        %s: %q\n",
			labelRevision, p.revision, annRunName, p.revisionName, annRevisionSpec, string(raw),
			annRevisionGen, fmt.Sprint(p.generation))
	}
	b.WriteString("    spec:\n")
	o := containerRender{project: projectOf(w.name), secrets: s.secrets, batch: true,
		injected: s.injectedFor(projectOf(w.name)), volumes: p.volumes}
	if len(cloudSQLVolumes(p.volumes)) > 0 {
		// A service's and a job's are served (#1346); no proxy is rendered
		// beside a worker pool's or an instance's containers yet.
		return "", apierror.Unimplemented("unsupported Cloud Run configuration: volumes.cloudSqlInstance: "+
			"Cloud SQL volumes are served on services and jobs, not on %ss", w.kind)
	}
	if err := renderVolumes(&b, p.volumes, o); err != nil {
		return "", err
	}
	b.WriteString("      containers:\n")
	if err := renderContainers(&b, p.containers, o); err != nil {
		return "", err
	}
	return b.String(), nil
}

// loadSpec reads the ConfigMap behind w and checks it is the one named.
func (s *Server) loadSpec(ctx context.Context, w workload) (kconfigMap, error) {
	var cm kconfigMap
	if err := s.kn.getObject(ctx, &cm, resConfigMap, w.objectName(), w.kind+" "+w.name); err != nil {
		return kconfigMap{}, err
	}
	if cm.Metadata.Labels[labelOwned] != "true" || cm.Metadata.Labels[labelKind] != "run-"+w.kind ||
		cm.Metadata.Annotations[annRunName] != w.name {
		return kconfigMap{}, apierror.NotFound("%s %s not found", w.kind, w.name)
	}
	return cm, nil
}

// decodeSpec decodes the spec a ConfigMap holds into into.
func decodeSpec(cm kconfigMap, into proto.Message) error {
	if err := protojson.Unmarshal([]byte(cm.Data[specDataKey]), into); err != nil {
		return apierror.Internal(err, "decode %s", cm.Metadata.Name)
	}
	return nil
}

// specsOfKind lists the ConfigMaps of one kind.
func (s *Server) specsOfKind(ctx context.Context, kind string) ([]kconfigMap, error) {
	return listObjects[kconfigMap](ctx, s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"=run-"+kind)
}

// loadDeployment reads w's Deployment; a missing one is the zero value.
func (s *Server) loadDeployment(ctx context.Context, w workload) (kdeployment, bool, error) {
	var d kdeployment
	if err := s.kn.getObject(ctx, &d, resDeployment, w.objectName(), w.kind+" "+w.name); err != nil {
		if apierror.From(err).Code == codes.NotFound {
			return kdeployment{}, false, nil
		}
		return kdeployment{}, false, err
	}
	return d, true, nil
}

// ensureAbsent refuses a create over an existing resource of the same ID.
func (s *Server) ensureAbsent(ctx context.Context, w workload) error {
	var existing kconfigMap
	err := s.kn.getObject(ctx, &existing, resConfigMap, w.objectName(), w.kind+" "+w.name)
	if err == nil {
		return apierror.AlreadyExists("%s %s already exists", w.kind, w.name)
	}
	if apierror.From(err).Code != codes.NotFound {
		return err
	}
	return nil
}

// writeWorkload writes the spec ConfigMap and the Deployment. verb is
// "create" or "replace"; the Deployment is applied either way, so one left
// by an interrupted delete is taken over rather than refused.
func (s *Server) writeWorkload(ctx context.Context, w workload, cmManifest, deployManifest, verb string) error {
	if err := s.kn.writeObject(ctx, verb, cmManifest, w.kind+" "+w.name); err != nil {
		return err
	}
	if err := s.kn.Kube.Apply(ctx, deployManifest, k8s.ApplyOptions{}); err != nil {
		detail := strings.TrimSpace(err.Error())
		if isRejection(detail) {
			return apierror.InvalidArgument("the cluster rejected %s: %s", w.name, detail)
		}
		return apierror.Internal(err, "apply the Deployment of %s: %s", w.name, detail)
	}
	return nil
}

// deleteWorkload removes the Deployment, its ReplicaSets and pods, an
// instance's route, and the spec ConfigMap.
func (s *Server) deleteWorkload(ctx context.Context, w workload) error {
	if err := s.kn.deleteObject(ctx, resDeployment, w.objectName()); err != nil {
		return err
	}
	if w.kind == kindInstance {
		if err := s.deleteInstanceRoute(ctx, w); err != nil {
			return err
		}
	}
	return s.kn.deleteObject(ctx, resConfigMap, w.objectName())
}

// deploymentConditions maps a Deployment's status onto Cloud Run's terminal
// condition: ready when every wanted pod is available, failed when the
// rollout made no progress, and reconciling otherwise.
func deploymentConditions(d kdeployment, found bool, updated time.Time) (*runpb.Condition, bool) {
	cond := &runpb.Condition{Type: "Ready", LastTransitionTime: timestamppb.New(updated)}
	if !found {
		cond.State = runpb.Condition_CONDITION_PENDING
		cond.Message = "the instances are not created yet"
		return cond, true
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	for _, c := range d.Status.Conditions {
		if c.Type == "Progressing" && c.Status == "False" {
			cond.State = runpb.Condition_CONDITION_FAILED
			cond.Message = c.Message
			return cond, false
		}
	}
	if d.Status.ObservedGeneration >= d.Metadata.Generation && d.Status.UpdatedReplicas >= want &&
		d.Status.AvailableReplicas >= want && d.Status.Replicas == want {
		cond.State = runpb.Condition_CONDITION_SUCCEEDED
		return cond, false
	}
	cond.State = runpb.Condition_CONDITION_RECONCILING
	cond.Message = fmt.Sprintf("%d of %d instances are available", d.Status.AvailableReplicas, want)
	return cond, true
}

// workloadOperation starts the operation of a create, update, start or stop
// and completes it only once the Deployment's pods are ready (#1348), as a
// service's completes once its revision is (awaitReady): before, the
// operation was done when written, so a caller's op.Wait returned while the
// instances were still pulling their image. read renders the resource and
// its terminal condition.
func (s *Server) workloadOperation(ctx context.Context, parent string, w workload,
	read func(context.Context, kconfigMap) (proto.Message, *runpb.Condition, error)) (*longrunningpb.Operation, error) {
	op := s.ops.Create(parent, w.name)
	go s.awaitWorkload(context.WithoutCancel(ctx), op.Name, w, read)
	return s.toProtoOperation(op.Name)
}

// awaitWorkload polls a workload until its terminal condition succeeds or
// fails, or the bound elapses. A failed rollout (the Deployment's
// Progressing=False) or a pod that cannot start fails the operation.
func (s *Server) awaitWorkload(ctx context.Context, opName string, w workload,
	read func(context.Context, kconfigMap) (proto.Message, *runpb.Condition, error)) {
	deadline := time.Now().Add(s.readyTimeout)
	for time.Now().Before(deadline) {
		cm, err := s.loadSpec(ctx, w)
		if err != nil && apierror.From(err).Code == codes.NotFound {
			_ = s.ops.Fail(opName, apierror.NotFound("%s %s was deleted before it became ready", w.kind, w.name))
			return
		}
		if err == nil {
			out, cond, rerr := read(ctx, cm)
			switch {
			case rerr != nil:
			case cond.GetState() == runpb.Condition_CONDITION_SUCCEEDED:
				_ = s.ops.Succeed(opName, out)
				return
			case cond.GetState() == runpb.Condition_CONDITION_FAILED:
				_ = s.ops.Fail(opName, apierror.FailedPrecondition("%s %s failed: %s", w.kind, w.name, cond.GetMessage()))
				return
			}
			if msg := s.podFailure(ctx, w); msg != "" {
				_ = s.ops.Fail(opName, apierror.FailedPrecondition("%s %s failed: %s", w.kind, w.name, msg))
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(workloadPoll):
		}
	}
	_ = s.ops.Fail(opName, apierror.FailedPrecondition("%s %s did not become ready within %s", w.kind, w.name, s.readyTimeout))
}

// workloadPoll is how often awaitWorkload reads a workload again.
var workloadPoll = time.Second

// kwaitingPod is the subset of a pod a start failure is read from.
type kwaitingPod struct {
	Status struct {
		ContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// podStartFailures are the waiting reasons a pod does not recover from by
// itself: the image cannot be pulled, or the container's configuration is
// invalid.
var podStartFailures = map[string]bool{"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"ErrImageNeverPull": true, "CreateContainerConfigError": true, "CreateContainerError": true}

// podFailure names why one of w's pods cannot start, or "".
func (s *Server) podFailure(ctx context.Context, w workload) string {
	pods, err := listObjects[kwaitingPod](ctx, s.kn, "pods", w.selector())
	if err != nil {
		return ""
	}
	for _, p := range pods {
		for _, c := range p.Status.ContainerStatuses {
			if wt := c.State.Waiting; wt != nil && podStartFailures[wt.Reason] {
				return fmt.Sprintf("container %s: %s: %s", c.Name, wt.Reason, wt.Message)
			}
		}
	}
	return ""
}
