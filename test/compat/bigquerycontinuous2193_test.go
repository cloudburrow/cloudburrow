//go:build compat

package compat

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	reservation "cloud.google.com/go/bigquery/reservation/apiv1"
	"cloud.google.com/go/bigquery/reservation/apiv1/reservationpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/spanner"
	bqresv "google.golang.org/api/bigqueryreservation/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// BigQuery continuous queries and the Reservation API (#2193), through
// Google's official Go clients: cloud.google.com/go/bigquery/reservation
// (REST and gRPC) for the Enterprise reservation and its CONTINUOUS
// assignment, cloud.google.com/go/bigquery for the continuous query's
// jobs.insert, and the Pub/Sub, Bigtable and Spanner clients for what it
// exports, as https://docs.cloud.google.com/bigquery/docs/continuous-queries
// documents it.

// reservationClients are the Reservation API's REST client, on the
// BigQuery front's port, and its gRPC client, on the Storage API's.
func reservationClients(t *testing.T, h *Harness) (rest, grpcClient *reservation.Client, project string) {
	t.Helper()
	endpoint := h.Endpoint(EnvBigQuery)
	storage := h.Endpoint(EnvBigQueryStorage)
	project = strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if project == "" {
		t.Skipf("%s is not set", EnvBigQueryProject)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		t.Fatalf("BigQuery endpoint %q: %v", endpoint, err)
	}
	ctx := h.Context()
	rest, err = reservation.NewRESTClient(ctx, option.WithEndpoint(u.Scheme+"://"+u.Host), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rest.Close() })
	grpcClient, err = reservation.NewClient(ctx, option.WithEndpoint(storage), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = grpcClient.Close() })
	return rest, grpcClient, project
}

// resvCode is a Reservation API error's canonical code: on gRPC its
// status, over REST the error body's "status", which Google's REST errors
// carry beside the HTTP code (400 is both INVALID_ARGUMENT and
// FAILED_PRECONDITION, 409 both ALREADY_EXISTS and ABORTED).
func resvCode(err error) codes.Code {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		var body struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(ge.Body), &body) == nil && body.Error.Status != "" {
			for c := codes.OK; c <= codes.Unauthenticated; c++ {
				if strings.EqualFold(strings.ReplaceAll(c.String(), "_", ""), strings.ReplaceAll(body.Error.Status, "_", "")) {
					return c
				}
			}
		}
	}
	return grpcCode(err)
}

// covers: bigqueryreservation.projects.locations.reservations.create, bigqueryreservation.projects.locations.reservations.get, bigqueryreservation.projects.locations.reservations.list, bigqueryreservation.projects.locations.reservations.patch, bigqueryreservation.projects.locations.reservations.delete, bigqueryreservation.projects.locations.reservations.assignments.create, bigqueryreservation.projects.locations.reservations.assignments.list, bigqueryreservation.projects.locations.reservations.assignments.delete, bigqueryreservation.projects.locations.reservations.assignments.patch, bigqueryreservation.projects.locations.reservations.assignments.move, bigqueryreservation.projects.locations.searchAllAssignments, bigqueryreservation.projects.locations.searchAssignments, bigqueryreservation.projects.locations.capacityCommitments.create, bigqueryreservation.projects.locations.capacityCommitments.get, bigqueryreservation.projects.locations.capacityCommitments.list, bigqueryreservation.projects.locations.capacityCommitments.patch, bigqueryreservation.projects.locations.capacityCommitments.split, bigqueryreservation.projects.locations.capacityCommitments.merge, bigqueryreservation.projects.locations.capacityCommitments.delete, bigqueryreservation.projects.locations.getBiReservation, bigqueryreservation.projects.locations.updateBiReservation
//
// TestBigQueryReservationAPI (#2193): reservations, assignments and
// capacity commitments through the REST and the gRPC client, with
// Google's documented rules: slots in multiples of 50, the edition kept,
// CONTINUOUS assignments not mixed with others, one assignment per
// assignee, job type and location, FAILED_PRECONDITION for a reservation
// with assignments and for a commitment before its end.
func TestBigQueryReservationAPI(t *testing.T) {
	h := New(t)
	rest, gc, project := reservationClients(t, h)
	ctx := h.Context()
	suffix := strings.ToLower(strings.ReplaceAll(h.Project(), "_", "-"))
	if len(suffix) > 40 {
		suffix = suffix[len(suffix)-40:]
	}
	parent := "projects/" + project + "/locations/US"
	for _, c := range []struct {
		name string
		cl   *reservation.Client
	}{{"rest", rest}, {"grpc", gc}} {
		t.Run(c.name, func(t *testing.T) {
			cl := c.cl
			id := "r" + c.name + "-" + suffix
			if _, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: id,
				Reservation: &reservationpb.Reservation{SlotCapacity: 75, Edition: reservationpb.Edition_ENTERPRISE}}); resvCode(err) != codes.InvalidArgument {
				t.Errorf("75 slots: %v, want INVALID_ARGUMENT", err)
			}
			if _, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: "Bad_" + id,
				Reservation: &reservationpb.Reservation{Edition: reservationpb.Edition_ENTERPRISE}}); resvCode(err) != codes.InvalidArgument {
				t.Errorf("bad ID: %v, want INVALID_ARGUMENT", err)
			}
			rv, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: id,
				Reservation: &reservationpb.Reservation{SlotCapacity: 100, Edition: reservationpb.Edition_ENTERPRISE,
					Autoscale: &reservationpb.Reservation_Autoscale{MaxSlots: 100}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = cl.DeleteReservation(h.Context(), &reservationpb.DeleteReservationRequest{Name: rv.GetName()})
			})
			if rv.GetName() != parent+"/reservations/"+id || rv.GetCreationTime() == nil || rv.GetPrimaryLocation() == "" {
				t.Errorf("created %v", rv)
			}
			if _, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: id,
				Reservation: &reservationpb.Reservation{Edition: reservationpb.Edition_ENTERPRISE}}); resvCode(err) != codes.AlreadyExists {
				t.Errorf("second create: %v, want ALREADY_EXISTS", err)
			}
			got, err := cl.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: rv.GetName()})
			if err != nil || got.GetSlotCapacity() != 100 || got.GetAutoscale().GetMaxSlots() != 100 {
				t.Errorf("get: %v %v", got, err)
			}
			found := false
			it := cl.ListReservations(ctx, &reservationpb.ListReservationsRequest{Parent: parent})
			for {
				r, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				found = found || r.GetName() == rv.GetName()
			}
			if !found {
				t.Error("list: the reservation is not listed")
			}
			if _, err := cl.UpdateReservation(ctx, &reservationpb.UpdateReservationRequest{Reservation: &reservationpb.Reservation{Name: rv.GetName(),
				Edition: reservationpb.Edition_ENTERPRISE_PLUS}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"edition"}}}); resvCode(err) != codes.InvalidArgument {
				t.Errorf("edition change: %v, want INVALID_ARGUMENT", err)
			}
			up, err := cl.UpdateReservation(ctx, &reservationpb.UpdateReservationRequest{Reservation: &reservationpb.Reservation{Name: rv.GetName(),
				SlotCapacity: 150}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"slot_capacity"}}})
			if err != nil || up.GetSlotCapacity() != 150 {
				t.Errorf("patch: %v %v", up, err)
			}
			if _, err := cl.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: rv.GetName(),
				Assignment: &reservationpb.Assignment{Assignee: "projects/" + project}}); resvCode(err) != codes.InvalidArgument {
				t.Errorf("no job type: %v, want INVALID_ARGUMENT", err)
			}
			assignee := "projects/other-" + c.name + "-" + suffix
			a, err := cl.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: rv.GetName(),
				Assignment: &reservationpb.Assignment{Assignee: assignee, JobType: reservationpb.Assignment_CONTINUOUS}})
			if err != nil {
				t.Fatal(err)
			}
			if a.GetState() != reservationpb.Assignment_ACTIVE || !strings.HasPrefix(a.GetName(), rv.GetName()+"/assignments/") {
				t.Errorf("assignment %v", a)
			}
			if _, err := cl.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: rv.GetName(),
				Assignment: &reservationpb.Assignment{Assignee: "projects/third-" + suffix, JobType: reservationpb.Assignment_QUERY}}); resvCode(err) != codes.InvalidArgument {
				t.Errorf("QUERY beside CONTINUOUS: %v, want INVALID_ARGUMENT", err)
			}
			if _, err := cl.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: rv.GetName(),
				Assignment: &reservationpb.Assignment{Assignee: assignee, JobType: reservationpb.Assignment_CONTINUOUS}}); resvCode(err) != codes.AlreadyExists {
				t.Errorf("second CONTINUOUS assignment: %v, want ALREADY_EXISTS", err)
			}
			n := 0
			ait := cl.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: rv.GetName()})
			for {
				_, err := ait.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				n++
			}
			if n != 1 {
				t.Errorf("listed %d assignments, want 1", n)
			}
			sit := cl.SearchAllAssignments(ctx, &reservationpb.SearchAllAssignmentsRequest{Parent: parent, Query: "assignee=" + assignee})
			if s, err := sit.Next(); err != nil || s.GetName() != a.GetName() {
				t.Errorf("searchAllAssignments: %v %v", s, err)
			}
			//lint:ignore SA1019 the deprecated method is still served
			sit2 := cl.SearchAssignments(ctx, &reservationpb.SearchAssignmentsRequest{Parent: parent, Query: "assignee=" + assignee})
			if s, err := sit2.Next(); err != nil || s.GetName() != a.GetName() {
				t.Errorf("searchAssignments: %v %v", s, err)
			}
			if p, err := cl.UpdateAssignment(ctx, &reservationpb.UpdateAssignmentRequest{Assignment: &reservationpb.Assignment{Name: a.GetName(),
				EnableGeminiInBigquery: true}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enable_gemini_in_bigquery"}}}); err != nil || !p.GetEnableGeminiInBigquery() {
				t.Errorf("patch assignment: %v %v", p, err)
			}
			if err := cl.DeleteReservation(ctx, &reservationpb.DeleteReservationRequest{Name: rv.GetName()}); resvCode(err) != codes.FailedPrecondition {
				t.Errorf("delete with an assignment: %v, want FAILED_PRECONDITION", err)
			}
			other, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: id + "-b",
				Reservation: &reservationpb.Reservation{SlotCapacity: 50, Edition: reservationpb.Edition_ENTERPRISE}})
			if err != nil {
				t.Fatal(err)
			}
			moved, err := cl.MoveAssignment(ctx, &reservationpb.MoveAssignmentRequest{Name: a.GetName(), DestinationId: other.GetName()})
			if err != nil || !strings.HasPrefix(moved.GetName(), other.GetName()+"/assignments/") {
				t.Fatalf("move: %v %v", moved, err)
			}
			if err := cl.DeleteAssignment(ctx, &reservationpb.DeleteAssignmentRequest{Name: moved.GetName()}); err != nil {
				t.Fatal(err)
			}
			if err := cl.DeleteReservation(ctx, &reservationpb.DeleteReservationRequest{Name: other.GetName()}); err != nil {
				t.Fatal(err)
			}
			if err := cl.DeleteReservation(ctx, &reservationpb.DeleteReservationRequest{Name: rv.GetName()}); err != nil {
				t.Fatal(err)
			}
			if _, err := cl.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: rv.GetName()}); resvCode(err) != codes.NotFound {
				t.Errorf("get after delete: %v, want NOT_FOUND", err)
			}

			// Capacity commitments.
			if _, err := cl.CreateCapacityCommitment(ctx, &reservationpb.CreateCapacityCommitmentRequest{Parent: parent,
				CapacityCommitment: &reservationpb.CapacityCommitment{SlotCount: 100}}); resvCode(err) != codes.InvalidArgument {
				t.Errorf("commitment without a plan: %v, want INVALID_ARGUMENT", err)
			}
			cm, err := cl.CreateCapacityCommitment(ctx, &reservationpb.CreateCapacityCommitmentRequest{Parent: parent, CapacityCommitmentId: "c" + id,
				CapacityCommitment: &reservationpb.CapacityCommitment{SlotCount: 100, Plan: reservationpb.CapacityCommitment_MONTHLY,
					Edition: reservationpb.Edition_ENTERPRISE}})
			if err != nil {
				t.Fatal(err)
			}
			if cm.GetState() != reservationpb.CapacityCommitment_ACTIVE ||
				cm.GetCommitmentEndTime().AsTime().Sub(cm.GetCommitmentStartTime().AsTime()) != 30*24*time.Hour {
				t.Errorf("commitment %v", cm)
			}
			if g, err := cl.GetCapacityCommitment(ctx, &reservationpb.GetCapacityCommitmentRequest{Name: cm.GetName()}); err != nil || g.GetSlotCount() != 100 {
				t.Errorf("get commitment: %v %v", g, err)
			}
			cit := cl.ListCapacityCommitments(ctx, &reservationpb.ListCapacityCommitmentsRequest{Parent: parent})
			listed := false
			for {
				x, err := cit.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				listed = listed || x.GetName() == cm.GetName()
			}
			if !listed {
				t.Error("the commitment is not listed")
			}
			if u, err := cl.UpdateCapacityCommitment(ctx, &reservationpb.UpdateCapacityCommitmentRequest{CapacityCommitment: &reservationpb.CapacityCommitment{
				Name: cm.GetName(), RenewalPlan: reservationpb.CapacityCommitment_ANNUAL}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"renewal_plan"}}}); err != nil ||
				u.GetRenewalPlan() != reservationpb.CapacityCommitment_ANNUAL {
				t.Errorf("patch commitment: %v %v", u, err)
			}
			sp, err := cl.SplitCapacityCommitment(ctx, &reservationpb.SplitCapacityCommitmentRequest{Name: cm.GetName(), SlotCount: 50})
			if err != nil || sp.GetFirst().GetSlotCount() != 50 || sp.GetSecond().GetSlotCount() != 50 {
				t.Fatalf("split: %v %v", sp, err)
			}
			ids := []string{lastPart(sp.GetFirst().GetName()), lastPart(sp.GetSecond().GetName())}
			mg, err := cl.MergeCapacityCommitments(ctx, &reservationpb.MergeCapacityCommitmentsRequest{Parent: parent, CapacityCommitmentIds: ids})
			if err != nil || mg.GetSlotCount() != 100 {
				t.Fatalf("merge: %v %v", mg, err)
			}
			if err := cl.DeleteCapacityCommitment(ctx, &reservationpb.DeleteCapacityCommitmentRequest{Name: mg.GetName()}); resvCode(err) != codes.FailedPrecondition {
				t.Errorf("delete before the end: %v, want FAILED_PRECONDITION", err)
			}

			bi, err := cl.UpdateBiReservation(ctx, &reservationpb.UpdateBiReservationRequest{BiReservation: &reservationpb.BiReservation{
				Name: parent + "/biReservation", Size: 1 << 30}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"size"}}})
			if err != nil || bi.GetSize() != 1<<30 {
				t.Errorf("updateBiReservation: %v %v", bi, err)
			}
			if g, err := cl.GetBiReservation(ctx, &reservationpb.GetBiReservationRequest{Name: parent + "/biReservation"}); err != nil || g.GetSize() != 1<<30 {
				t.Errorf("getBiReservation: %v %v", g, err)
			}
		})
	}
}

func lastPart(n string) string { return n[strings.LastIndex(n, "/")+1:] }

// continuousReservation makes an Enterprise reservation with a CONTINUOUS
// assignment of project, removed at cleanup.
func continuousReservation(t *testing.T, h *Harness, cl *reservation.Client, project, id string) {
	t.Helper()
	ctx := h.Context()
	parent := "projects/" + project + "/locations/US"
	rv, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: id,
		Reservation: &reservationpb.Reservation{SlotCapacity: 100, Edition: reservationpb.Edition_ENTERPRISE}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := cl.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: rv.GetName(),
		Assignment: &reservationpb.Assignment{Assignee: "projects/" + project, JobType: reservationpb.Assignment_CONTINUOUS}})
	if err != nil {
		_ = cl.DeleteReservation(ctx, &reservationpb.DeleteReservationRequest{Name: rv.GetName()})
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cl.DeleteAssignment(h.Context(), &reservationpb.DeleteAssignmentRequest{Name: a.GetName()})
		_ = cl.DeleteReservation(h.Context(), &reservationpb.DeleteReservationRequest{Name: rv.GetName()})
	})
}

// startContinuous inserts a continuous query job, as a service account
// when sa is not "", and waits until it runs.
func startContinuous(t *testing.T, h *Harness, bq *bigquery.Client, sql, sa string) *bigquery.Job {
	t.Helper()
	q := bq.Query(sql)
	q.Continuous = true
	if sa != "" {
		q.ConnectionProperties = []*bigquery.ConnectionProperty{{Key: "service_account", Value: sa}}
	}
	job, err := q.Run(h.Context())
	if err != nil {
		t.Fatalf("continuous query: %v", err)
	}
	st, err := job.Status(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != bigquery.Running || st.Err() != nil {
		t.Fatalf("continuous query state %v %v, want RUNNING", st.State, st.Err())
	}
	return job
}

// cancelContinuous cancels job and waits for it to end, stopped.
func cancelContinuous(t *testing.T, h *Harness, job *bigquery.Job) {
	t.Helper()
	ctx := h.Context()
	if err := job.Cancel(ctx); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, err := job.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Done() {
			var e *bigquery.Error
			if !errors.As(st.Err(), &e) || e.Reason != "stopped" {
				t.Errorf("cancelled job: %v, want reason stopped", st.Err())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled continuous query is still running after 60s")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// covers: bigquery.jobs.insert, bigquery.jobs.cancel
//
// TestBigQueryContinuousQueryPubSub (#2193): the documented flow — an
// Enterprise reservation with a CONTINUOUS assignment, a continuous query
// that exports to Pub/Sub as a service account, rows inserted after it
// started received as messages with their attributes, the job RUNNING in
// INFORMATION_SCHEMA.JOBS with continuous TRUE and an output watermark,
// then cancelled (reason stopped). Before the assignment, and without a
// service account, and with an unsupported operator, the job is refused.
func TestBigQueryContinuousQueryPubSub(t *testing.T) {
	h := New(t)
	t.Setenv("PUBSUB_EMULATOR_HOST", runShardEndpoint(h, envSinkPubSub, EnvPubSub))
	bq, project := bigqueryClient(t, h)
	rest, _, _ := reservationClients(t, h)
	ctx := h.Context()
	ps, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	ds := bq.Dataset("cq_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	src := ds.Table("rides")
	if err := src.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "status", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	topic := "projects/" + h.Project() + "/topics/cq-rides"
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.TopicAdminClient.DeleteTopic(h.Context(), &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	subName := "projects/" + h.Project() + "/subscriptions/cq-rides"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSubscription(h.Context(), &pubsubpb.DeleteSubscriptionRequest{Subscription: subName})
	})
	table := "`" + project + "." + src.DatasetID + ".rides`"
	sql := "EXPORT DATA OPTIONS (format = 'CLOUD_PUBSUB', uri = 'https://pubsub.googleapis.com/" + topic + "') AS (" +
		"SELECT TO_JSON_STRING(STRUCT(id, status)) AS message, TO_JSON(STRUCT(CAST(id AS STRING) AS ride_id)) AS _ATTRIBUTES " +
		"FROM " + table + " WHERE status = 'done')"
	sa := "cq-runner@" + project + ".iam.gserviceaccount.com"

	refused := func(what, sql, sa, want string) {
		t.Helper()
		q := bq.Query(sql)
		q.Continuous = true
		if sa != "" {
			q.ConnectionProperties = []*bigquery.ConnectionProperty{{Key: "service_account", Value: sa}}
		}
		_, err := q.Run(ctx)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != 400 || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want 400 %q", what, err, want)
		}
	}
	refused("on demand", sql, sa, "CONTINUOUS assignment")
	continuousReservation(t, h, rest, project, "cq-"+strings.ToLower(lastPart(strings.ReplaceAll(h.Project(), "_", "-"))))
	refused("no service account", sql, "", "service account")
	refused("SELECT DISTINCT", strings.Replace(sql, "SELECT TO_JSON_STRING", "SELECT DISTINCT TO_JSON_STRING", 1), sa, "SELECT DISTINCT")

	// Rows from before the job started are not exported.
	if err := runDML(ctx, bq.Query("INSERT INTO "+table+" (id, status) VALUES (0, 'done')")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	job := startContinuous(t, h, bq, sql, sa)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = job.Cancel(context.Background())
		}
	})

	if err := runDML(ctx, bq.Query("INSERT INTO "+table+" (id, status) VALUES (1, 'done'), (2, 'pending'), (3, 'done')")); err != nil {
		t.Fatal(err)
	}
	ins := src.Inserter()
	if err := ins.Put(ctx, []*bigquery.ValuesSaver{{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "status", Type: bigquery.StringFieldType}}, Row: []bigquery.Value{int64(4), "done"}}}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	got := map[string]string{}
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	err = ps.Subscriber(subName).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
		m.Ack()
		mu.Lock()
		defer mu.Unlock()
		got[string(m.Data)] = m.Attributes["ride_id"]
		if len(got) >= 3 {
			cancel()
		}
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	mu.Lock()
	want := map[string]string{`{"id":1,"status":"done"}`: "1", `{"id":3,"status":"done"}`: "3", `{"id":4,"status":"done"}`: "4"}
	if len(got) != len(want) {
		t.Errorf("received %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("message %s: attribute ride_id %q, want %q (received %v)", k, got[k], v, got)
		}
	}
	mu.Unlock()

	// INFORMATION_SCHEMA.JOBS: continuous, its output watermark, its
	// reservation and edition.
	it, err := bq.Query("SELECT continuous, continuous_query_info.output_watermark IS NOT NULL, state, edition, reservation_id " +
		"FROM `region-us`.INFORMATION_SCHEMA.JOBS WHERE job_id = '" + job.ID() + "'").Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatalf("JOBS row of %s: %v", job.ID(), err)
	}
	if row[0] != true || row[1] != true || row[2] != "RUNNING" || row[3] != "ENTERPRISE" || !strings.Contains(row[4].(string), ".cq-") {
		t.Errorf("JOBS row %v, want continuous, a watermark, RUNNING, ENTERPRISE and the reservation", row)
	}
	cancelContinuous(t, h, job)
	stopped = true
}

// covers: bigquery.jobs.insert
//
// TestBigQueryContinuousQueryInsert (#2193): a continuous INSERT into a
// BigQuery table, with a SQL UDF made by CREATE TEMP FUNCTION and an
// APPENDS start, then cancelled; rows appended after the start reach the
// destination, transformed.
func TestBigQueryContinuousQueryInsert(t *testing.T) {
	h := New(t)
	bq, project := bigqueryClient(t, h)
	rest, _, _ := reservationClients(t, h)
	ctx := h.Context()
	ds := bq.Dataset("cqi_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	schema := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "name", Type: bigquery.StringFieldType}}
	for _, n := range []string{"src", "dst"} {
		if err := ds.Table(n).Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
	}
	continuousReservation(t, h, rest, project, "cqi-"+strings.ToLower(lastPart(strings.ReplaceAll(h.Project(), "_", "-"))))
	src, dst := "`"+project+"."+ds.DatasetID+".src`", "`"+project+"."+ds.DatasetID+".dst`"
	sql := "CREATE TEMP FUNCTION shout(s STRING) AS (UPPER(s) || '!');\n" +
		"INSERT INTO " + dst + " (id, name) SELECT id, shout(name) FROM APPENDS(TABLE " + src + ", CURRENT_TIMESTAMP() - INTERVAL 10 MINUTE) WHERE id > 0"
	if err := runDML(ctx, bq.Query("INSERT INTO "+src+" (id, name) VALUES (1, 'before')")); err != nil {
		t.Fatal(err)
	}
	job := startContinuous(t, h, bq, sql, "")
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = job.Cancel(context.Background())
		}
	})
	if err := runDML(ctx, bq.Query("INSERT INTO "+src+" (id, name) VALUES (2, 'after'), (-1, 'skipped')")); err != nil {
		t.Fatal(err)
	}
	var names []string
	eventually1874(t, "dst rows", 60*time.Second, func() bool {
		names = nil
		it, err := bq.Query("SELECT name FROM " + dst + " ORDER BY id").Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for {
			var r []bigquery.Value
			if err := it.Next(&r); err == iterator.Done {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			names = append(names, r[0].(string))
		}
		return len(names) >= 2
	})
	if strings.Join(names, ",") != "BEFORE!,AFTER!" {
		t.Errorf("dst rows %v, want BEFORE! (within the APPENDS start) and AFTER!", names)
	}
	cancelContinuous(t, h, job)
	stopped = true
}

// covers: bigquery.jobs.insert
//
// TestBigQueryContinuousQueryBigtableAndSpanner (#2193): continuous
// exports to Bigtable (a rowkey and a column family, 8-byte big-endian
// INT64) and to Spanner (an existing table, InsertOrUpdate), each read
// back with its official client.
func TestBigQueryContinuousQueryBigtableAndSpanner(t *testing.T) {
	h := New(t)
	bq, project := bigqueryClient(t, h)
	rest, _, _ := reservationClients(t, h)
	btEndpoint := h.Endpoint(EnvBigtable)
	spAddr := h.Endpoint(EnvSpanner)
	ctx := h.Context()
	ds := bq.Dataset("cqx_" + strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	if err := ds.Table("src").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "name", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatal(err)
	}
	src := "`" + project + "." + ds.DatasetID + ".src`"
	continuousReservation(t, h, rest, project, "cqx-"+strings.ToLower(lastPart(strings.ReplaceAll(h.Project(), "_", "-"))))

	// Bigtable.
	t.Setenv("BIGTABLE_EMULATOR_HOST", btEndpoint)
	btTable := "cq_" + strings.ReplaceAll(h.Project(), "-", "_")
	_, btc := bigtableTable(t, h, btTable)
	btSQL := "EXPORT DATA OPTIONS (format = 'CLOUD_BIGTABLE', uri = 'https://bigtable.googleapis.com/projects/" + h.Project() +
		"/instances/" + bigtableInstance + "/appProfiles/default/tables/" + btTable + "') AS (" +
		"SELECT CAST(id AS STRING) AS rowkey, STRUCT(name, id) AS info FROM " + src + ")"
	btJob := startContinuous(t, h, bq, btSQL, "")

	// Spanner.
	inst, db := "cq-inst", "cqdb"
	spannerDatabase(t, ctx, spAddr, h.Project(), inst, db, "CREATE TABLE people (id INT64 NOT NULL, name STRING(MAX)) PRIMARY KEY (id)")
	spSQL := "EXPORT DATA OPTIONS (format = 'CLOUD_SPANNER', uri = 'https://spanner.googleapis.com/projects/" + h.Project() +
		"/instances/" + inst + "/databases/" + db + "', spanner_options = '{\"table\": \"people\"}') AS (SELECT id, name FROM " + src + ")"
	spJob := startContinuous(t, h, bq, spSQL, "")
	defer func() {
		_ = btJob.Cancel(context.Background())
		_ = spJob.Cancel(context.Background())
	}()

	if err := runDML(ctx, bq.Query("INSERT INTO "+src+" (id, name) VALUES (7, 'seven'), (8, 'eight')")); err != nil {
		t.Fatal(err)
	}
	tbl := btc.Open(btTable)
	eventually1874(t, "bigtable rows", 60*time.Second, func() bool {
		r, err := tbl.ReadRow(ctx, "8")
		return err == nil && len(r["info"]) == 2
	})
	r, err := tbl.ReadRow(ctx, "7")
	if err != nil {
		t.Fatal(err)
	}
	cells := map[string][]byte{}
	for _, it := range r["info"] {
		cells[it.Column] = it.Value
	}
	if string(cells["info:name"]) != "seven" || len(cells["info:id"]) != 8 || binary.BigEndian.Uint64(cells["info:id"]) != 7 {
		t.Errorf("bigtable row 7: %v", cells)
	}

	sc, err := spanner.NewClient(ctx, "projects/"+h.Project()+"/instances/"+inst+"/databases/"+db, spannerOpts(spAddr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	var got []string
	eventually1874(t, "spanner rows", 60*time.Second, func() bool {
		got = nil
		it := sc.Single().Query(ctx, spanner.Statement{SQL: "SELECT name FROM people ORDER BY id"})
		_ = it.Do(func(r *spanner.Row) error {
			var n spanner.NullString
			if err := r.Columns(&n); err != nil {
				return err
			}
			got = append(got, n.StringVal)
			return nil
		})
		return len(got) >= 2
	})
	sort.Strings(got)
	if strings.Join(got, ",") != "eight,seven" {
		t.Errorf("spanner rows %v", got)
	}
	cancelContinuous(t, h, btJob)
	cancelContinuous(t, h, spJob)
}

// runDML runs q to its end.
func runDML(ctx context.Context, q *bigquery.Query) error {
	j, err := q.Run(ctx)
	if err != nil {
		return err
	}
	st, err := j.Wait(ctx)
	if err != nil {
		return err
	}
	return st.Err()
}

// covers: bigqueryreservation.projects.locations.reservationGroups.create, bigqueryreservation.projects.locations.reservationGroups.get, bigqueryreservation.projects.locations.reservationGroups.list, bigqueryreservation.projects.locations.reservationGroups.patch, bigqueryreservation.projects.locations.reservationGroups.delete, bigqueryreservation.projects.locations.reservations.failoverReservation, bigqueryreservation.projects.locations.reservations.getIamPolicy, bigqueryreservation.projects.locations.reservations.setIamPolicy, bigqueryreservation.projects.locations.reservations.testIamPermissions, bigqueryreservation.projects.locations.reservations.assignments.getIamPolicy, bigqueryreservation.projects.locations.reservations.assignments.setIamPolicy, bigqueryreservation.projects.locations.reservations.assignments.testIamPermissions, bigqueryreservation.projects.locations.upgradeEdition
//
// TestBigQueryReservationGroupsFailoverIAMAndUpgrade (#2193): reservation
// groups (a group with a member is not deleted), an Enterprise Plus
// reservation's secondary location and failoverReservation (a secondary
// location on an Enterprise reservation refused), the IAM policies of a
// reservation and an assignment, through the REST and the gRPC client; and
// upgradeEdition through Google's discovery client
// (google.golang.org/api/bigqueryreservation/v1), which the Go gapic
// client does not have: a reservation upgraded alone while its commitment
// stays is refused for raising the flexible slots, and the two together
// are upgraded.
func TestBigQueryReservationGroupsFailoverIAMAndUpgrade(t *testing.T) {
	h := New(t)
	rest, gc, project := reservationClients(t, h)
	ctx := h.Context()
	suffix := strings.ToLower(strings.ReplaceAll(h.Project(), "_", "-"))
	if len(suffix) > 40 {
		suffix = suffix[len(suffix)-40:]
	}
	parent := "projects/" + project + "/locations/us-central1"
	for _, c := range []struct {
		name string
		cl   *reservation.Client
	}{{"rest", rest}, {"grpc", gc}} {
		t.Run(c.name, func(t *testing.T) {
			cl := c.cl
			gid := "g" + c.name + "-" + suffix
			g, err := cl.CreateReservationGroup(ctx, &reservationpb.CreateReservationGroupRequest{Parent: parent, ReservationGroupId: gid,
				ReservationGroup: &reservationpb.ReservationGroup{}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = cl.DeleteReservationGroup(h.Context(), &reservationpb.DeleteReservationGroupRequest{Name: g.GetName()})
			})
			if got, err := cl.GetReservationGroup(ctx, &reservationpb.GetReservationGroupRequest{Name: g.GetName()}); err != nil || got.GetName() != parent+"/reservationGroups/"+gid {
				t.Errorf("get group: %v %v", got, err)
			}
			git := cl.ListReservationGroups(ctx, &reservationpb.ListReservationGroupsRequest{Parent: parent})
			found := false
			for x, err := git.Next(); err == nil; x, err = git.Next() {
				found = found || x.GetName() == g.GetName()
			}
			if !found {
				t.Error("the group is not listed")
			}
			if _, err := cl.UpdateReservationGroup(ctx, &reservationpb.UpdateReservationGroupRequest{ReservationGroup: g}); err != nil {
				t.Errorf("patch group: %v", err)
			}

			id := "f" + c.name + "-" + suffix
			if _, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: id + "-e",
				Reservation: &reservationpb.Reservation{SlotCapacity: 50, Edition: reservationpb.Edition_ENTERPRISE, SecondaryLocation: "us-west1"}}); resvCode(err) != codes.InvalidArgument {
				t.Errorf("a secondary location on an Enterprise reservation: %v, want INVALID_ARGUMENT", err)
			}
			rv, err := cl.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: id,
				Reservation: &reservationpb.Reservation{SlotCapacity: 50, Edition: reservationpb.Edition_ENTERPRISE_PLUS,
					SecondaryLocation: "us-west1", ReservationGroup: g.GetName()}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = cl.DeleteReservation(h.Context(), &reservationpb.DeleteReservationRequest{Name: rv.GetName()})
			})
			if err := cl.DeleteReservationGroup(ctx, &reservationpb.DeleteReservationGroupRequest{Name: g.GetName()}); resvCode(err) != codes.FailedPrecondition {
				t.Errorf("delete a group with a member: %v, want FAILED_PRECONDITION", err)
			}
			fo, err := cl.FailoverReservation(ctx, &reservationpb.FailoverReservationRequest{Name: rv.GetName()})
			if err != nil || fo.GetPrimaryLocation() != "us-west1" || fo.GetSecondaryLocation() != "us-central1" {
				t.Errorf("failover: %v %v", fo, err)
			}

			a, err := cl.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: rv.GetName(),
				Assignment: &reservationpb.Assignment{Assignee: "projects/" + project, JobType: reservationpb.Assignment_QUERY}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = cl.DeleteAssignment(h.Context(), &reservationpb.DeleteAssignmentRequest{Name: a.GetName()})
			})
			for _, resource := range []string{rv.GetName(), a.GetName()} {
				pol, err := cl.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
				if err != nil {
					t.Fatalf("getIamPolicy %s: %v", resource, err)
				}
				pol.Bindings = []*iampb.Binding{{Role: "roles/bigquery.resourceViewer", Members: []string{"user:viewer@example.com"}}}
				set, err := cl.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol})
				if err != nil || len(set.GetBindings()) != 1 || len(set.GetEtag()) == 0 {
					t.Errorf("setIamPolicy %s: %v %v", resource, set, err)
				}
				if got, err := cl.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource}); err != nil || len(got.GetBindings()) != 1 ||
					got.GetBindings()[0].GetMembers()[0] != "user:viewer@example.com" {
					t.Errorf("getIamPolicy after set %s: %v %v", resource, got, err)
				}
				pol.Etag = []byte("stale")
				if _, err := cl.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol}); err == nil {
					t.Errorf("setIamPolicy %s with a stale etag succeeded", resource)
				}
				if tp, err := cl.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: resource,
					Permissions: []string{"bigquery.reservations.get"}}); err != nil || tp == nil {
					t.Errorf("testIamPermissions %s: %v %v", resource, tp, err)
				}
			}
		})
	}

	t.Run("upgradeEdition", func(t *testing.T) {
		h := h.For(t)
		endpoint := h.Endpoint(EnvBigQuery)
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			t.Fatalf("BigQuery endpoint %q: %v", endpoint, err)
		}
		svc, err := bqresv.NewService(ctx, option.WithEndpoint(u.Scheme+"://"+u.Host+"/"), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		loc := "projects/" + project + "/locations/europe-west1"
		id := "u-" + suffix
		// An earlier run's FLEX commitments, deletable a minute after they
		// were made; any Enterprise one left covers this run's reservation.
		var leftEnterprise int64
		lit := rest.ListCapacityCommitments(ctx, &reservationpb.ListCapacityCommitmentsRequest{Parent: loc})
		for c, err := lit.Next(); err == nil; c, err = lit.Next() {
			if rest.DeleteCapacityCommitment(ctx, &reservationpb.DeleteCapacityCommitmentRequest{Name: c.GetName()}) != nil &&
				c.GetEdition() == reservationpb.Edition_ENTERPRISE {
				leftEnterprise += c.GetSlotCount()
			}
		}
		rv, err := rest.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: loc, ReservationId: id,
			Reservation: &reservationpb.Reservation{SlotCapacity: 100, Edition: reservationpb.Edition_ENTERPRISE}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = rest.DeleteReservation(h.Context(), &reservationpb.DeleteReservationRequest{Name: rv.GetName()})
		})
		cm, err := rest.CreateCapacityCommitment(ctx, &reservationpb.CreateCapacityCommitmentRequest{Parent: loc,
			CapacityCommitment: &reservationpb.CapacityCommitment{SlotCount: 100, Plan: reservationpb.CapacityCommitment_FLEX,
				Edition: reservationpb.Edition_ENTERPRISE}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Projects.Locations.UpgradeEdition(loc, &bqresv.UpgradeEditionRequest{Reservations: []string{rv.GetName()},
			TargetEdition: "STANDARD"}).Context(ctx).Do(); resvCode(err) != codes.InvalidArgument {
			t.Errorf("to STANDARD: %v, want INVALID_ARGUMENT", err)
		}
		if leftEnterprise > 0 {
			t.Logf("%d Enterprise slots of an earlier run's commitments in %s cover the reservation: its upgrade alone is not checked", leftEnterprise, loc)
		} else if _, err := svc.Projects.Locations.UpgradeEdition(loc, &bqresv.UpgradeEditionRequest{Reservations: []string{rv.GetName()},
			TargetEdition: "ENTERPRISE_PLUS"}).Context(ctx).Do(); resvCode(err) != codes.FailedPrecondition {
			t.Errorf("the reservation alone, its commitment left: %v, want FAILED_PRECONDITION", err)
		}
		up, err := svc.Projects.Locations.UpgradeEdition(loc, &bqresv.UpgradeEditionRequest{Reservations: []string{rv.GetName()},
			CapacityCommitments: []string{cm.GetName()}, TargetEdition: "ENTERPRISE_PLUS"}).Context(ctx).Do()
		if err != nil || len(up.Reservations) != 1 || up.Reservations[0].Edition != "ENTERPRISE_PLUS" ||
			len(up.CapacityCommitments) != 1 || up.CapacityCommitments[0].Edition != "ENTERPRISE_PLUS" {
			t.Fatalf("upgrade both: %v %v", up, err)
		}
		if got, err := rest.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: rv.GetName()}); err != nil ||
			got.GetEdition() != reservationpb.Edition_ENTERPRISE_PLUS {
			t.Errorf("after upgradeEdition: %v %v", got, err)
		}
		if _, err := svc.Projects.Locations.UpgradeEdition(loc, &bqresv.UpgradeEditionRequest{Reservations: []string{rv.GetName()},
			CapacityCommitments: []string{cm.GetName()}, TargetEdition: "ENTERPRISE"}).Context(ctx).Do(); resvCode(err) != codes.FailedPrecondition {
			t.Errorf("a downgrade: %v, want FAILED_PRECONDITION", err)
		}
	})
}
