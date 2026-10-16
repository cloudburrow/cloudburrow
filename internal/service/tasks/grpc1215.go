package tasks

// RunTask, BatchCreateTasks, BatchDeleteTasks, GetCmekConfig and
// UpdateCmekConfig (#1215), as google.cloud.tasks.v2 documents them.
//
//   - RunTask forces a task to be dispatched now, even if its queue is
//     paused or at its rate limits, and returns the task. On failure the
//     retry policy reschedules it as usual. A task already in flight
//     finishes its attempt first, where Google may start a second one.
//   - BatchCreateTasks (at most 100 requests, all for the parent queue) and
//     BatchDeleteTasks (at most 1000 names) are non-atomic: each request
//     succeeds or fails alone, the failures are reported by index in the
//     operation's metadata, and when every request fails the operation's
//     error is ABORTED. Both finish before they return, so the operation is
//     done. A request_id repeated for the same queue returns the first
//     operation instead of running the batch again.
//   - The CMEK config is stored per location and returned as set. Nothing
//     is encrypted with the key: CloudBurrow's state is plaintext on the
//     host, and the key is not looked up in Cloud KMS.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	taskspb "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/rpc/code"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Batch limits, as BatchCreateTasksRequest and BatchDeleteTasksRequest
// document them.
const (
	MaxBatchCreate = 100
	MaxBatchDelete = 1000
)

// RunTask forces a task to run now.
func (g *GRPCServer) RunTask(_ context.Context, req *taskspb.RunTaskRequest) (*taskspb.Task, error) {
	if _, err := resource.Parse(req.GetName()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	t, err := g.store.RunTask(req.GetName())
	if err != nil {
		return nil, err
	}
	return g.protoTask(t), nil
}

// BatchCreateTasks creates up to 100 tasks in one queue.
func (g *GRPCServer) BatchCreateTasks(ctx context.Context, req *taskspb.BatchCreateTasksRequest) (*longrunningpb.Operation, error) {
	parent := req.GetParent()
	if _, err := resource.Parse(parent); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	n := len(req.GetRequests())
	if n == 0 || n > MaxBatchCreate {
		return nil, apierror.InvalidArgument("requests must hold between 1 and %d CreateTaskRequests, not %d", MaxBatchCreate, n)
	}
	for i, r := range req.GetRequests() {
		if r.GetParent() != parent {
			return nil, apierror.InvalidArgument("requests[%d].parent %q is not the batch's parent %q: every task must be for the same queue",
				i, r.GetParent(), parent)
		}
	}
	if _, err := g.store.GetQueue(parent); err != nil {
		return nil, err
	}
	if op, ok := g.store.batchOperation(parent, req.GetRequestId()); ok {
		return op, nil
	}
	start := time.Now()
	resp := &taskspb.BatchCreateTasksResponse{}
	failed := map[int32]*statuspb.Status{}
	for i, r := range req.GetRequests() {
		t, err := g.CreateTask(ctx, r)
		if err != nil {
			failed[int32(i)] = status.Convert(err).Proto()
			continue
		}
		resp.Tasks = append(resp.Tasks, t)
	}
	meta := &taskspb.BatchCreateTasksMetadata{
		StartTime: timestamppb.New(start), EndTime: timestamppb.Now(),
		State: taskspb.BatchCreateTasksMetadata_SUCCEEDED, FailedRequests: failed,
	}
	if len(failed) > 0 {
		meta.State = taskspb.BatchCreateTasksMetadata_PARTIALLY_SUCCEEDED
	}
	op, err := batchOperation(parent, meta, resp, len(failed) == n,
		"None of the requests succeeded, refer to BatchCreateTasksMetadata.failed_requests for individual error details")
	if err != nil {
		return nil, err
	}
	g.store.keepBatchOperation(parent, req.GetRequestId(), op)
	return op, nil
}

// BatchDeleteTasks deletes up to 1000 tasks of one queue.
func (g *GRPCServer) BatchDeleteTasks(_ context.Context, req *taskspb.BatchDeleteTasksRequest) (*longrunningpb.Operation, error) {
	parent := req.GetParent()
	if _, err := resource.Parse(parent); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	n := len(req.GetNames())
	if n == 0 || n > MaxBatchDelete {
		return nil, apierror.InvalidArgument("names must hold between 1 and %d task names, not %d", MaxBatchDelete, n)
	}
	if _, err := g.store.GetQueue(parent); err != nil {
		return nil, err
	}
	if op, ok := g.store.batchOperation(parent, req.GetRequestId()); ok {
		return op, nil
	}
	start := time.Now()
	failed := map[int32]*statuspb.Status{}
	for i, name := range req.GetNames() {
		var err error
		if !strings.HasPrefix(name, parent+"/tasks/") {
			err = apierror.InvalidArgument("%q is not a task of %s", name, parent)
		} else {
			err = g.store.DeleteTask(name)
		}
		if err != nil {
			failed[int32(i)] = status.Convert(err).Proto()
		}
	}
	meta := &taskspb.BatchDeleteTasksMetadata{
		StartTime: timestamppb.New(start), EndTime: timestamppb.Now(),
		State: taskspb.BatchDeleteTasksMetadata_SUCCEEDED, FailedRequests: failed,
	}
	if len(failed) > 0 {
		meta.State = taskspb.BatchDeleteTasksMetadata_PARTIALLY_SUCCEEDED
	}
	op, err := batchOperation(parent, meta, &emptypb.Empty{}, len(failed) == n,
		"None of the requests succeeded, refer to BatchDeleteTasksMetadata.failed_requests for individual error details")
	if err != nil {
		return nil, err
	}
	g.store.keepBatchOperation(parent, req.GetRequestId(), op)
	return op, nil
}

// batchOperation is a finished batch: its metadata, and its response or,
// when every request failed, ABORTED with abortMsg.
func batchOperation(queue string, meta, resp proto.Message, allFailed bool, abortMsg string) (*longrunningpb.Operation, error) {
	n, _ := resource.Parse(queue)
	md, err := anypb.New(meta)
	if err != nil {
		return nil, apierror.Internal(err, "encode operation metadata")
	}
	op := &longrunningpb.Operation{
		Name:     fmt.Sprintf("%s/operations/%d", n.LocationName(), time.Now().UnixNano()),
		Metadata: md, Done: true,
	}
	if allFailed {
		op.Result = &longrunningpb.Operation_Error{Error: &statuspb.Status{Code: int32(code.Code_ABORTED), Message: abortMsg}}
		return op, nil
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Internal(err, "encode operation response")
	}
	op.Result = &longrunningpb.Operation_Response{Response: r}
	return op, nil
}

func batchKey(queue, requestID string) string { return "tasks/batches/" + queue + "/" + requestID }

// batchOperation returns the operation a request_id already produced.
func (s *Store) batchOperation(queue, requestID string) (*longrunningpb.Operation, bool) {
	if requestID == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := s.db.Get(batchKey(queue, requestID))
	if err != nil {
		return nil, false
	}
	op := &longrunningpb.Operation{}
	if proto.Unmarshal(b, op) != nil {
		return nil, false
	}
	return op, true
}

// keepBatchOperation records the operation a request_id produced.
func (s *Store) keepBatchOperation(queue, requestID string, op *longrunningpb.Operation) {
	if requestID == "" {
		return
	}
	b, err := proto.Marshal(op)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.db.Put(batchKey(queue, requestID), b)
}

// cmekNameRE is a CMEK config's name; kmsKeyRE the key it names.
var (
	cmekNameRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/cmekConfig$`)
	kmsKeyRE   = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)
)

func cmekKey(name string) string { return "tasks/cmek/" + name }

// GetCmekConfig returns a location's CMEK config; by default no key.
func (g *GRPCServer) GetCmekConfig(_ context.Context, req *taskspb.GetCmekConfigRequest) (*taskspb.CmekConfig, error) {
	name := req.GetName()
	if !cmekNameRE.MatchString(name) {
		return nil, apierror.InvalidArgument("%q is not projects/{project}/locations/{location}/cmekConfig", name)
	}
	cfg := &taskspb.CmekConfig{Name: name}
	g.store.mu.RLock()
	defer g.store.mu.RUnlock()
	var key string
	if g.store.get(cmekKey(name), &key) == nil {
		cfg.KmsKey = key
	}
	return cfg, nil
}

// UpdateCmekConfig sets or clears a location's CMEK key. It is stored and
// returned; nothing is encrypted with it.
func (g *GRPCServer) UpdateCmekConfig(_ context.Context, req *taskspb.UpdateCmekConfigRequest) (*taskspb.CmekConfig, error) {
	cfg := req.GetCmekConfig()
	name := cfg.GetName()
	if !cmekNameRE.MatchString(name) {
		return nil, apierror.InvalidArgument("cmek_config.name %q is not projects/{project}/locations/{location}/cmekConfig", name)
	}
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "kms_key" && p != "kmsKey" {
			return nil, apierror.InvalidArgument("update_mask path %q: only kms_key can be updated", p)
		}
	}
	key := cfg.GetKmsKey()
	if key != "" {
		if !kmsKeyRE.MatchString(key) {
			return nil, apierror.InvalidArgument("kms_key %q is not projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{key}", key)
		}
		if loc := strings.Split(key, "/")[3]; loc != strings.Split(name, "/")[3] {
			return nil, apierror.InvalidArgument("kms_key %q is in location %s, not the config's %s", key, loc, strings.Split(name, "/")[3])
		}
	}
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	var err error
	if key == "" {
		err = g.store.db.Delete(cmekKey(name))
		if err != nil && g.store.get(cmekKey(name), new(string)) != nil {
			err = nil // nothing was set
		}
	} else {
		err = g.store.put(cmekKey(name), key)
	}
	if err != nil {
		return nil, apierror.From(err)
	}
	return &taskspb.CmekConfig{Name: name, KmsKey: key}, nil
}
