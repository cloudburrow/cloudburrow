package logging

// ConfigServiceV2.CopyLogEntries (#1394), `gcloud logging copy`, to
// docs.cloud.google.com/logging/docs/routing/copy-logs: the entries of a log
// bucket that filter matches, stored when the call is made, are copied to a
// storage.googleapis.com/BUCKET destination on the instance's Cloud Storage,
// through a long-running operation.
//
//   - The source is projects/P/locations/L/buckets/B. Every entry is stored
//     once, as _Default's (buckets.go), so _Default copies the project's
//     entries and any other bucket copies none. The bucket must exist and be
//     active; a missing one is NOT_FOUND.
//   - The objects are named as a Cloud Storage sink names them
//     (docs.cloud.google.com/logging/docs/export/storage): LOG_ID/YYYY/MM/DD/
//     then the hour's window in UTC, HH:00:00_HH:59:59, and a shard suffix,
//     _S0.json for entries received in the hour of their timestamp, and
//     _A0:UNIX.json, UNIX the copy's start in seconds, for those received in
//     another. Each holds its entries as LogEntry JSON, one per line, in
//     timestamp order (Google: "neither uniform nor otherwise guaranteed").
//   - The Operation is projects/P/locations/L/operations/ID. Its metadata is
//     a CopyLogEntriesMetadata: the request, start and end time, state
//     (SCHEDULED, RUNNING, then SUCCEEDED, FAILED or CANCELLED), progress
//     0-100, cancellation_requested and writer_identity, the project's
//     Logging service account (links.go). Its response is a
//     CopyLogEntriesResponse with log_entries_copied_count.
//   - Google's copy takes at least an hour; this one starts copyDelay after
//     the call, SCHEDULED until then, so `gcloud logging operations cancel`
//     has a window, then writes one object at a time.
//   - google.longrunning.Operations is served for these operations:
//     GetOperation, ListOperations (a filter of request_type=CopyLogEntries,
//     as gcloud sends, or none) and CancelOperation. A cancel sets
//     cancellation_requested; a copy not yet finished stops before its next
//     object and ends CANCELLED with the Operation's error CANCELLED, keeping
//     what was written, as Google's docs say. Operations are kept in memory,
//     and not across a restart.
//   - A failed write ends the operation FAILED with PERMISSION_DENIED
//     "Failed to write export data", the error Google's troubleshooting
//     names (code 7), with the reason; a missing destination bucket is one.
//   - Without Cloud Storage on the instance the call is FAILED_PRECONDITION.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// copyDelay is how long a copy stays SCHEDULED before it starts.
const copyDelay = 2 * time.Second

// ObjectWriter writes one object to the instance's Cloud Storage.
type ObjectWriter interface {
	WriteObject(ctx context.Context, bucket, name, contentType string, data []byte) error
}

// copyOp is one CopyLogEntries operation.
type copyOp struct {
	name string
	md   *loggingpb.CopyLogEntriesMetadata
	done bool
	err  *spb.Status
	resp *loggingpb.CopyLogEntriesResponse
	stop chan struct{} // closed by a cancel
}

// copies holds the Admin's operations and what a copy needs.
type copies struct {
	mu      sync.Mutex
	ops     map[string]*copyOp
	order   []string
	storage ObjectWriter
	entries func() []*loggingpb.LogEntry
	delay   time.Duration
	// pace is waited between objects, for tests that cancel mid-copy.
	pace time.Duration
}

// UseStorage sets where CopyLogEntries writes: the instance's Cloud Storage.
func (a *Admin) UseStorage(w ObjectWriter) {
	a.copies.mu.Lock()
	a.copies.storage = w
	a.copies.mu.Unlock()
}

func (a *Admin) useEntries(fn func() []*loggingpb.LogEntry) {
	a.copies.mu.Lock()
	a.copies.entries = fn
	a.copies.mu.Unlock()
}

// operation is op as a google.longrunning.Operation. The caller holds
// copies.mu.
func (op *copyOp) operation() (*longrunningpb.Operation, error) {
	md, err := anypb.New(proto.Clone(op.md))
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "operation metadata"))
	}
	out := &longrunningpb.Operation{Name: op.name, Metadata: md, Done: op.done}
	switch {
	case op.err != nil:
		out.Result = &longrunningpb.Operation_Error{Error: op.err}
	case op.done:
		resp, err := anypb.New(op.resp)
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "operation response"))
		}
		out.Result = &longrunningpb.Operation_Response{Response: resp}
	}
	return out, nil
}

// copyFile is one object a copy writes.
type copyFile struct {
	name    string
	entries []*loggingpb.LogEntry
}

// copyFiles lays entries out as a Cloud Storage sink names its objects.
func copyFiles(entries []*loggingpb.LogEntry, start time.Time) ([]copyFile, error) {
	byName := map[string]*copyFile{}
	for _, e := range entries {
		_, logID, err := resource.ParseLogName(e.GetLogName())
		if err != nil {
			continue
		}
		if u, err := url.PathUnescape(logID); err == nil {
			logID = u
		}
		ts := e.GetTimestamp().AsTime().UTC()
		hour := ts.Truncate(time.Hour)
		shard := "S0"
		if rt := e.GetReceiveTimestamp(); rt != nil && !rt.AsTime().UTC().Truncate(time.Hour).Equal(hour) {
			shard = fmt.Sprintf("A0:%d", start.Unix())
		}
		name := fmt.Sprintf("%s/%s/%s_%s_%s.json", logID, hour.Format("2006/01/02"),
			hour.Format("15:04:05"), hour.Add(time.Hour-time.Second).Format("15:04:05"), shard)
		f := byName[name]
		if f == nil {
			f = &copyFile{name: name}
			byName[name] = f
		}
		f.entries = append(f.entries, e)
	}
	out := make([]copyFile, 0, len(byName))
	for _, f := range byName {
		sort.SliceStable(f.entries, func(i, j int) bool { return cursorOf(f.entries[i]).compare(cursorOf(f.entries[j])) < 0 })
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func (f copyFile) body() ([]byte, error) {
	var b bytes.Buffer
	for _, e := range f.entries {
		j, err := protojson.Marshal(e)
		if err != nil {
			return nil, err
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.Bytes(), nil
}

// CopyLogEntries starts a copy and answers its operation, not yet done.
func (s configServer) CopyLogEntries(_ context.Context, req *loggingpb.CopyLogEntriesRequest) (*longrunningpb.Operation, error) {
	a := s.a
	ref, err := parseBucket("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	dest := req.GetDestination()
	bucket, ok := strings.CutPrefix(dest, "storage.googleapis.com/")
	if !ok || bucket == "" || strings.Contains(bucket, "/") {
		return nil, apierror.Wrap(apierror.InvalidArgument("destination %q is not storage.googleapis.com/[BUCKET]: "+
			"a copy's destination is a Cloud Storage bucket", dest))
	}
	match, err := compile(req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	a.mu.Lock()
	err = a.activeBucket(ref)
	a.mu.Unlock()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	c := &a.copies
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storage == nil {
		return nil, apierror.Wrap(apierror.FailedPrecondition("CopyLogEntries writes to this instance's Cloud Storage, which is not served: start it with storage"))
	}
	// Only entries stored when the copy starts are copied.
	var picked []*loggingpb.LogEntry
	if ref.bucket == bucketDefault && c.entries != nil {
		for _, e := range c.entries() {
			p, _, perr := resource.ParseLogName(e.GetLogName())
			if perr == nil && p == ref.project && match(e) {
				picked = append(picked, e)
			}
		}
	}
	now := a.now()
	files, err := copyFiles(picked, now)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "laying out the copy"))
	}
	if c.ops == nil {
		c.ops = map[string]*copyOp{}
	}
	a.mu.Lock()
	a.ops++
	n := a.ops
	a.mu.Unlock()
	op := &copyOp{
		name: fmt.Sprintf("projects/%s/locations/%s/operations/copy-%d-%d", ref.project, ref.location, now.UnixNano(), n),
		md: &loggingpb.CopyLogEntriesMetadata{StartTime: timestamppb.New(now), State: loggingpb.OperationState_OPERATION_STATE_SCHEDULED,
			Request:        proto.Clone(req).(*loggingpb.CopyLogEntriesRequest),
			WriterIdentity: "serviceAccount:" + serviceAccount(ref.project)},
		stop: make(chan struct{}),
	}
	c.ops[op.name] = op
	c.order = append(c.order, op.name)
	delay := c.delay
	if delay == 0 {
		delay = copyDelay
	}
	go a.runCopy(op, bucket, files, delay, c.pace)
	return op.operation()
}

// runCopy writes files, one at a time, until done, cancelled or failed.
func (a *Admin) runCopy(op *copyOp, bucket string, files []copyFile, delay, pace time.Duration) {
	c := &a.copies
	finish := func(st loggingpb.OperationState, copied int64, fail *spb.Status) {
		c.mu.Lock()
		defer c.mu.Unlock()
		op.md.State, op.md.EndTime, op.done = st, timestamppb.New(a.now()), true
		op.err = fail
		if fail == nil {
			op.md.Progress = 100
		}
		op.resp = &loggingpb.CopyLogEntriesResponse{LogEntriesCopiedCount: copied}
	}
	cancelled := func(copied int64) {
		finish(loggingpb.OperationState_OPERATION_STATE_CANCELLED, copied,
			&spb.Status{Code: int32(codes.Canceled), Message: "Operation was cancelled."})
	}
	select {
	case <-op.stop:
		cancelled(0)
		return
	case <-time.After(delay):
	}
	c.mu.Lock()
	op.md.State = loggingpb.OperationState_OPERATION_STATE_RUNNING
	storage := c.storage
	c.mu.Unlock()
	total := 0
	for _, f := range files {
		total += len(f.entries)
	}
	var copied int64
	for i, f := range files {
		if i > 0 && pace > 0 {
			select {
			case <-op.stop:
			case <-time.After(pace):
			}
		}
		select {
		case <-op.stop:
			cancelled(copied)
			return
		default:
		}
		body, err := f.body()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err = storage.WriteObject(ctx, bucket, f.name, "application/json", body)
			cancel()
		}
		if err != nil {
			finish(loggingpb.OperationState_OPERATION_STATE_FAILED, copied,
				&spb.Status{Code: int32(codes.PermissionDenied), Message: "Failed to write export data: " + err.Error()})
			return
		}
		copied += int64(len(f.entries))
		c.mu.Lock()
		op.md.Progress = int32(copied * 100 / int64(max(total, 1)))
		c.mu.Unlock()
	}
	finish(loggingpb.OperationState_OPERATION_STATE_SUCCEEDED, copied, nil)
}

// operationsServer is google.longrunning.Operations for copy operations.
type operationsServer struct {
	longrunningpb.UnimplementedOperationsServer
	a *Admin
}

func (s operationsServer) find(name string) (*copyOp, error) {
	op := s.a.copies.ops[name]
	if op == nil {
		return nil, apierror.Wrap(apierror.NotFound("operation %q not found", name))
	}
	return op, nil
}

func (s operationsServer) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	c := &s.a.copies
	c.mu.Lock()
	defer c.mu.Unlock()
	op, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	return op.operation()
}

func (s operationsServer) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	c := &s.a.copies
	c.mu.Lock()
	defer c.mu.Unlock()
	op, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	if op.done {
		return nil, apierror.Wrap(apierror.FailedPrecondition("operation %q has already finished", req.GetName()))
	}
	if !op.md.CancellationRequested {
		op.md.CancellationRequested = true
		close(op.stop)
	}
	return &emptypb.Empty{}, nil
}

// ListOperations lists a location's copy operations, oldest first. The
// filter may be empty or request_type=CopyLogEntries, as `gcloud logging
// operations list` sends; any other request_type matches none.
func (s operationsServer) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	parent := req.GetName()
	if _, err := splitName("name", parent); err != nil {
		return nil, apierror.Wrap(err)
	}
	f := strings.TrimSpace(req.GetFilter())
	if f != "" {
		k, v, ok := strings.Cut(strings.ReplaceAll(f, " ", ""), "=")
		if !ok || (k != "request_type" && k != "requestType") {
			return nil, apierror.Wrap(apierror.InvalidArgument("filter %q: only request_type=CopyLogEntries is supported", f))
		}
		if strings.Trim(v, `"`) != "CopyLogEntries" {
			return &longrunningpb.ListOperationsResponse{}, nil
		}
	}
	c := &s.a.copies
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for _, n := range c.order {
		if strings.HasPrefix(n, parent+"/operations/") {
			names = append(names, n)
		}
	}
	scope := "logging-operations|" + parent + "|" + f
	after, err := paging.DecodeToken(scope, req.GetPageToken())
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("page_token: %v", err))
	}
	start := 0
	if after != "" {
		start = len(names)
		for i, n := range names {
			if n == after {
				start = i + 1
				break
			}
		}
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > 100 {
		size = 100
	}
	end := min(start+size, len(names))
	resp := &longrunningpb.ListOperationsResponse{}
	for _, n := range names[start:end] {
		o, err := c.ops[n].operation()
		if err != nil {
			return nil, err
		}
		resp.Operations = append(resp.Operations, o)
	}
	if end < len(names) {
		resp.NextPageToken = paging.EncodeToken(scope, names[end-1])
	}
	return resp, nil
}

// registerOperations adds google.longrunning.Operations.
func (a *Admin) registerOperations(g grpc.ServiceRegistrar) {
	longrunningpb.RegisterOperationsServer(g, operationsServer{a: a})
}

// OperationsServer is the google.longrunning.Operations server for copy
// operations, for the console.
func (a *Admin) OperationsServer() longrunningpb.OperationsServer { return operationsServer{a: a} }

// CanCopy reports whether CopyLogEntries has a Cloud Storage to write to.
func (a *Admin) CanCopy() bool {
	a.copies.mu.Lock()
	defer a.copies.mu.Unlock()
	return a.copies.storage != nil
}
