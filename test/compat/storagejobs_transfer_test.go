//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/storage"
	sts "cloud.google.com/go/storagetransfer/apiv1"
	"cloud.google.com/go/storagetransfer/apiv1/storagetransferpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	stsv1 "google.golang.org/api/storagetransfer/v1"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func transferClient(t *testing.T, h *Harness) *sts.Client {
	t.Helper()
	c, err := sts.NewRESTClient(h.Context(), option.WithEndpoint("http://"+stripScheme(h.Endpoint(EnvStorageTransfer))),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func gcsSpec(src, dst string) *storagetransferpb.TransferSpec {
	return &storagetransferpb.TransferSpec{
		DataSource: &storagetransferpb.TransferSpec_GcsDataSource{GcsDataSource: &storagetransferpb.GcsData{BucketName: src}},
		DataSink:   &storagetransferpb.TransferSpec_GcsDataSink{GcsDataSink: &storagetransferpb.GcsData{BucketName: dst}},
	}
}

// createTransferJob creates job and deletes it afterwards.
func createTransferJob(t *testing.T, h *Harness, c *sts.Client, job *storagetransferpb.TransferJob) *storagetransferpb.TransferJob {
	t.Helper()
	job.ProjectId = h.Project()
	if job.Status == storagetransferpb.TransferJob_STATUS_UNSPECIFIED {
		job.Status = storagetransferpb.TransferJob_ENABLED
	}
	got, err := c.CreateTransferJob(h.Context(), &storagetransferpb.CreateTransferJobRequest{TransferJob: job})
	if err != nil {
		t.Fatalf("CreateTransferJob: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteTransferJob(context.Background(), &storagetransferpb.DeleteTransferJobRequest{JobName: got.GetName(), ProjectId: h.Project()})
	})
	return got
}

// transferOperation reads an operation's TransferOperation metadata.
func transferOperation(t *testing.T, h *Harness, c *sts.Client, name string) (*storagetransferpb.TransferOperation, bool) {
	t.Helper()
	op, err := c.GetOperation(h.Context(), &longrunningpb.GetOperationRequest{Name: name})
	if err != nil {
		t.Fatalf("GetOperation %s: %v", name, err)
	}
	var md storagetransferpb.TransferOperation
	if err := op.GetMetadata().UnmarshalTo(&md); err != nil {
		t.Fatalf("operation %s metadata: %v", name, err)
	}
	return &md, op.GetDone()
}

// waitTransfer waits (up to 50 s) until the operation is done, and returns
// its metadata.
func waitTransfer(t *testing.T, h *Harness, c *sts.Client, name string) *storagetransferpb.TransferOperation {
	t.Helper()
	deadline := time.Now().Add(50 * time.Second)
	for {
		md, done := transferOperation(t, h, c, name)
		if done {
			return md
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s is still %v after 50 s", name, md.GetStatus())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitTransferStatus waits (up to 20 s) for the operation to reach status.
func waitTransferStatus(t *testing.T, h *Harness, c *sts.Client, name string, want storagetransferpb.TransferOperation_Status) *storagetransferpb.TransferOperation {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		md, _ := transferOperation(t, h, c, name)
		if md.GetStatus() == want {
			return md
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s is %v after 20 s, want %v", name, md.GetStatus(), want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func runTransfer(t *testing.T, h *Harness, c *sts.Client, job string) string {
	t.Helper()
	op, err := c.RunTransferJob(h.Context(), &storagetransferpb.RunTransferJobRequest{JobName: job, ProjectId: h.Project()})
	if err != nil {
		t.Fatalf("RunTransferJob %s: %v", job, err)
	}
	if !strings.HasPrefix(op.Name(), "transferOperations/") {
		t.Errorf("run's operation %q is not a transferOperations/ name", op.Name())
	}
	return op.Name()
}

func objectBody(t *testing.T, ctx context.Context, bh *storage.BucketHandle, name string) string {
	t.Helper()
	r, err := bh.Object(name).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return "<none>"
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func transferFilter(project string, key string, values ...string) string {
	b, _ := json.Marshal(map[string]any{"projectId": project, key: values})
	return string(b)
}

// covers: storagetransfer.transferJobs.create, storagetransfer.transferJobs.get, storagetransfer.transferJobs.list, storagetransfer.transferJobs.patch, storagetransfer.transferJobs.run, storagetransfer.transferJobs.delete, storagetransfer.transferOperations.get, storagetransfer.transferOperations.list, storagetransfer.googleServiceAccounts.get
//
// TestStorageTransferBetweenBuckets (#1957), with
// cloud.google.com/go/storagetransfer/apiv1 (REST) and
// google.golang.org/api/storagetransfer/v1: a job from one local bucket to
// another copies what its object conditions select (an include and an
// exclude prefix), overwrites what differs and deletes what is only in the
// sink, with counters saying so; patched to never overwrite and to delete
// from the source, its next run skips the objects already there and moves a
// new one. Jobs and operations are listed by the required filter, the job
// names its latest operation, mutually exclusive delete options are
// refused, and the project's service account is the transfer service's.
func TestStorageTransferBetweenBuckets(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := transferClient(t, h)
	ctx := h.Context()
	src, dst := h.Project()+"-sts", h.Project()+"-std"
	sb := companionBucket(t, h, sc, src, "")
	db := companionBucket(t, h, sc, dst, "")
	for name, body := range map[string]string{"logs/a": "A", "logs/b": "B", "logs/skip/c": "C", "other/d": "D"} {
		putCompanionObject(t, ctx, sb, name, body)
	}
	putCompanionObject(t, ctx, db, "logs/b", "OLD")
	putCompanionObject(t, ctx, db, "logs/stale", "S")

	spec := gcsSpec(src, dst)
	spec.ObjectConditions = &storagetransferpb.ObjectConditions{IncludePrefixes: []string{"logs/"}, ExcludePrefixes: []string{"logs/skip/"}}
	spec.TransferOptions = &storagetransferpb.TransferOptions{OverwriteWhen: storagetransferpb.TransferOptions_DIFFERENT, DeleteObjectsUniqueInSink: true}
	job := createTransferJob(t, h, c, &storagetransferpb.TransferJob{Description: "logs between buckets", TransferSpec: spec})
	if !strings.HasPrefix(job.GetName(), "transferJobs/") || job.GetCreationTime() == nil {
		t.Errorf("created %q, creationTime %v", job.GetName(), job.GetCreationTime())
	}
	op1 := runTransfer(t, h, c, job.GetName())
	md := waitTransfer(t, h, c, op1)
	cn := md.GetCounters()
	if md.GetStatus() != storagetransferpb.TransferOperation_SUCCESS || md.GetTransferJobName() != job.GetName() ||
		cn.GetObjectsFoundFromSource() != 2 || cn.GetObjectsCopiedToSink() != 2 || cn.GetObjectsDeletedFromSink() != 1 {
		t.Errorf("first run: %v, job %q, counters %+v", md.GetStatus(), md.GetTransferJobName(), cn)
	}
	for name, want := range map[string]string{"logs/a": "A", "logs/b": "B", "logs/skip/c": "<none>", "other/d": "<none>", "logs/stale": "<none>"} {
		if got := objectBody(t, ctx, db, name); got != want {
			t.Errorf("sink %s = %q, want %q", name, got, want)
		}
	}

	putCompanionObject(t, ctx, sb, "logs/a", "A2")
	putCompanionObject(t, ctx, sb, "logs/new", "N")
	spec.TransferOptions = &storagetransferpb.TransferOptions{OverwriteWhen: storagetransferpb.TransferOptions_NEVER, DeleteObjectsFromSourceAfterTransfer: true}
	patched, err := c.UpdateTransferJob(ctx, &storagetransferpb.UpdateTransferJobRequest{JobName: job.GetName(), ProjectId: h.Project(),
		TransferJob:                &storagetransferpb.TransferJob{TransferSpec: spec, Description: "patched"},
		UpdateTransferJobFieldMask: &fieldmaskpb.FieldMask{Paths: []string{"transfer_spec", "description"}}})
	if err != nil || patched.GetDescription() != "patched" || !patched.GetTransferSpec().GetTransferOptions().GetDeleteObjectsFromSourceAfterTransfer() {
		t.Fatalf("UpdateTransferJob: %+v, %v", patched, err)
	}
	op2 := runTransfer(t, h, c, job.GetName())
	md = waitTransfer(t, h, c, op2)
	cn = md.GetCounters()
	if md.GetStatus() != storagetransferpb.TransferOperation_SUCCESS || cn.GetObjectsCopiedToSink() != 1 ||
		cn.GetObjectsFromSourceSkippedBySync() != 2 || cn.GetObjectsDeletedFromSource() < 1 {
		t.Errorf("second run: %v, counters %+v", md.GetStatus(), cn)
	}
	if got := objectBody(t, ctx, db, "logs/a"); got != "A" {
		t.Errorf("overwriteWhen NEVER replaced logs/a with %q", got)
	}
	if got := objectBody(t, ctx, db, "logs/new"); got != "N" {
		t.Errorf("sink logs/new = %q", got)
	}
	if got := objectBody(t, ctx, sb, "logs/new"); got != "<none>" {
		t.Errorf("logs/new is still in the source after deleteObjectsFromSourceAfterTransfer: %q", got)
	}

	got, err := c.GetTransferJob(ctx, &storagetransferpb.GetTransferJobRequest{JobName: job.GetName(), ProjectId: h.Project()})
	if err != nil || got.GetLatestOperationName() != op2 {
		t.Errorf("GetTransferJob: latest %q (%v), want %s", got.GetLatestOperationName(), err, op2)
	}
	var jobs []string
	jit := c.ListTransferJobs(ctx, &storagetransferpb.ListTransferJobsRequest{Filter: transferFilter(h.Project(), "jobNames", job.GetName())})
	for {
		j, err := jit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListTransferJobs: %v", err)
		}
		jobs = append(jobs, j.GetName())
	}
	if len(jobs) != 1 || jobs[0] != job.GetName() {
		t.Errorf("ListTransferJobs by name: %v", jobs)
	}
	var ops []string
	oit := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: "transferOperations",
		Filter: transferFilter(h.Project(), "jobNames", job.GetName())})
	for {
		o, err := oit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListOperations: %v", err)
		}
		ops = append(ops, o.GetName())
	}
	if len(ops) != 2 {
		t.Errorf("transferOperations.list for the job: %v, want %s and %s", ops, op1, op2)
	}

	// The discovery client: the list filter is required, and the service account.
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	dc, err := stsv1.NewService(ctx, option.WithEndpoint("http://"+stripScheme(h.Endpoint(EnvStorageTransfer))+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dc.TransferJobs.List("").Context(ctx).Do(); storageJobsCode(err) != codes.InvalidArgument {
		t.Errorf("transferJobs.list without a filter: %v, want INVALID_ARGUMENT", err)
	}
	sa, err := dc.GoogleServiceAccounts.Get(h.Project()).Context(ctx).Do()
	if err != nil || !strings.HasSuffix(sa.AccountEmail, "@storage-transfer-service.iam.gserviceaccount.com") || sa.SubjectId == "" {
		t.Errorf("googleServiceAccounts.get: %+v, %v", sa, err)
	}
	if dop, err := dc.TransferOperations.Get(op1).Context(ctx).Do(); err != nil || !dop.Done {
		t.Errorf("discovery transferOperations.get: %+v, %v", dop, err)
	}

	bad := gcsSpec(src, dst)
	bad.TransferOptions = &storagetransferpb.TransferOptions{DeleteObjectsUniqueInSink: true, DeleteObjectsFromSourceAfterTransfer: true}
	if _, err := c.CreateTransferJob(ctx, &storagetransferpb.CreateTransferJobRequest{TransferJob: &storagetransferpb.TransferJob{
		ProjectId: h.Project(), Status: storagetransferpb.TransferJob_ENABLED, TransferSpec: bad}}); storageJobsCode(err) != codes.InvalidArgument {
		t.Errorf("both delete options: %v, want INVALID_ARGUMENT", err)
	}

	if err := c.DeleteTransferJob(ctx, &storagetransferpb.DeleteTransferJobRequest{JobName: job.GetName(), ProjectId: h.Project()}); err != nil {
		t.Fatalf("DeleteTransferJob: %v", err)
	}
	after, err := c.GetTransferJob(ctx, &storagetransferpb.GetTransferJobRequest{JobName: job.GetName(), ProjectId: h.Project()})
	if err == nil && after.GetStatus() != storagetransferpb.TransferJob_DELETED {
		t.Errorf("after delete: status %v", after.GetStatus())
	} else if err != nil && storageJobsCode(err) != codes.NotFound {
		t.Errorf("get after delete: %v", err)
	}
}

// covers: storagetransfer.transferJobs.create, storagetransfer.transferJobs.get, storagetransfer.transferOperations.get
//
// TestStorageTransferScheduleFollowsTheInstanceClock (#1957): a job
// scheduled once, an hour ahead on the instance clock, has not run on
// create, and runs once the clock is moved past its start.
func TestStorageTransferScheduleFollowsTheInstanceClock(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := transferClient(t, h)
	ctx := h.Context()
	src, dst := h.Project()+"-sss", h.Project()+"-ssd"
	sb := companionBucket(t, h, sc, src, "")
	db := companionBucket(t, h, sc, dst, "")
	putCompanionObject(t, ctx, sb, "nightly", "N")
	start := storageJobsClock(t, h.Endpoint(EnvStorageTransfer), 0).Add(time.Hour).UTC()
	day := civilDate(start)
	job := createTransferJob(t, h, c, &storagetransferpb.TransferJob{TransferSpec: gcsSpec(src, dst),
		Schedule: &storagetransferpb.Schedule{ScheduleStartDate: day, ScheduleEndDate: day,
			StartTimeOfDay: &timeofday.TimeOfDay{Hours: int32(start.Hour()), Minutes: int32(start.Minute())}}})
	time.Sleep(3 * time.Second)
	if got, err := c.GetTransferJob(ctx, &storagetransferpb.GetTransferJobRequest{JobName: job.GetName(), ProjectId: h.Project()}); err != nil ||
		got.GetLatestOperationName() != "" {
		t.Fatalf("before its start time the job ran: %q, %v", got.GetLatestOperationName(), err)
	}
	storageJobsClock(t, h.Endpoint(EnvStorageTransfer), 2*time.Hour)
	got, err := c.GetTransferJob(ctx, &storagetransferpb.GetTransferJobRequest{JobName: job.GetName(), ProjectId: h.Project()})
	if err != nil || got.GetLatestOperationName() == "" {
		t.Fatalf("after the clock passed its start the job has not run: %+v, %v", got, err)
	}
	if md := waitTransfer(t, h, c, got.GetLatestOperationName()); md.GetStatus() != storagetransferpb.TransferOperation_SUCCESS {
		t.Errorf("scheduled run: %v", md.GetStatus())
	}
	if body := objectBody(t, ctx, db, "nightly"); body != "N" {
		t.Errorf("sink nightly = %q", body)
	}
}

// covers: storagetransfer.projects.agentPools.create, storagetransfer.projects.agentPools.get, storagetransfer.projects.agentPools.list, storagetransfer.projects.agentPools.patch, storagetransfer.projects.agentPools.delete, storagetransfer.transferOperations.pause, storagetransfer.transferOperations.resume, storagetransfer.transferOperations.cancel
//
// TestStorageTransferPosixAgentPool (#1957): an agent pool limited to
// 1 MB/s (limitMbps is megabytes per second, as Google documents) carries a
// transfer of a 16 MB local file slowly enough to pause it (PAUSED, and
// pause refused when not running), resume it and cancel it (ABORTED);
// patched to 8 MB/s, the next run takes about 2 s, not the 16 s a megabit
// limit would, and the object holds the file; patched to no limit, the
// next run finds it unchanged and skips it.
func TestStorageTransferPosixAgentPool(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := transferClient(t, h)
	ctx := h.Context()
	dst := h.Project() + "-spd"
	db := companionBucket(t, h, sc, dst, "")
	dir := t.TempDir()
	data := bytes.Repeat([]byte("burrow-"), 16<<20/7)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	poolID := "pool-" + strings.TrimPrefix(h.Project(), "cb-test-")
	pool, err := c.CreateAgentPool(ctx, &storagetransferpb.CreateAgentPoolRequest{ProjectId: h.Project(), AgentPoolId: poolID,
		AgentPool: &storagetransferpb.AgentPool{DisplayName: "slow pool", BandwidthLimit: &storagetransferpb.AgentPool_BandwidthLimit{LimitMbps: 1}}})
	if err != nil {
		t.Fatalf("CreateAgentPool: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteAgentPool(context.Background(), &storagetransferpb.DeleteAgentPoolRequest{Name: pool.GetName()})
	})
	if pool.GetName() != "projects/"+h.Project()+"/agentPools/"+poolID || pool.GetState() != storagetransferpb.AgentPool_CREATED {
		t.Errorf("created pool: %+v", pool)
	}
	if got, err := c.GetAgentPool(ctx, &storagetransferpb.GetAgentPoolRequest{Name: pool.GetName()}); err != nil || got.GetBandwidthLimit().GetLimitMbps() != 1 {
		t.Errorf("GetAgentPool: %+v, %v", got, err)
	}
	found := false
	pit := c.ListAgentPools(ctx, &storagetransferpb.ListAgentPoolsRequest{ProjectId: h.Project()})
	for {
		p, err := pit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListAgentPools: %v", err)
		}
		found = found || p.GetName() == pool.GetName()
	}
	if !found {
		t.Errorf("ListAgentPools lacks %s", pool.GetName())
	}

	job := createTransferJob(t, h, c, &storagetransferpb.TransferJob{TransferSpec: &storagetransferpb.TransferSpec{
		DataSource:          &storagetransferpb.TransferSpec_PosixDataSource{PosixDataSource: &storagetransferpb.PosixFilesystem{RootDirectory: dir}},
		DataSink:            &storagetransferpb.TransferSpec_GcsDataSink{GcsDataSink: &storagetransferpb.GcsData{BucketName: dst, Path: "posix/"}},
		SourceAgentPoolName: pool.GetName(),
	}})
	op := runTransfer(t, h, c, job.GetName())
	waitTransferStatus(t, h, c, op, storagetransferpb.TransferOperation_IN_PROGRESS)
	if err := c.PauseTransferOperation(ctx, &storagetransferpb.PauseTransferOperationRequest{Name: op}); err != nil {
		t.Fatalf("PauseTransferOperation: %v", err)
	}
	waitTransferStatus(t, h, c, op, storagetransferpb.TransferOperation_PAUSED)
	if err := c.PauseTransferOperation(ctx, &storagetransferpb.PauseTransferOperationRequest{Name: op}); storageJobsCode(err) != codes.FailedPrecondition {
		t.Errorf("pause of a paused operation: %v, want FAILED_PRECONDITION", err)
	}
	if err := c.ResumeTransferOperation(ctx, &storagetransferpb.ResumeTransferOperationRequest{Name: op}); err != nil {
		t.Fatalf("ResumeTransferOperation: %v", err)
	}
	waitTransferStatus(t, h, c, op, storagetransferpb.TransferOperation_IN_PROGRESS)
	if err := c.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if md := waitTransfer(t, h, c, op); md.GetStatus() != storagetransferpb.TransferOperation_ABORTED {
		t.Errorf("after cancel: %v, want ABORTED", md.GetStatus())
	}

	if _, err := c.UpdateAgentPool(ctx, &storagetransferpb.UpdateAgentPoolRequest{
		AgentPool:  &storagetransferpb.AgentPool{Name: pool.GetName(), BandwidthLimit: &storagetransferpb.AgentPool_BandwidthLimit{LimitMbps: 8}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bandwidth_limit"}}}); err != nil {
		t.Fatalf("UpdateAgentPool: %v", err)
	}
	start := time.Now()
	op = runTransfer(t, h, c, job.GetName())
	if md := waitTransfer(t, h, c, op); md.GetStatus() != storagetransferpb.TransferOperation_SUCCESS ||
		md.GetCounters().GetBytesCopiedToSink() != int64(len(data)) {
		t.Errorf("8 MB/s run: %v, counters %+v", md.GetStatus(), md.GetCounters())
	}
	// 16 MB at 8 MB/s is 2 s; at 8 megabits it would be 16 s.
	if d := time.Since(start); d < 1500*time.Millisecond || d > 10*time.Second {
		t.Errorf("16 MB through an 8 MB/s pool took %v, want about 2 s", d)
	}
	if _, err := c.UpdateAgentPool(ctx, &storagetransferpb.UpdateAgentPoolRequest{
		AgentPool:  &storagetransferpb.AgentPool{Name: pool.GetName(), BandwidthLimit: &storagetransferpb.AgentPool_BandwidthLimit{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bandwidth_limit"}}}); err != nil {
		t.Fatalf("UpdateAgentPool: %v", err)
	}
	op = runTransfer(t, h, c, job.GetName())
	// The file is in the sink now, unchanged, so the run skips it.
	if md := waitTransfer(t, h, c, op); md.GetStatus() != storagetransferpb.TransferOperation_SUCCESS ||
		md.GetCounters().GetObjectsFromSourceSkippedBySync() != 1 {
		t.Errorf("unlimited run: %v, counters %+v", md.GetStatus(), md.GetCounters())
	}
	if got := objectBody(t, ctx, db, "posix/big.bin"); got != string(data) {
		t.Errorf("posix/big.bin holds %d bytes, want %d", len(got), len(data))
	}
}

// covers: storagetransfer.transferJobs.create, storagetransfer.transferJobs.run
//
// TestStorageTransferNotifiesPubSub (#1957): a job's notification
// configuration publishes its operation's success to a local Pub/Sub topic,
// with Google's attributes and the operation as the JSON payload.
func TestStorageTransferNotifiesPubSub(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := transferClient(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	src, dst := h.Project()+"-sns", h.Project()+"-snd"
	sb := companionBucket(t, h, sc, src, "")
	companionBucket(t, h, sc, dst, "")
	putCompanionObject(t, ctx, sb, "x", "X")
	tp := topic(t, h, ps, "sts-notify-"+strings.TrimPrefix(h.Project(), "cb-test-"))
	sub := subscription(t, h, ps, "sts-notify-sub-"+strings.TrimPrefix(h.Project(), "cb-test-"), tp)
	job := createTransferJob(t, h, c, &storagetransferpb.TransferJob{TransferSpec: gcsSpec(src, dst),
		NotificationConfig: &storagetransferpb.NotificationConfig{PubsubTopic: tp,
			EventTypes:    []storagetransferpb.NotificationConfig_EventType{storagetransferpb.NotificationConfig_TRANSFER_OPERATION_SUCCESS},
			PayloadFormat: storagetransferpb.NotificationConfig_JSON}})
	op := runTransfer(t, h, c, job.GetName())
	waitTransfer(t, h, c, op)
	m := receiveOne(t, ps, sub, 30*time.Second)
	if m == nil {
		t.Fatal("no notification")
	}
	a := m.Attributes
	if a["eventType"] != "TRANSFER_OPERATION_SUCCESS" || a["transferJobName"] != job.GetName() || a["transferOperationName"] != op ||
		a["projectId"] != h.Project() || a["payloadFormat"] != "JSON" || a["eventTime"] == "" {
		t.Errorf("notification attributes: %v", a)
	}
	var body struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(m.Data, &body); err != nil || body.Name != op || body.Status != "SUCCESS" {
		t.Errorf("notification payload %s: %v", m.Data, err)
	}
}

// TestStorageTransferRefusesSourcesOutsideTheInstance (#1957): Amazon S3,
// Azure Blob Storage and HTTP URL list sources are refused as unimplemented,
// saying that CloudBurrow keeps every transfer local.
func TestStorageTransferRefusesSourcesOutsideTheInstance(t *testing.T) {
	h := New(t)
	c := transferClient(t, h)
	ctx := h.Context()
	sink := &storagetransferpb.TransferSpec_GcsDataSink{GcsDataSink: &storagetransferpb.GcsData{BucketName: h.Project() + "-nowhere"}}
	for what, spec := range map[string]*storagetransferpb.TransferSpec{
		"Amazon S3": {DataSink: sink, DataSource: &storagetransferpb.TransferSpec_AwsS3DataSource{AwsS3DataSource: &storagetransferpb.AwsS3Data{BucketName: "b"}}},
		"Azure": {DataSink: sink, DataSource: &storagetransferpb.TransferSpec_AzureBlobStorageDataSource{
			AzureBlobStorageDataSource: &storagetransferpb.AzureBlobStorageData{StorageAccount: "a", Container: "c"}}},
		"an HTTP URL list": {DataSink: sink, DataSource: &storagetransferpb.TransferSpec_HttpDataSource{HttpDataSource: &storagetransferpb.HttpData{ListUrl: "https://example.com/list.tsv"}}},
	} {
		j, err := c.CreateTransferJob(ctx, &storagetransferpb.CreateTransferJobRequest{TransferJob: &storagetransferpb.TransferJob{
			ProjectId: h.Project(), Status: storagetransferpb.TransferJob_ENABLED, TransferSpec: spec}})
		if err == nil {
			_ = c.DeleteTransferJob(ctx, &storagetransferpb.DeleteTransferJobRequest{JobName: j.GetName(), ProjectId: h.Project()})
		}
		if storageJobsCode(err) != codes.Unimplemented || !strings.Contains(strings.ToLower(fmt.Sprint(err)), "local") {
			t.Errorf("a source at %s: %v, want UNIMPLEMENTED saying transfers stay local", what, err)
		}
	}
}
