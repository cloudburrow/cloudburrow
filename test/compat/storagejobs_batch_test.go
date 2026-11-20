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
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/storage"
	sbo "cloud.google.com/go/storagebatchoperations/apiv1"
	"cloud.google.com/go/storagebatchoperations/apiv1/storagebatchoperationspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	sbov1 "google.golang.org/api/storagebatchoperations/v1"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc/codes"
)

// The Cloud Storage companion APIs (#1957), each on its own port.
const (
	EnvStorageBatch    = "CLOUDBURROW_TEST_STORAGE_BATCH"
	EnvStorageInsights = "CLOUDBURROW_TEST_STORAGE_INSIGHTS"
	EnvStorageTransfer = "CLOUDBURROW_TEST_STORAGE_TRANSFER"
)

// storageJobsCode is the canonical code a REST error's envelope names (gax
// maps only the HTTP status, so FAILED_PRECONDITION reads as
// INVALID_ARGUMENT otherwise).
func storageJobsCode(err error) codes.Code { return kmsCode("rest", err) }

// storageJobsClock moves the companion APIs' shared clock (a CloudBurrow
// extension, POST /cloudburrow/v1/clock:advance) forward by d, and returns
// the clock's time after the move. d == 0 only reads it.
func storageJobsClock(t *testing.T, endpoint string, d time.Duration) time.Time {
	t.Helper()
	method, path, body := http.MethodGet, "/cloudburrow/v1/clock", ""
	if d > 0 {
		method, path = http.MethodPost, "/cloudburrow/v1/clock:advance"
		body = fmt.Sprintf(`{"duration":"%ds"}`, int64(d/time.Second))
	}
	req, err := http.NewRequest(method, "http://"+stripScheme(endpoint)+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	var out struct{ Now time.Time }
	if err := json.Unmarshal(raw, &out); err != nil || out.Now.IsZero() {
		t.Fatalf("clock answer %s: %v", raw, err)
	}
	return out.Now
}

// setStorageIntelligence sets the project's Storage Intelligence edition
// through the Storage Control API's REST mapping on the storage port, as
// batch operations require an enrolled project.
func setStorageIntelligence(t *testing.T, h *Harness, edition string) {
	t.Helper()
	u := h.Endpoint(EnvStorage) + "/v2/projects/" + h.Project() + "/locations/global/intelligenceConfig?updateMask=edition_config"
	req, err := http.NewRequest(http.MethodPatch, u, bytes.NewReader([]byte(`{"editionConfig":"`+edition+`"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("set Storage Intelligence %s: %d %s", edition, resp.StatusCode, raw)
	}
}

func batchClient(t *testing.T, h *Harness) *sbo.Client {
	t.Helper()
	c, err := sbo.NewRESTClient(h.Context(), option.WithEndpoint("http://"+stripScheme(h.Endpoint(EnvStorageBatch))),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// companionBucket creates a bucket named name (in location, "" for the
// default) and empties and deletes it afterwards.
func companionBucket(t *testing.T, h *Harness, c *storage.Client, name, loc string) *storage.BucketHandle {
	t.Helper()
	bh := c.Bucket(name)
	var attrs *storage.BucketAttrs
	if loc != "" {
		attrs = &storage.BucketAttrs{Location: loc}
	}
	if err := bh.Create(h.Context(), h.Project(), attrs); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	return bh
}

func putCompanionObject(t *testing.T, ctx context.Context, bh *storage.BucketHandle, name, body string) {
	t.Helper()
	w := bh.Object(name).NewWriter(ctx)
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func objectExists(t *testing.T, ctx context.Context, bh *storage.BucketHandle, name string) bool {
	t.Helper()
	_, err := bh.Object(name).Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false
	}
	if err != nil {
		t.Fatalf("attrs %s: %v", name, err)
	}
	return true
}

// runBatchJob creates a job and waits for its operation, returning the
// job the operation answers.
func runBatchJob(t *testing.T, h *Harness, c *sbo.Client, id string, job *storagebatchoperationspb.Job) *storagebatchoperationspb.Job {
	t.Helper()
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/global"
	op, err := c.CreateJob(ctx, &storagebatchoperationspb.CreateJobRequest{Parent: parent, JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob %s: %v", id, err)
	}
	t.Cleanup(func() {
		_ = c.DeleteJob(context.Background(), &storagebatchoperationspb.DeleteJobRequest{Name: parent + "/jobs/" + id})
	})
	if !strings.HasPrefix(op.Name(), parent+"/operations/") {
		t.Errorf("operation name %q is not under %s/operations/", op.Name(), parent)
	}
	got, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("job %s: %v", id, err)
	}
	return got
}

func prefixJob(bucket string, prefixes ...string) *storagebatchoperationspb.Job {
	return &storagebatchoperationspb.Job{Source: &storagebatchoperationspb.Job_BucketList{BucketList: &storagebatchoperationspb.BucketList{
		Buckets: []*storagebatchoperationspb.BucketList_Bucket{{Bucket: bucket,
			ObjectConfiguration: &storagebatchoperationspb.BucketList_Bucket_PrefixList{
				PrefixList: &storagebatchoperationspb.PrefixList{IncludedObjectPrefixes: prefixes}}}}}}}
}

func manifestJob(bucket, location string) *storagebatchoperationspb.Job {
	return &storagebatchoperationspb.Job{Source: &storagebatchoperationspb.Job_BucketList{BucketList: &storagebatchoperationspb.BucketList{
		Buckets: []*storagebatchoperationspb.BucketList_Bucket{{Bucket: bucket,
			ObjectConfiguration: &storagebatchoperationspb.BucketList_Bucket_Manifest{
				Manifest: &storagebatchoperationspb.Manifest{ManifestLocation: location}}}}}}}
}

// covers: storagebatchoperations.projects.locations.jobs.create, storagebatchoperations.projects.locations.jobs.get, storagebatchoperations.projects.locations.jobs.list, storagebatchoperations.projects.locations.jobs.cancel, storagebatchoperations.projects.locations.jobs.delete, storagebatchoperations.projects.locations.jobs.bucketOperations.list, storagebatchoperations.projects.locations.jobs.bucketOperations.get, storagebatchoperations.projects.locations.operations.get, storagebatchoperations.projects.locations.operations.list, storagebatchoperations.projects.locations.get, storagebatchoperations.projects.locations.list
//
// TestStorageBatchOperationsJobs (#1957), with
// cloud.google.com/go/storagebatchoperations/apiv1 (REST) and
// google.golang.org/api/storagebatchoperations/v1: over one bucket's
// objects, a dry run counts and changes nothing; a delete job over a prefix
// deletes exactly its objects; a put-metadata job sets content type and
// custom metadata; a manifest's hold job sets a temporary hold and a
// prefix's releases it, and a manifest row naming a missing object is one
// failed object with its error logged. Jobs are listed (and filtered by
// state), their bucket operations read, a finished job's cancel refused and
// its delete honoured.
func TestStorageBatchOperationsJobs(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := batchClient(t, h)
	ctx := h.Context()
	setStorageIntelligence(t, h, "STANDARD")
	t.Cleanup(func() { setStorageIntelligence(t, h, "INHERIT") })
	name := h.Project() + "-sbo"
	bh := companionBucket(t, h, sc, name, "")
	for _, o := range []string{"keep/a", "del/1", "del/2", "del/3"} {
		putCompanionObject(t, ctx, bh, o, "data of "+o)
	}
	parent := "projects/" + h.Project() + "/locations/global"
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")

	dry := prefixJob(name, "del/")
	dry.Transformation = &storagebatchoperationspb.Job_DeleteObject{DeleteObject: &storagebatchoperationspb.DeleteObject{}}
	dry.DryRun = true
	j := runBatchJob(t, h, c, "dry-"+suffix, dry)
	if j.GetState() != storagebatchoperationspb.Job_SUCCEEDED || j.GetCounters().GetTotalObjectCount() != 3 || !j.GetDryRun() {
		t.Errorf("dry run: state %v, counters %+v, dryRun %v", j.GetState(), j.GetCounters(), j.GetDryRun())
	}
	if !objectExists(t, ctx, bh, "del/1") {
		t.Error("a dry run deleted del/1")
	}

	del := prefixJob(name, "del/")
	del.Description = "delete del/"
	del.Transformation = &storagebatchoperationspb.Job_DeleteObject{DeleteObject: &storagebatchoperationspb.DeleteObject{PermanentObjectDeletionEnabled: true}}
	j = runBatchJob(t, h, c, "del-"+suffix, del)
	if j.GetState() != storagebatchoperationspb.Job_SUCCEEDED || j.GetCounters().GetTotalObjectCount() != 3 ||
		j.GetCounters().GetSucceededObjectCount() != 3 || j.GetCounters().GetFailedObjectCount() != 0 || j.GetCompleteTime() == nil {
		t.Errorf("delete job: state %v, counters %+v, completeTime %v", j.GetState(), j.GetCounters(), j.GetCompleteTime())
	}
	for _, o := range []string{"del/1", "del/2", "del/3"} {
		if objectExists(t, ctx, bh, o) {
			t.Errorf("%s survived the delete job", o)
		}
	}
	if !objectExists(t, ctx, bh, "keep/a") {
		t.Error("keep/a, outside the prefix, was deleted")
	}

	meta := prefixJob(name, "keep/")
	ct := "text/x-burrow"
	meta.Transformation = &storagebatchoperationspb.Job_PutMetadata{PutMetadata: &storagebatchoperationspb.PutMetadata{
		ContentType: &ct, CustomMetadata: map[string]string{"team": "burrow"}}}
	if j = runBatchJob(t, h, c, "meta-"+suffix, meta); j.GetCounters().GetSucceededObjectCount() != 1 {
		t.Errorf("put metadata: %+v", j.GetCounters())
	}
	a, err := bh.Object("keep/a").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.ContentType != ct || a.Metadata["team"] != "burrow" {
		t.Errorf("after put metadata: contentType %q, metadata %v", a.ContentType, a.Metadata)
	}

	putCompanionObject(t, ctx, bh, "manifest.csv", "bucket,name,generation\n"+name+",keep/a,\n"+name+",missing,\n")
	hold := manifestJob(name, "gs://"+name+"/manifest.csv")
	hold.Transformation = &storagebatchoperationspb.Job_PutObjectHold{PutObjectHold: &storagebatchoperationspb.PutObjectHold{
		TemporaryHold: storagebatchoperationspb.PutObjectHold_SET}}
	j = runBatchJob(t, h, c, "hold-"+suffix, hold)
	if j.GetCounters().GetTotalObjectCount() != 2 || j.GetCounters().GetSucceededObjectCount() != 1 || j.GetCounters().GetFailedObjectCount() != 1 {
		t.Errorf("manifest hold job counters: %+v", j.GetCounters())
	}
	logged := false
	for _, s := range j.GetErrorSummaries() {
		for _, e := range s.GetErrorLogEntries() {
			if e.GetObjectUri() == "gs://"+name+"/missing" {
				logged = true
			}
		}
	}
	if !logged {
		t.Errorf("the missing object's error is not logged: %+v", j.GetErrorSummaries())
	}
	if a, err = bh.Object("keep/a").Attrs(ctx); err != nil || !a.TemporaryHold {
		t.Fatalf("after the hold job: %+v, %v", a, err)
	}
	release := prefixJob(name, "keep/")
	release.Transformation = &storagebatchoperationspb.Job_PutObjectHold{PutObjectHold: &storagebatchoperationspb.PutObjectHold{
		TemporaryHold: storagebatchoperationspb.PutObjectHold_UNSET}}
	runBatchJob(t, h, c, "unhold-"+suffix, release)
	if a, err = bh.Object("keep/a").Attrs(ctx); err != nil || a.TemporaryHold {
		t.Errorf("after the release job: %+v, %v", a, err)
	}

	// The discovery client reads the same job.
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	dc, err := sbov1.NewService(ctx, option.WithEndpoint("http://"+stripScheme(h.Endpoint(EnvStorageBatch))+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	dj, err := dc.Projects.Locations.Jobs.Get(parent + "/jobs/del-" + suffix).Context(ctx).Do()
	if err != nil || dj.State != "SUCCEEDED" || dj.Counters == nil || dj.Counters.SucceededObjectCount != 3 {
		t.Errorf("discovery jobs.get: %+v, %v", dj, err)
	}

	var listed []string
	it := c.ListJobs(ctx, &storagebatchoperationspb.ListJobsRequest{Parent: parent, Filter: "state=SUCCEEDED", PageSize: 2})
	for {
		lj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		if lj.GetState() != storagebatchoperationspb.Job_SUCCEEDED {
			t.Errorf("filter state=SUCCEEDED listed %s in %v", lj.GetName(), lj.GetState())
		}
		listed = append(listed, lj.GetName())
	}
	for _, id := range []string{"dry-", "del-", "meta-", "hold-", "unhold-"} {
		if !strings.Contains(strings.Join(listed, " "), parent+"/jobs/"+id+suffix) {
			t.Errorf("ListJobs lacks %s%s: %v", id, suffix, listed)
		}
	}

	jobName := parent + "/jobs/del-" + suffix
	bit := c.ListBucketOperations(ctx, &storagebatchoperationspb.ListBucketOperationsRequest{Parent: jobName})
	bo, err := bit.Next()
	if err != nil || bo.GetBucketName() != name {
		t.Fatalf("ListBucketOperations: %+v, %v", bo, err)
	}
	if got, err := c.GetBucketOperation(ctx, &storagebatchoperationspb.GetBucketOperationRequest{Name: bo.GetName()}); err != nil ||
		got.GetCounters().GetSucceededObjectCount() != 3 {
		t.Errorf("GetBucketOperation %s: %+v, %v", bo.GetName(), got, err)
	}

	ops, err := dc.Projects.Locations.Operations.List(parent).Context(ctx).Do()
	if err != nil || len(ops.Operations) < 5 {
		t.Errorf("operations.list: %+v, %v", ops, err)
	} else if op, err := dc.Projects.Locations.Operations.Get(ops.Operations[0].Name).Context(ctx).Do(); err != nil || !op.Done {
		t.Errorf("operations.get: %+v, %v", op, err)
	}

	if _, err := c.CancelJob(ctx, &storagebatchoperationspb.CancelJobRequest{Name: jobName}); storageJobsCode(err) != codes.FailedPrecondition {
		t.Errorf("cancel of a finished job: %v, want FAILED_PRECONDITION", err)
	}
	if err := c.DeleteJob(ctx, &storagebatchoperationspb.DeleteJobRequest{Name: jobName}); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, err := c.GetJob(ctx, &storagebatchoperationspb.GetJobRequest{Name: jobName}); storageJobsCode(err) != codes.NotFound {
		t.Errorf("get after delete: %v, want NOT_FOUND", err)
	}

	if loc, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent}); err != nil || loc.GetLocationId() != "global" {
		t.Errorf("GetLocation: %+v, %v", loc, err)
	}
	if locs, err := dc.Projects.Locations.List("projects/" + h.Project()).Context(ctx).Do(); err != nil || len(locs.Locations) == 0 {
		t.Errorf("locations.list: %+v, %v", locs, err)
	}
}

// covers: storagebatchoperations.projects.locations.jobs.create
//
// TestStorageBatchOperationsRefusals (#1957): what Google refuses is
// refused. A project without Storage Intelligence cannot create a job; a
// location other than global, a missing job ID, a duplicate one, more than
// 1,000 prefixes, and a job with no transformation are invalid.
func TestStorageBatchOperationsRefusals(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := batchClient(t, h)
	ctx := h.Context()
	name := h.Project() + "-sbr"
	companionBucket(t, h, sc, name, "")
	parent := "projects/" + h.Project() + "/locations/global"
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	job := func() *storagebatchoperationspb.Job {
		j := prefixJob(name, "x/")
		j.Transformation = &storagebatchoperationspb.Job_DeleteObject{DeleteObject: &storagebatchoperationspb.DeleteObject{}}
		return j
	}
	create := func(parent, id string, j *storagebatchoperationspb.Job) error {
		op, err := c.CreateJob(ctx, &storagebatchoperationspb.CreateJobRequest{Parent: parent, JobId: id, Job: j})
		if err == nil {
			_, err = op.Wait(ctx)
			t.Cleanup(func() {
				_ = c.DeleteJob(context.Background(), &storagebatchoperationspb.DeleteJobRequest{Name: parent + "/jobs/" + id})
			})
		}
		return err
	}

	setStorageIntelligence(t, h, "DISABLED")
	t.Cleanup(func() { setStorageIntelligence(t, h, "INHERIT") })
	err := create(parent, "noint-"+suffix, job())
	if storageJobsCode(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Storage Intelligence") {
		t.Errorf("a job in a project without Storage Intelligence: %v, want FAILED_PRECONDITION naming it", err)
	}
	setStorageIntelligence(t, h, "STANDARD")

	if err := create("projects/"+h.Project()+"/locations/us-central1", "loc-"+suffix, job()); storageJobsCode(err) != codes.InvalidArgument {
		t.Errorf("location us-central1: %v, want INVALID_ARGUMENT", err)
	}
	if err := create(parent, "", job()); storageJobsCode(err) != codes.InvalidArgument {
		t.Errorf("no job ID: %v, want INVALID_ARGUMENT", err)
	}
	many := make([]string, 1001)
	for i := range many {
		many[i] = fmt.Sprintf("p%04d/", i)
	}
	big := prefixJob(name, many...)
	big.Transformation = &storagebatchoperationspb.Job_DeleteObject{DeleteObject: &storagebatchoperationspb.DeleteObject{}}
	if err := create(parent, "many-"+suffix, big); storageJobsCode(err) != codes.InvalidArgument {
		t.Errorf("1,001 prefixes: %v, want INVALID_ARGUMENT", err)
	}
	if err := create(parent, "none-"+suffix, prefixJob(name, "x/")); storageJobsCode(err) != codes.InvalidArgument {
		t.Errorf("no transformation: %v, want INVALID_ARGUMENT", err)
	}
	if err := create(parent, "dup-"+suffix, job()); err != nil {
		t.Fatalf("first dup job: %v", err)
	}
	if err := create(parent, "dup-"+suffix, job()); storageJobsCode(err) != codes.AlreadyExists {
		t.Errorf("a second job with the same ID: %v, want ALREADY_EXISTS", err)
	}
}

// covers: storagebatchoperations.projects.locations.jobs.create, storagebatchoperations.projects.locations.jobs.get
//
// TestStorageBatchOperationsRewriteToKMSKey (#1957): a rewrite job moves
// a prefix's objects to a Cloud KMS key, which the objects then name, and
// leaves their data as it was.
func TestStorageBatchOperationsRewriteToKMSKey(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := batchClient(t, h)
	h.Endpoint(EnvKMS)
	ctx := h.Context()
	setStorageIntelligence(t, h, "STANDARD")
	t.Cleanup(func() { setStorageIntelligence(t, h, "INHERIT") })
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	_, keys := storageCMEKKeys(t, h, sc, "sbo-ring-"+suffix, "sbo-key")
	name := h.Project() + "-sbk"
	bh := companionBucket(t, h, sc, name, "")
	putCompanionObject(t, ctx, bh, "enc/a", "secret a")
	putCompanionObject(t, ctx, bh, "plain/b", "plain b")
	j := prefixJob(name, "enc/")
	j.Transformation = &storagebatchoperationspb.Job_RewriteObject{RewriteObject: &storagebatchoperationspb.RewriteObject{KmsKey: &keys[0]}}
	got := runBatchJob(t, h, c, "kms-"+suffix, j)
	if got.GetState() != storagebatchoperationspb.Job_SUCCEEDED || got.GetCounters().GetSucceededObjectCount() != 1 {
		t.Fatalf("rewrite job: %v %+v %+v", got.GetState(), got.GetCounters(), got.GetErrorSummaries())
	}
	a, err := bh.Object("enc/a").Attrs(ctx)
	if err != nil || !strings.HasPrefix(a.KMSKeyName, keys[0]) {
		t.Errorf("enc/a after the rewrite: key %q, %v; want %s", a.KMSKeyName, err, keys[0])
	}
	if b, err := bh.Object("plain/b").Attrs(ctx); err != nil || b.KMSKeyName != "" {
		t.Errorf("plain/b, outside the prefix, names key %q (%v)", b.KMSKeyName, err)
	}
	r, err := bh.Object("enc/a").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if body, _ := io.ReadAll(r); string(body) != "secret a" {
		t.Errorf("enc/a reads %q after the rewrite", body)
	}
}

// covers: storagebatchoperations.projects.locations.operations.cancel, storagebatchoperations.projects.locations.operations.delete
//
// TestStorageBatchOperationsOperationCancelAndDelete (#1957), official Go
// client: a job over a manifest of 20,000 rows runs long enough that
// cancelling its long-running operation cancels the job (CANCELED, fewer
// objects processed than listed); cancelling it again is
// FAILED_PRECONDITION; deleting the operation makes operations.get
// NOT_FOUND while the job itself stays readable.
func TestStorageBatchOperationsOperationCancelAndDelete(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := batchClient(t, h)
	ctx := h.Context()
	setStorageIntelligence(t, h, "STANDARD")
	t.Cleanup(func() { setStorageIntelligence(t, h, "INHERIT") })
	name := h.Project() + "-sbc"
	bh := companionBucket(t, h, sc, name, "")
	var manifest strings.Builder
	manifest.WriteString("bucket,name,generation\n")
	for i := range 20000 {
		fmt.Fprintf(&manifest, "%s,missing/%05d,\n", name, i)
	}
	putCompanionObject(t, ctx, bh, "manifest.csv", manifest.String())
	parent := "projects/" + h.Project() + "/locations/global"
	id := "cancel-" + strings.TrimPrefix(h.Project(), "cb-test-")
	job := manifestJob(name, "gs://"+name+"/manifest.csv")
	job.Transformation = &storagebatchoperationspb.Job_PutObjectHold{PutObjectHold: &storagebatchoperationspb.PutObjectHold{
		TemporaryHold: storagebatchoperationspb.PutObjectHold_SET}}
	op, err := c.CreateJob(ctx, &storagebatchoperationspb.CreateJobRequest{Parent: parent, JobId: id, Job: job})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteJob(context.Background(), &storagebatchoperationspb.DeleteJobRequest{Name: parent + "/jobs/" + id})
	})
	if err := c.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.Name()}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	var j *storagebatchoperationspb.Job
	deadline := time.Now().Add(30 * time.Second)
	for {
		j, err = c.GetJob(ctx, &storagebatchoperationspb.GetJobRequest{Name: parent + "/jobs/" + id})
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if j.GetState() != storagebatchoperationspb.Job_RUNNING || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if j.GetState() != storagebatchoperationspb.Job_CANCELED {
		t.Fatalf("after operations.cancel the job is %v, want CANCELED", j.GetState())
	}
	if done := j.GetCounters().GetSucceededObjectCount() + j.GetCounters().GetFailedObjectCount(); done >= 20000 {
		t.Errorf("a cancelled job processed all %d objects", done)
	}
	if _, err := op.Wait(ctx); err == nil {
		t.Error("the cancelled job's operation finished without an error")
	}
	if err := c.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op.Name()}); storageJobsCode(err) != codes.FailedPrecondition {
		t.Errorf("second CancelOperation: %v, want FAILED_PRECONDITION", err)
	}
	if err := c.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: op.Name()}); err != nil {
		t.Fatalf("DeleteOperation: %v", err)
	}
	if _, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name()}); storageJobsCode(err) != codes.NotFound {
		t.Errorf("GetOperation after DeleteOperation: %v, want NOT_FOUND", err)
	}
	if _, err := c.GetJob(ctx, &storagebatchoperationspb.GetJobRequest{Name: parent + "/jobs/" + id}); err != nil {
		t.Errorf("the job is gone with its operation: %v", err)
	}
}
