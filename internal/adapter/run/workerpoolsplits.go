package run

import (
	"context"
	"sort"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// Instance splits across a worker pool's revisions (#1387).
//
// Cloud Run lets a worker pool run several revisions at once: instanceSplits
// gives each revision, or the latest one, a percentage of
// scaling.manualInstanceCount, and the percentages add up to 100. Here the
// pool's own Deployment runs the latest revision's share, as before, and
// each other revision with a share runs as a Deployment of its own,
// run-worker-pool-<id>-<revision>, rendered from the template its
// ReplicaSet recorded. Its pods carry the pool's workload label and the
// revision's, so the pool's instances and revisions read back as one, and a
// split label, so its pod template never hashes like the retired
// ReplicaSet of the same revision under the pool's Deployment.
//
// A share's instance count is its percentage of the instance count, rounded
// by largest remainder so the counts add up to the instance count; ties go
// to the split listed first. Google does not document its rounding
// (UNVERIFIED).

const (
	// labelSplitOf marks a split revision's Deployment and its pods; its
	// value is the pool's, as labelWorkload's is.
	labelSplitOf = "cloudburrow.dev/run-split-of"
	splitLatest  = runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST
	splitByRev   = runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_REVISION
)

// splitType is a split's allocation type; one with no type that names a
// revision is to that revision.
func splitType(sp *runpb.InstanceSplit) runpb.InstanceSplitAllocationType {
	if sp.GetType() == runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_UNSPECIFIED && sp.GetRevision() != "" {
		return splitByRev
	}
	return sp.GetType()
}

// splitShare is one revision's part of a pool.
type splitShare struct {
	revID   string
	percent int32
	count   int32
}

// splitRevisionID is a split's revision as an ID; a full revision name is
// accepted too.
func splitRevisionID(rev string) string {
	if i := strings.LastIndex(rev, "/revisions/"); i >= 0 {
		return rev[i+len("/revisions/"):]
	}
	return rev
}

// checkInstanceSplits validates the splits as given.
func checkInstanceSplits(splits []*runpb.InstanceSplit) error {
	if len(splits) == 0 {
		return nil
	}
	var sum int32
	latest := false
	seen := map[string]bool{}
	for i, sp := range splits {
		if sp.GetPercent() < 0 || sp.GetPercent() > 100 {
			return apierror.InvalidArgument("instanceSplits[%d].percent must be between 0 and 100, got %d", i, sp.GetPercent())
		}
		switch splitType(sp) {
		case splitLatest:
			if sp.GetRevision() != "" {
				return apierror.InvalidArgument("instanceSplits[%d]: a split to the latest revision names no revision", i)
			}
			if latest {
				return apierror.InvalidArgument("instanceSplits[%d]: the latest revision is split to twice", i)
			}
			latest = true
		case splitByRev:
			id := splitRevisionID(sp.GetRevision())
			if id == "" {
				return apierror.InvalidArgument("instanceSplits[%d].revision is required when type is INSTANCE_SPLIT_ALLOCATION_TYPE_REVISION", i)
			}
			if seen[id] {
				return apierror.InvalidArgument("instanceSplits[%d]: revision %s is split to twice", i, id)
			}
			seen[id] = true
		default:
			return apierror.InvalidArgument("instanceSplits[%d].type is required", i)
		}
		sum += sp.GetPercent()
	}
	if sum != 100 {
		return apierror.InvalidArgument("instanceSplits must add up to 100%%, not %d%%", sum)
	}
	return nil
}

// splitShares is each revision's share of total instances, the latest
// first, then in the order the splits name them. No splits is 100% to the
// latest. A split naming the latest revision by ID adds to its share.
func splitShares(splits []*runpb.InstanceSplit, latestID string, total int32) []splitShare {
	if len(splits) == 0 {
		return []splitShare{{revID: latestID, percent: 100, count: total}}
	}
	shares := []splitShare{{revID: latestID}}
	index := map[string]int{latestID: 0}
	for _, sp := range splits {
		id := latestID
		if splitType(sp) == splitByRev {
			id = splitRevisionID(sp.GetRevision())
		}
		i, ok := index[id]
		if !ok {
			i = len(shares)
			index[id] = i
			shares = append(shares, splitShare{revID: id})
		}
		shares[i].percent += sp.GetPercent()
	}
	// Largest remainder: floor each, then give the rest one by one to the
	// largest remainders, the earlier share first on a tie.
	var given int32
	rem := make([]int64, len(shares))
	for i := range shares {
		exact := int64(total) * int64(shares[i].percent)
		shares[i].count = int32(exact / 100)
		rem[i] = exact % 100
		given += shares[i].count
	}
	order := make([]int, len(shares))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return rem[order[a]] > rem[order[b]] })
	for k := 0; given < total && k < len(order); k++ {
		shares[order[k]].count++
		given++
	}
	return shares
}

// latestShare is the latest revision's instance count.
func latestShare(spec *runpb.WorkerPool) int32 {
	latestID := revisionID(spec.GetLatestCreatedRevision())
	return splitShares(spec.GetInstanceSplits(), latestID, spec.GetScaling().GetManualInstanceCount())[0].count
}

// splitWorkload is the Deployment a revision's share runs as.
func splitDeploymentName(w workload, revID string) string { return w.objectName() + "-" + revID }

// renderSplits renders a Deployment for each revision other than the
// latest with a share, from the template its ReplicaSet recorded. A split
// naming a revision the pool does not have is INVALID_ARGUMENT, as is one
// naming a revision on a pool being created.
func (s *Server) renderSplits(ctx context.Context, w workload, spec *runpb.WorkerPool, create bool) ([]splitManifest, error) {
	latestID := revisionID(spec.GetLatestCreatedRevision())
	shares := splitShares(spec.GetInstanceSplits(), latestID, spec.GetScaling().GetManualInstanceCount())
	if len(shares) == 1 {
		return nil, nil
	}
	if create {
		return nil, apierror.InvalidArgument("instanceSplits names revision %s, but a new worker pool has only its first revision", shares[1].revID)
	}
	items, err := s.workerPoolRevisions(ctx, w)
	if err != nil {
		return nil, err
	}
	var out []splitManifest
	for _, sh := range shares[1:] {
		rs, ok := revisionReplicaSet(items, w.name+"/revisions/"+sh.revID)
		if !ok {
			return nil, apierror.InvalidArgument("instanceSplits: revision %s of worker pool %s not found", sh.revID, w.name)
		}
		if sh.count == 0 {
			continue
		}
		t, gen, err := replicaSetTemplate(rs)
		if err != nil {
			return nil, err
		}
		dep, err := s.deployment(w, podSpec{containers: t.GetContainers(), volumes: t.GetVolumes(), revision: sh.revID,
			revisionName: w.name + "/revisions/" + sh.revID, revisionSpec: t, generation: gen,
			serviceAccount: t.GetServiceAccount(), deploymentName: splitDeploymentName(w, sh.revID)}, sh.count)
		if err != nil {
			return nil, err
		}
		out = append(out, splitManifest{name: splitDeploymentName(w, sh.revID), manifest: dep, serviceAccount: t.GetServiceAccount()})
	}
	return out, nil
}

// splitManifest is a split revision's rendered Deployment.
type splitManifest struct{ name, manifest, serviceAccount string }

// revisionReplicaSet finds a revision's ReplicaSet, preferring the pool's
// own (the one its Deployment created) over a split's.
func revisionReplicaSet(items []kreplicaSet, name string) (kreplicaSet, bool) {
	var found *kreplicaSet
	for i := range items {
		if items[i].Spec.Template.Metadata.Annotations[annRunName] != name {
			continue
		}
		if found == nil || items[i].Spec.Template.Metadata.Labels[labelSplitOf] == "" {
			found = &items[i]
		}
	}
	if found == nil {
		return kreplicaSet{}, false
	}
	return *found, true
}

// replicaSetTemplate decodes the revision template a ReplicaSet recorded.
func replicaSetTemplate(rs kreplicaSet) (*runpb.WorkerPoolRevisionTemplate, int64, error) {
	rev, err := fromReplicaSet(rs, "", nil)
	if err != nil {
		return nil, 0, err
	}
	var t runpb.WorkerPoolRevisionTemplate
	ann := rs.Spec.Template.Metadata.Annotations
	if err := decodeSpec(kconfigMap{Metadata: rs.Metadata, Data: map[string]string{specDataKey: ann[annRevisionSpec]}}, &t); err != nil {
		return nil, 0, err
	}
	return proto.Clone(&t).(*runpb.WorkerPoolRevisionTemplate), rev.GetGeneration(), nil
}

// writeSplits applies the split Deployments and removes the pool's split
// Deployments no longer wanted.
func (s *Server) writeSplits(ctx context.Context, w workload, splits []splitManifest) error {
	keep := map[string]bool{}
	for _, sp := range splits {
		if err := s.ensureWorkloadMetadata(ctx, projectOf(w.name), sp.serviceAccount); err != nil {
			return err
		}
		if err := s.kn.Kube.Apply(ctx, sp.manifest, k8s.ApplyOptions{}); err != nil {
			return apierror.Internal(err, "apply the Deployment of %s's split %s: %s", w.name, sp.name, strings.TrimSpace(err.Error()))
		}
		keep[sp.name] = true
	}
	existing, err := s.splitDeployments(ctx, w)
	if err != nil {
		return err
	}
	for _, d := range existing {
		if !keep[d.Metadata.Name] {
			if err := s.kn.deleteObject(ctx, resDeployment, d.Metadata.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitDeployments are the pool's split revisions' Deployments.
func (s *Server) splitDeployments(ctx context.Context, w workload) ([]kdeployment, error) {
	if w.kind != kindWorkerPool {
		return nil, nil
	}
	return listObjects[kdeployment](ctx, s.kn, resDeployment, labelSplitOf+"="+w.kind+"-"+w.id)
}

// servingRevisions is the set of a pool's revisions with instances: the
// latest, and each split revision with a share.
func servingRevisions(wp *runpb.WorkerPool) map[string]bool {
	latestID := revisionID(wp.GetLatestCreatedRevision())
	out := map[string]bool{wp.GetLatestCreatedRevision(): true}
	for _, sh := range splitShares(wp.GetInstanceSplits(), latestID, wp.GetScaling().GetManualInstanceCount()) {
		if sh.percent > 0 {
			out[wp.GetName()+"/revisions/"+sh.revID] = true
		}
	}
	return out
}

// splitStatuses reports the splits as the pool runs them.
func splitStatuses(splits []*runpb.InstanceSplit) []*runpb.InstanceSplitStatus {
	if len(splits) == 0 {
		return []*runpb.InstanceSplitStatus{{Type: splitLatest, Percent: 100}}
	}
	out := make([]*runpb.InstanceSplitStatus, 0, len(splits))
	for _, sp := range splits {
		st := &runpb.InstanceSplitStatus{Type: splitType(sp), Percent: sp.GetPercent()}
		if splitType(sp) == splitByRev {
			st.Revision = splitRevisionID(sp.GetRevision())
		}
		out = append(out, st)
	}
	return out
}

// revisionID is the last segment of a revision name.
func revisionID(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// RevisionInstances is how many instances each revision of a worker pool
// runs, by revision name, as its instanceSplits share the instance count
// out (#1387): the console's revision listing.
func RevisionInstances(wp *runpb.WorkerPool) map[string]int32 {
	out := map[string]int32{}
	latestID := revisionID(wp.GetLatestCreatedRevision())
	for _, sh := range splitShares(wp.GetInstanceSplits(), latestID, wp.GetScaling().GetManualInstanceCount()) {
		out[wp.GetName()+"/revisions/"+sh.revID] = sh.count
	}
	return out
}
