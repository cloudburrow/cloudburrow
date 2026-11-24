package dataflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Snapshotter is an Engine that saves a running streaming job's state
// (#2215): CloudBurrow's takes an Apache Flink savepoint of the job's
// Flink job, which a job created from the snapshot (createdFromSnapshotId)
// is restored from.
type Snapshotter interface {
	// Snapshot saves the state of the running job jobID as snapshot id,
	// and answers its size on disk.
	Snapshot(ctx context.Context, jobID, id string) (sizeBytes int64, err error)
	// DeleteSnapshot removes snapshot id's saved state.
	DeleteSnapshot(id string) error
}

// Snapshots, as Google documents them (guides/using-snapshots, the v1b3
// REST reference for projects.locations.snapshots and
// projects.locations.jobs.snapshot, read 2026-11-24):
//
//   - only a running streaming job is snapshotted;
//   - a snapshot is PENDING, RUNNING while it is taken, then READY (or
//     FAILED), and is deleted when its time to live (ttl, default 7 days,
//     at most 30) has passed;
//   - a job is created from a READY snapshot of its project and region
//     with createdFromSnapshotId (the SDKs' --createFromSnapshot), and a
//     snapshot of a job's region only.
const (
	defaultSnapshotTTL = 7 * 24 * time.Hour
	maxSnapshotTTL     = 30 * 24 * time.Hour
)

type snapRec struct {
	snap *dataflowpb.Snapshot
}

func sortSnapshots(v []*dataflowpb.Snapshot) {
	sort.Slice(v, func(i, j int) bool {
		a, b := v[i].GetCreationTime().AsTime(), v[j].GetCreationTime().AsTime()
		if !a.Equal(b) {
			return a.After(b)
		}
		return v[i].Id < v[j].Id
	})
}

func snapKey(project, loc, id string) string { return project + "/" + loc + "/" + id }

func newSnapshotID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// SnapshotJob snapshots a running streaming job.
func (s *Server) SnapshotJob(ctx context.Context, req *dataflowpb.SnapshotJobRequest) (*dataflowpb.Snapshot, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	ttl := defaultSnapshotTTL
	if d := req.GetTtl(); d != nil {
		ttl = d.AsDuration()
		if ttl <= 0 || ttl > maxSnapshotTTL {
			// UNVERIFIED (docs re-read 2026-11-24): the reference bounds ttl at 30 days.
			return nil, apierror.InvalidArgument("Snapshot TTL must be positive and at most 30 days, but was %s.", ttl)
		}
	}
	sn, ok := s.opts.Engine.(Snapshotter)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.jobs[jobKey(req.GetProjectId(), loc, req.GetJobId())]
	if r == nil {
		return nil, apierror.NotFound("(%s): Could not find job with id %s in region %s.", req.GetProjectId(), req.GetJobId(), loc) // UNVERIFIED (docs re-read 2026-11-24)
	}
	j := r.job
	if j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING || j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING {
		// guides/using-snapshots: snapshots are of running streaming jobs.
		return nil, apierror.FailedPrecondition("(%s): Snapshots are only supported for running streaming jobs; job %s is a %s job in state %s.", req.GetProjectId(), j.Id, j.Type, j.CurrentState) // UNVERIFIED (docs re-read 2026-11-24)
	}
	if !ok || r.cancel == nil {
		return nil, apierror.FailedPrecondition("(%s): The job %s's runner takes no snapshot.", req.GetProjectId(), j.Id) // UNVERIFIED (docs re-read 2026-11-24)
	}
	now := s.now()
	snap := &dataflowpb.Snapshot{
		Id: newSnapshotID(), ProjectId: j.ProjectId, SourceJobId: j.Id, CreationTime: timestamppb.New(now),
		Ttl: durationpb.New(ttl), State: dataflowpb.SnapshotState_PENDING, Description: req.GetDescription(), Region: loc,
	}
	if req.GetSnapshotSources() {
		// The job's Pub/Sub sources' subscriptions are snapshotted with it
		// on Google; CloudBurrow's reads keep their place in the job's
		// state, so the snapshot names none (UNVERIFIED).
		snap.PubsubMetadata = []*dataflowpb.PubsubSnapshotMetadata{}
	}
	key := snapKey(j.ProjectId, loc, snap.Id)
	s.snaps[key] = &snapRec{snap: snap}
	s.save()
	out := proto.Clone(snap).(*dataflowpb.Snapshot)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		if rec := s.snaps[key]; rec != nil {
			rec.snap.State = dataflowpb.SnapshotState_RUNNING
		}
		s.mu.Unlock()
		c, cancel := context.WithTimeout(s.base, 10*time.Minute)
		size, err := sn.Snapshot(c, j.Id, snap.Id)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		rec := s.snaps[key]
		if rec == nil {
			if err == nil {
				_ = sn.DeleteSnapshot(snap.Id)
			}
			return
		}
		if err != nil {
			rec.snap.State = dataflowpb.SnapshotState_FAILED
			s.logf("dataflow: snapshot %s of job %s: %v", snap.Id, j.Id, err)
		} else {
			rec.snap.State, rec.snap.DiskSizeBytes = dataflowpb.SnapshotState_READY, size
		}
		s.save()
	}()
	return out, nil
}

// expireLocked deletes the snapshots whose time to live has passed.
func (s *Server) expireLocked() {
	now := s.now()
	for k, r := range s.snaps {
		if r.snap.GetCreationTime().AsTime().Add(r.snap.GetTtl().AsDuration()).Before(now) {
			s.dropSnapLocked(k, r)
		}
	}
}

func (s *Server) dropSnapLocked(key string, r *snapRec) {
	delete(s.snaps, key)
	if sn, ok := s.opts.Engine.(Snapshotter); ok && r.snap.State == dataflowpb.SnapshotState_READY {
		if err := sn.DeleteSnapshot(r.snap.Id); err != nil {
			s.logf("dataflow: delete snapshot %s: %v", r.snap.Id, err)
		}
	}
}

func (s *Server) lookupSnapLocked(project, loc, id string) (*snapRec, error) {
	s.expireLocked()
	r := s.snaps[snapKey(project, loc, id)]
	if r == nil {
		return nil, apierror.NotFound("Snapshot %s not found in project %s, region %s.", id, project, loc) // UNVERIFIED (docs re-read 2026-11-24)
	}
	return r, nil
}

// GetSnapshot answers a snapshot.
func (s *Server) GetSnapshot(_ context.Context, req *dataflowpb.GetSnapshotRequest) (*dataflowpb.Snapshot, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupSnapLocked(req.GetProjectId(), loc, req.GetSnapshotId())
	if err != nil {
		return nil, err
	}
	return proto.Clone(r.snap).(*dataflowpb.Snapshot), nil
}

// ListSnapshots lists a region's snapshots, or one job's.
func (s *Server) ListSnapshots(_ context.Context, req *dataflowpb.ListSnapshotsRequest) (*dataflowpb.ListSnapshotsResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	out := &dataflowpb.ListSnapshotsResponse{}
	for _, r := range s.snaps {
		sn := r.snap
		if sn.ProjectId != req.GetProjectId() || sn.Region != loc || (req.GetJobId() != "" && sn.SourceJobId != req.GetJobId()) {
			continue
		}
		out.Snapshots = append(out.Snapshots, proto.Clone(sn).(*dataflowpb.Snapshot))
	}
	sortSnapshots(out.Snapshots)
	return out, nil
}

// DeleteSnapshot deletes a snapshot and its saved state.
func (s *Server) DeleteSnapshot(_ context.Context, req *dataflowpb.DeleteSnapshotRequest) (*dataflowpb.DeleteSnapshotResponse, error) {
	if err := checkProject(req.GetProjectId()); err != nil {
		return nil, err
	}
	loc, err := location(req.GetLocation())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.lookupSnapLocked(req.GetProjectId(), loc, req.GetSnapshotId())
	if err != nil {
		return nil, err
	}
	s.dropSnapLocked(snapKey(req.GetProjectId(), loc, req.GetSnapshotId()), r)
	s.save()
	return &dataflowpb.DeleteSnapshotResponse{}, nil
}

// restoreSnapshotLocked checks that job, created from a snapshot, may be:
// the snapshot is READY in its project and region and the job streams.
func (s *Server) restoreSnapshotLocked(project, loc string, job *dataflowpb.Job) error {
	id := job.GetCreatedFromSnapshotId()
	if id == "" {
		return nil
	}
	r, err := s.lookupSnapLocked(project, loc, id)
	if err != nil {
		return err
	}
	if r.snap.State != dataflowpb.SnapshotState_READY {
		return apierror.FailedPrecondition("(%s): Snapshot %s is %s, not READY: a job is created from a ready snapshot.", project, id, r.snap.State) // UNVERIFIED (docs re-read 2026-11-24)
	}
	if job.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
		return apierror.FailedPrecondition("(%s): A job created from a snapshot must be a streaming job.", project) // UNVERIFIED (docs re-read 2026-11-24)
	}
	return nil
}

// Snapshots are the project's snapshots ("" every project's), newest
// first: the console's Snapshots page.
func (s *Server) Snapshots(project string) []*dataflowpb.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	var out []*dataflowpb.Snapshot
	for _, r := range s.snaps {
		if project == "" || r.snap.GetProjectId() == project {
			out = append(out, proto.Clone(r.snap).(*dataflowpb.Snapshot))
		}
	}
	sortSnapshots(out)
	return out
}
