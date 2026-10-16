package run

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Reference-counted volume objects (#1476).
//
// A Cloud Storage FUSE volume's gcsfuse pod (gcsfuse.go), and a service's
// NFS or Cloud Storage PersistentVolume and claim (nfsClaims, gcsClaims),
// are shared by every workload that mounts the same bucket or export with
// the same options. Each is removed once no workload uses it: no service,
// none of a live service's revisions, no job, worker pool or instance, no
// Deployment and no running execution.
//
// The references are not counted in memory. They are read from the cluster
// each time (collectVolumes): the Knative Services and revisions, the
// Deployments and batch Jobs, whose pod specs name the claim or the node
// directory, and the specs the adapter stores for jobs, worker pools and
// instances. So a restarted instance counts exactly what an uninterrupted
// one would, and the first collection after a restart also removes what an
// older version left behind. A collection runs after every delete and
// update of a workload, when the adapter starts, and on a reset.
//
// volumeMu orders a collection after any create or update in flight: a
// mount pod is applied before the workload that mounts it, and a
// collection in between would see it unused.

const (
	resPV          = "persistentvolumes"
	resPVC         = "persistentvolumeclaims"
	resKService    = "services.serving.knative.dev"
	resKRevision   = "revisions.serving.knative.dev"
	resDeployments = "deployments.apps"
	labelGCSFuse   = "cloudburrow.dev/gcsfuse"
	labelKService  = "serving.knative.dev/service"
)

// volumeObject reports whether name is one of the shared volume objects
// collectVolumes manages.
func volumeObject(name string) bool {
	return strings.HasPrefix(name, "run-gcsfuse-") || strings.HasPrefix(name, "run-nfs-")
}

// holdVolumes keeps a collection from running while a workload that mounts
// a shared volume is being written; the caller releases it once the
// workload is applied.
func (s *Server) holdVolumes(volumes []*runpb.Volume) func() {
	for _, v := range volumes {
		if v.GetGcs() != nil || v.GetNfs() != nil {
			s.volumeMu.RLock()
			return s.volumeMu.RUnlock
		}
	}
	return func() {}
}

// A reference is to a claim (with its PersistentVolume of the same name)
// or to a gcsfuse pod's mount. A service's claim of a bucket uses the mount
// too; a job, worker pool or instance mounts the node directory, or an
// NFS export inline, and uses no claim.
const (
	refClaim = "claim/"
	refMount = "mount/"
)

// podVolumes are a pod spec's volumes, as far as they name a shared
// object: a claim, or a node directory a gcsfuse pod mounts.
type podVolumes []struct {
	PersistentVolumeClaim *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
	HostPath *struct {
		Path string `json:"path"`
	} `json:"hostPath"`
}

func (p podVolumes) addRefs(into map[string]bool) {
	for _, v := range p {
		if c := v.PersistentVolumeClaim; c != nil && volumeObject(c.ClaimName) {
			into[refClaim+c.ClaimName] = true
			// A bucket's claim is of the node directory its pod mounts.
			into[refMount+c.ClaimName] = true
		}
		if h := v.HostPath; h != nil {
			if name, ok := strings.CutPrefix(h.Path, gcsFuseHostRoot+"/"); ok && volumeObject(name) {
				into[refMount+name] = true
			}
		}
	}
}

// volumeHolder is any object read for the volumes it uses: a Knative
// Service, Deployment or batch Job (spec.template.spec.volumes), a Knative
// revision (spec.volumes) or a spec ConfigMap (data).
type volumeHolder struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Volumes  podVolumes `json:"volumes"`
		Template struct {
			Spec struct {
				Volumes podVolumes `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Active int32 `json:"active"`
	} `json:"status"`
	Data map[string]string `json:"data"`
}

// specVolumeRefs adds the bucket mounts of every Cloud Run volume in a
// stored spec (a job's, worker pool's or instance's protojson), wherever
// the spec nests its template. Their NFS volumes are inline and use no
// claim.
func specVolumeRefs(raw string, into map[string]bool) {
	var doc any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return
	}
	var walk func(any)
	walk = func(n any) {
		switch n := n.(type) {
		case map[string]any:
			if vols, ok := n["volumes"].([]any); ok {
				for _, e := range vols {
					b, err := json.Marshal(e)
					if err != nil {
						continue
					}
					var v runpb.Volume
					if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &v) == nil && v.GetGcs() != nil {
						into[refMount+gcsMountName(v.GetGcs())] = true
					}
				}
			}
			for _, c := range n {
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	walk(doc)
}

// listAll reads every object of a resource in the namespace.
func (s *Server) listAll(ctx context.Context, resource, selector string) ([]volumeHolder, error) {
	args := []string{"get", resource, "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	out, err := s.kn.Kube.Do(ctx, "", args...)
	if err != nil {
		return nil, apierror.Internal(err, "list %s", resource)
	}
	var list struct {
		Items []volumeHolder `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, apierror.Internal(err, "decode %s list", resource)
	}
	return list.Items, nil
}

// volumeRefs is the set of shared volume objects some workload uses. Any
// read that fails fails the whole count: an object is never removed on a
// partial one.
func (s *Server) volumeRefs(ctx context.Context) (map[string]bool, error) {
	refs := map[string]bool{}
	services, err := s.listAll(ctx, resKService, "")
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, k := range services {
		if k.Metadata.DeletionTimestamp != "" {
			continue
		}
		live[k.Metadata.Name] = true
		k.Spec.Template.Spec.Volumes.addRefs(refs)
	}
	// A live service's revisions, which traffic may still route to; a
	// deleted service's linger until Knative collects them.
	revisions, err := s.listAll(ctx, resKRevision, "")
	if err != nil {
		return nil, err
	}
	for _, r := range revisions {
		if live[r.Metadata.Labels[labelKService]] {
			r.Spec.Volumes.addRefs(refs)
		}
	}
	deployments, err := s.listAll(ctx, resDeployments, "")
	if err != nil {
		return nil, err
	}
	for _, d := range deployments {
		if d.Metadata.DeletionTimestamp == "" {
			d.Spec.Template.Spec.Volumes.addRefs(refs)
		}
	}
	// A running execution, whose job may since have dropped the volume.
	jobs, err := s.listAll(ctx, resBatchJob, "")
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.Metadata.DeletionTimestamp == "" && j.Status.Active > 0 {
			j.Spec.Template.Spec.Volumes.addRefs(refs)
		}
	}
	specs, err := s.listAll(ctx, resConfigMap, labelOwned+"=true,"+labelKind)
	if err != nil {
		return nil, err
	}
	for _, cm := range specs {
		switch cm.Metadata.Labels[labelKind] {
		case kindRunJob, "run-" + kindWorkerPool, "run-" + kindInstance:
			for _, raw := range cm.Data {
				specVolumeRefs(raw, refs)
			}
		}
	}
	return refs, nil
}

// CollectVolumes removes every gcsfuse pod, PersistentVolume and claim of
// this instance that no workload uses, and returns how many it removed.
func (s *Server) CollectVolumes(ctx context.Context) (int, error) {
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()
	owned := labelOwned + "=true," + labelInstance + "=" + s.instance
	// The claims first, then their volumes, then the mounts under them. A
	// claim a terminating pod still mounts goes once the pod does: delete
	// does not wait for it.
	kinds := []struct{ resource, selector, ref string }{
		{resPVC, owned, refClaim}, {resPV, owned, refClaim}, {resPod, owned + "," + labelGCSFuse, refMount},
	}
	found := make([][]string, len(kinds))
	pending := false
	for i, k := range kinds {
		items, err := s.listAll(ctx, k.resource, k.selector)
		if err != nil {
			return 0, err
		}
		for _, it := range items {
			if volumeObject(it.Metadata.Name) && it.Metadata.DeletionTimestamp == "" {
				found[i] = append(found[i], it.Metadata.Name)
				pending = true
			}
		}
	}
	if !pending {
		return 0, nil
	}
	refs, err := s.volumeRefs(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for i, k := range kinds {
		for _, name := range found[i] {
			if refs[k.ref+name] {
				continue
			}
			if _, err := s.kn.Kube.Do(ctx, "", "delete", k.resource, name, "--ignore-not-found", "--wait=false"); err != nil {
				return removed, apierror.Internal(err, "delete %s %s", k.resource, name)
			}
			removed++
		}
	}
	return removed, nil
}

// collectVolumes runs CollectVolumes after a workload was deleted or
// changed. A failure leaves the objects for the next collection and does
// not fail the call that triggered it.
func (s *Server) collectVolumes(ctx context.Context) {
	if _, err := s.CollectVolumes(context.WithoutCancel(ctx)); err != nil {
		slog.Warn("Cloud Run could not remove unused volume objects", "err", err)
	}
}
