package pubsubfront

// UpdateSnapshot (#1406). The emulator refuses it, UNIMPLEMENTED "Method
// google.pubsub.v1.Subscriber/UpdateSnapshot is unimplemented", and serves
// CreateSnapshot, GetSnapshot, ListSnapshots and DeleteSnapshot. The front
// applies it as it applies a labels update to a topic (#949, labels.go): the
// labels and expire_time an update names are kept in the front, and every
// snapshot read back through it (GetSnapshot, ListSnapshots and
// UpdateSnapshot, over gRPC and REST) names them in place of the emulator's.
// A later CreateSnapshot or DeleteSnapshot of the name drops them.
//
// Google's rules, from google/pubsub/v1/pubsub.proto: UpdateSnapshot
// "Updates an existing snapshot by updating the fields specified in the
// update mask", and Snapshot.expire_time: "The snapshot is guaranteed to
// exist up until this time. A newly-created snapshot expires no later than 7
// days from the time of its creation. Its exact lifetime is determined at
// creation by the existing backlog in the source subscription." So an
// update may bring a snapshot's expiry forward, never past what it was given
// at creation, and never into the past; the front deletes the snapshot from
// the emulator once its kept expire_time passes (Sweep). The labels are
// checked against Google's rules (labelrules.go), and the mask may name
// only labels and expire_time: name, topic and the rest are fixed at
// creation (INVALID_ARGUMENT).

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// keptSnapshot is what an UpdateSnapshot set on a snapshot.
type keptSnapshot struct {
	// Labels is set when an update named labels; never nil then.
	Labels map[string]string `json:"labels,omitempty"`
	// HasLabels is set when an update named labels, so that none is kept as
	// none.
	HasLabels bool `json:"hasLabels,omitempty"`
	// ExpireTime is the expiry an update set, zero for none.
	ExpireTime time.Time `json:"expireTime,omitzero"`
}

// snapshotAdmin is the part of the emulator's Subscriber service the front
// reads and deletes snapshots with.
func (f *Front) snapshotAdmin() pubsubpb.SubscriberClient {
	return pubsubpb.NewSubscriberClient(f.upstream)
}

// withSnapshot puts what an update set, if anything, in place of what the
// emulator returned for a snapshot; it reports whether it changed anything.
func (f *Front) withSnapshot(s *pubsubpb.Snapshot) bool {
	if s == nil {
		return false
	}
	f.mu.Lock()
	k, ok := f.snapshots[s.GetName()]
	f.mu.Unlock()
	if !ok {
		return false
	}
	changed := false
	if k.HasLabels && !labelsEqual(k.Labels, s.GetLabels()) {
		s.Labels = updatedLabels(k.Labels)
		if len(s.Labels) == 0 {
			s.Labels = nil
		}
		changed = true
	}
	if !k.ExpireTime.IsZero() && !s.GetExpireTime().AsTime().Equal(k.ExpireTime) {
		s.ExpireTime = timestamppb.New(k.ExpireTime)
		changed = true
	}
	return changed
}

// UpdateSnapshot applies an update of a snapshot's labels and expire_time.
func (f *Front) UpdateSnapshot(ctx context.Context, req *pubsubpb.UpdateSnapshotRequest) (*pubsubpb.Snapshot, error) {
	name := req.GetSnapshot().GetName()
	if name == "" {
		return nil, apierror.InvalidArgument("snapshot.name is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask must name at least one field")
	}
	labels, expire := false, false
	for _, p := range paths {
		switch camelToSnake(p) {
		case "labels":
			labels = true
		case "expire_time":
			expire = true
		default:
			return nil, apierror.InvalidArgument("Invalid update_mask provided in the UpdateSnapshotRequest: "+
				"the field %q cannot be updated; only labels and expire_time can", p)
		}
	}
	if labels {
		if err := CheckLabels(req.GetSnapshot().GetLabels()); err != nil {
			return nil, err
		}
	}
	cur, err := f.snapshotAdmin().GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: name})
	if err != nil {
		return nil, err
	}
	f.withSnapshot(cur)
	f.mu.Lock()
	next := f.snapshots[name]
	f.mu.Unlock()
	if labels {
		next.Labels = updatedLabels(req.GetSnapshot().GetLabels())
		next.HasLabels = true
	}
	if expire {
		t := req.GetSnapshot().GetExpireTime()
		if t == nil || t.CheckValid() != nil {
			return nil, apierror.InvalidArgument("snapshot.expire_time must be a valid time when update_mask names it")
		}
		at := t.AsTime()
		if !at.After(f.clock.Now()) {
			return nil, apierror.InvalidArgument("snapshot.expire_time %s is not in the future", at.UTC().Format(time.RFC3339Nano))
		}
		if limit := cur.GetExpireTime(); limit != nil && at.After(limit.AsTime()) {
			return nil, apierror.InvalidArgument("snapshot.expire_time %s is later than %s, the latest the snapshot is guaranteed to exist until",
				at.UTC().Format(time.RFC3339Nano), limit.AsTime().UTC().Format(time.RFC3339Nano))
		}
		next.ExpireTime = at
	}
	f.mu.Lock()
	f.snapshots[name] = next
	f.mu.Unlock()
	f.persist()
	f.withSnapshot(cur)
	return cur, nil
}

// handleUpdateSnapshot serves Subscriber/UpdateSnapshot over gRPC.
func (f *Front) handleUpdateSnapshot(ctx context.Context, ss grpc.ServerStream) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	f.sawProject(in)
	var req pubsubpb.UpdateSnapshotRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return apierror.InvalidArgument("the request is not a google.pubsub.v1.UpdateSnapshotRequest: %v", err)
	}
	resp, err := f.UpdateSnapshot(ctx, &req)
	if err != nil {
		return err
	}
	return sendProto(ss, resp)
}

// dropSnapshot forgets what updates set on a snapshot that was deleted or
// made again.
func (f *Front) dropSnapshot(name string) {
	f.mu.Lock()
	_, ok := f.snapshots[name]
	delete(f.snapshots, name)
	f.mu.Unlock()
	if ok {
		f.persist()
	}
}

// snapshotResponse rewrites a GetSnapshot or ListSnapshots answer.
func (f *Front) snapshotResponse(method string, fr frame) frame {
	switch method {
	case "GetSnapshot":
		var s pubsubpb.Snapshot
		if proto.Unmarshal(fr, &s) != nil || !f.withSnapshot(&s) {
			return fr
		}
		if b, err := proto.Marshal(&s); err == nil {
			return b
		}
	case "ListSnapshots":
		var l pubsubpb.ListSnapshotsResponse
		if proto.Unmarshal(fr, &l) != nil {
			return fr
		}
		changed := false
		for _, s := range l.GetSnapshots() {
			changed = f.withSnapshot(s) || changed
		}
		if !changed {
			return fr
		}
		if b, err := proto.Marshal(&l); err == nil {
			return b
		}
	case "CreateSnapshot":
		var s pubsubpb.Snapshot
		if proto.Unmarshal(fr, &s) == nil {
			f.dropSnapshot(s.GetName())
		}
	}
	return fr
}

// sweepSnapshots deletes every snapshot whose kept expire_time has passed.
func (f *Front) sweepSnapshots(ctx context.Context) {
	now := f.clock.Now()
	var due []string
	f.mu.Lock()
	for name, k := range f.snapshots {
		if !k.ExpireTime.IsZero() && !now.Before(k.ExpireTime) {
			due = append(due, name)
		}
	}
	f.mu.Unlock()
	sort.Strings(due)
	for _, name := range due {
		_, err := f.snapshotAdmin().DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: name})
		if err != nil && status.Code(err) != codes.NotFound {
			f.logf("pubsub front: expire the snapshot %s: %v", name, err)
			continue
		}
		f.forget(name)
		f.dropSnapshot(name)
		f.logf("pubsub front: the snapshot %s expired", name)
	}
}

// serveRESTSnapshot serves a REST call on /v1/projects/{p}/snapshots/{n}
// the front acts on, and reports whether it answered: a PATCH is
// UpdateSnapshot, its body {snapshot, updateMask}; a GET's answer names
// what updates set.
func (f *Front) serveRESTSnapshot(w http.ResponseWriter, r *http.Request, name, verb string) bool {
	if verb != "" {
		return false
	}
	switch r.Method {
	case http.MethodPatch:
		body, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		var req pubsubpb.UpdateSnapshotRequest
		if err := restJSON.Unmarshal(body, &req); err != nil {
			writeRESTError(w, apierror.InvalidArgument("the body is not an UpdateSnapshotRequest: %v", err))
			return true
		}
		if req.Snapshot == nil {
			req.Snapshot = &pubsubpb.Snapshot{}
		}
		req.Snapshot.Name = name
		if m := r.URL.Query().Get("updateMask"); m != "" && len(req.GetUpdateMask().GetPaths()) == 0 {
			req.UpdateMask = &fieldmaskpb.FieldMask{Paths: splitMask(m, nil)}
		}
		resp, err := f.UpdateSnapshot(r.Context(), &req)
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		writeProtoJSON(w, resp)
		return true
	case http.MethodGet:
		f.mu.Lock()
		_, kept := f.snapshots[name]
		f.mu.Unlock()
		if !kept {
			return false
		}
		s, err := f.snapshotAdmin().GetSnapshot(r.Context(), &pubsubpb.GetSnapshotRequest{Snapshot: name})
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		f.withSnapshot(s)
		writeProtoJSON(w, s)
		return true
	}
	return false
}

// restListSnapshots answers GET /v1/projects/{p}/snapshots when a snapshot
// of the project has been updated: the emulator's page is read over gRPC
// and each snapshot names what updates set. It reports whether it answered.
func (f *Front) restListSnapshots(w http.ResponseWriter, r *http.Request) bool {
	rest, found := strings.CutPrefix(r.URL.Path, "/v1/projects/")
	if !found || r.Method != http.MethodGet {
		return false
	}
	p, c, _ := strings.Cut(rest, "/")
	if p == "" || c != "snapshots" {
		return false
	}
	project := "projects/" + p
	f.mu.Lock()
	kept := false
	for name := range f.snapshots {
		kept = kept || strings.HasPrefix(name, project+"/snapshots/")
	}
	f.mu.Unlock()
	if !kept {
		return false
	}
	req := &pubsubpb.ListSnapshotsRequest{Project: project, PageToken: r.URL.Query().Get("pageToken")}
	if n := r.URL.Query().Get("pageSize"); n != "" {
		size, err := strconv.ParseInt(n, 10, 32)
		if err != nil {
			writeRESTError(w, apierror.InvalidArgument("pageSize %q is not a number", n))
			return true
		}
		req.PageSize = int32(size)
	}
	l, err := f.snapshotAdmin().ListSnapshots(r.Context(), req)
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	for _, s := range l.GetSnapshots() {
		f.withSnapshot(s)
	}
	writeProtoJSON(w, l)
	return true
}
