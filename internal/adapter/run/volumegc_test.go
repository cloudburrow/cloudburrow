package run

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// volumeKube is a kubectl that lists objects by resource, filtered by a
// label selector, and records the deletes. A fresh Server over the same
// objects is what a restarted instance is.
type volumeKube struct {
	mu      sync.Mutex
	objects map[string][]map[string]any
	fail    string
	deleted []string
}

func (f *volumeKube) Run(_ context.Context, _ string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	args = args[4:] // --kubeconfig k -n ns
	switch args[0] {
	case "get":
		if args[1] == f.fail {
			return "", errors.New("the server is currently unable to handle the request")
		}
		sel := ""
		for i, a := range args {
			if a == "-l" {
				sel = args[i+1]
			}
		}
		items := []any{}
		for _, o := range f.objects[args[1]] {
			if sel == "" || matches(stringMap(o["metadata"].(map[string]any)["labels"]), sel) {
				items = append(items, o)
			}
		}
		b, err := json.Marshal(map[string]any{"items": items})
		return string(b), err
	case "delete":
		f.deleted = append(f.deleted, args[1]+"/"+args[2])
		kept := f.objects[args[1]][:0]
		for _, o := range f.objects[args[1]] {
			if o["metadata"].(map[string]any)["name"] != args[2] {
				kept = append(kept, o)
			}
		}
		f.objects[args[1]] = kept
		return "", nil
	}
	return "", errors.New("unexpected kubectl " + strings.Join(args, " "))
}

func (f *volumeKube) add(resource string, obj map[string]any) {
	f.objects[resource] = append(f.objects[resource], obj)
}

func (f *volumeKube) remove(resource, name string) {
	kept := f.objects[resource][:0]
	for _, o := range f.objects[resource] {
		if o["metadata"].(map[string]any)["name"] != name {
			kept = append(kept, o)
		}
	}
	f.objects[resource] = kept
}

func (f *volumeKube) names(resource string) []string {
	var out []string
	for _, o := range f.objects[resource] {
		out = append(out, o["metadata"].(map[string]any)["name"].(string))
	}
	sort.Strings(out)
	return out
}

func meta(name string, labels map[string]string) map[string]any {
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return map[string]any{"name": name, "labels": l}
}

func ownedLabels(extra ...string) map[string]string {
	l := map[string]string{labelOwned: "true", labelInstance: "inst"}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

// mountObjects adds a gcsfuse pod with its PersistentVolume and claim.
func (f *volumeKube) mountObjects(name string) {
	f.add(resPod, map[string]any{"metadata": meta(name, ownedLabels(labelGCSFuse, name))})
	f.add(resPV, map[string]any{"metadata": meta(name, ownedLabels())})
	f.add(resPVC, map[string]any{"metadata": meta(name, ownedLabels())})
}

// ksvcWithClaims is a Knative Service, and its revision, that mount claims.
func (f *volumeKube) ksvcWithClaims(name string, claims ...string) {
	var vols []any
	for _, c := range claims {
		vols = append(vols, map[string]any{"name": c, "persistentVolumeClaim": map[string]any{"claimName": c}})
	}
	f.add(resKService, map[string]any{"metadata": meta(name, nil),
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": vols}}}})
	f.add(resKRevision, map[string]any{"metadata": meta(name+"-00001", map[string]string{labelKService: name}),
		"spec": map[string]any{"volumes": vols}})
}

func (f *volumeKube) storedJob(t *testing.T, id string, vols ...*runpb.Volume) {
	t.Helper()
	raw, err := protojson.Marshal(&runpb.Job{Name: "projects/p/locations/l/jobs/" + id,
		Template: &runpb.ExecutionTemplate{Template: &runpb.TaskTemplate{Volumes: vols}}})
	if err != nil {
		t.Fatal(err)
	}
	f.add(resConfigMap, map[string]any{"metadata": meta("run-job-"+id, ownedLabels(labelKind, kindRunJob)),
		"data": map[string]any{jobDataKey: string(raw)}})
}

func newVolumeKube() *volumeKube { return &volumeKube{objects: map[string][]map[string]any{}} }

func volumeServer(f *volumeKube) *Server {
	return NewServer(&Knative{Kube: k8s.NewWith(f, "k", "", "cloudburrow-run")}, "inst", time.Second)
}

func gcsVol(bucket string) *runpb.Volume {
	return &runpb.Volume{Name: "v", VolumeType: &runpb.Volume_Gcs{Gcs: &runpb.GCSVolumeSource{Bucket: bucket}}}
}

func nfsVol(server, path string) *runpb.Volume {
	return &runpb.Volume{Name: "n", VolumeType: &runpb.Volume_Nfs{Nfs: &runpb.NFSVolumeSource{Server: server, Path: path}}}
}

func collect(t *testing.T, s *Server) int {
	t.Helper()
	n, err := s.CollectVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A bucket's mount pod, volume and claim stay while either of two services
// mounts it, and go with the last one, counted afresh by each Server, as
// after a restart (#1476).
func TestCollectVolumesKeepsAMountUntilItsLastService(t *testing.T) {
	t.Parallel()
	f := newVolumeKube()
	mount := gcsMountName(gcsVol("data").GetGcs())
	f.mountObjects(mount)
	f.ksvcWithClaims("a", mount)
	f.ksvcWithClaims("b", mount)

	if n := collect(t, volumeServer(f)); n != 0 {
		t.Fatalf("removed %d objects two services use: %v", n, f.deleted)
	}
	f.remove(resKService, "a")
	if n := collect(t, volumeServer(f)); n != 0 {
		t.Fatalf("removed %d objects service b uses: %v", n, f.deleted)
	}
	// The deleted service's revision lingers until Knative collects it, and
	// is not a use.
	f.remove(resKService, "b")
	if n := collect(t, volumeServer(f)); n != 3 {
		t.Fatalf("removed %d objects, want the pod, volume and claim: %v", n, f.deleted)
	}
	want := []string{resPVC + "/" + mount, resPV + "/" + mount, resPod + "/" + mount}
	if strings.Join(f.deleted, " ") != strings.Join(want, " ") {
		t.Errorf("deleted %v, want %v in that order", f.deleted, want)
	}
}

// A stored job keeps its bucket's mount pod between executions, and an NFS
// export's claim stays while any service mounts it.
func TestCollectVolumesCountsJobsAndSharedNFS(t *testing.T) {
	t.Parallel()
	f := newVolumeKube()
	jobMount := gcsMountName(gcsVol("jobdata").GetGcs())
	f.add(resPod, map[string]any{"metadata": meta(jobMount, ownedLabels(labelGCSFuse, jobMount))})
	f.storedJob(t, "nightly", gcsVol("jobdata"))
	claim := nfsClaimName(nfsVol("10.0.0.2", "/share").GetNfs())
	f.add(resPV, map[string]any{"metadata": meta(claim, ownedLabels())})
	f.add(resPVC, map[string]any{"metadata": meta(claim, ownedLabels())})
	f.ksvcWithClaims("a", claim)
	f.ksvcWithClaims("b", claim)
	// Another instance's and a user's own objects are never touched.
	f.add(resPV, map[string]any{"metadata": meta("run-nfs-0000", map[string]string{labelOwned: "true", labelInstance: "other"})})
	f.add(resPVC, map[string]any{"metadata": meta("mine", ownedLabels())})

	if n := collect(t, volumeServer(f)); n != 0 {
		t.Fatalf("removed objects in use: %v", f.deleted)
	}
	f.remove(resKService, "a")
	f.remove(resConfigMap, "run-job-nightly")
	if n := collect(t, volumeServer(f)); n != 1 || f.deleted[0] != resPod+"/"+jobMount {
		t.Fatalf("deleted %v, want only the job's mount pod", f.deleted)
	}
	f.remove(resKService, "b")
	collect(t, volumeServer(f))
	if got := f.names(resPV); strings.Join(got, ",") != "run-nfs-0000" {
		t.Errorf("volumes left %v, want only the other instance's", got)
	}
	if got := f.names(resPVC); strings.Join(got, ",") != "mine" {
		t.Errorf("claims left %v, want only the one not named as a volume object", got)
	}
}

// Deployments (worker pools, instances) and running executions use the
// node directory a gcsfuse pod mounts.
func TestCollectVolumesCountsHostPathMounts(t *testing.T) {
	t.Parallel()
	f := newVolumeKube()
	g := gcsVol("pool").GetGcs()
	f.mountObjects(gcsMountName(g))
	hostVol := []any{map[string]any{"name": "v", "hostPath": map[string]any{"path": gcsHostPath(g)}}}
	f.add(resDeployments, map[string]any{"metadata": meta("worker-pool-p", nil),
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": hostVol}}}})
	if n := collect(t, volumeServer(f)); n != 2 {
		// The claim and volume a service would mount are unused; the pod is
		// the Deployment's.
		t.Fatalf("deleted %v, want the unused claim and volume", f.deleted)
	}
	f.remove(resDeployments, "worker-pool-p")
	f.add(resBatchJob, map[string]any{"metadata": meta("exec-1", nil), "status": map[string]any{"active": 1},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": hostVol}}}})
	if n := collect(t, volumeServer(f)); n != 0 {
		t.Fatalf("deleted the mount of a running execution: %v", f.deleted)
	}
	f.objects[resBatchJob][0]["status"] = map[string]any{"succeeded": 1}
	if n := collect(t, volumeServer(f)); n != 1 {
		t.Fatalf("a finished execution kept its mount: %v", f.deleted)
	}
}

// A count that cannot be read in full removes nothing.
func TestCollectVolumesRemovesNothingOnAFailedRead(t *testing.T) {
	t.Parallel()
	f := newVolumeKube()
	f.mountObjects("run-gcsfuse-abc")
	f.fail = resKRevision
	if _, err := volumeServer(f).CollectVolumes(context.Background()); err == nil {
		t.Fatal("a failed revision list was not an error")
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v on a partial count", f.deleted)
	}
}

// With no volume objects, a collection reads nothing else.
func TestCollectVolumesWithNoObjects(t *testing.T) {
	t.Parallel()
	f := newVolumeKube()
	f.fail = resKService
	if n := collect(t, volumeServer(f)); n != 0 {
		t.Fatal(n)
	}
}
