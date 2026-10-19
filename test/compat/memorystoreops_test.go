//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.location.Locations/ListLocations, google.cloud.location.Locations/GetLocation, google.cloud.redis.v1.CloudRedis/UpdateInstance, google.cloud.redis.v1.CloudRedis/RescheduleMaintenance
//
// TestMemorystoreLocationsOperationsMaintenance (#1386), with cloud.google.com/
// go/redis/apiv1: ListLocations lists us-central1 with its zones in
// LocationMetadata, and GetLocation reads it; ListOperations filters on
// done and metadata.verb (another field is INVALID_ARGUMENT);
// DeleteOperation forgets an operation (GetOperation is NOT_FOUND after);
// CancelOperation of a done one leaves it as it is. UpdateInstance stores
// a maintenance_policy, read back with its one-hour window, and turns AUTH
// off and on, which the server's requirepass follows; RescheduleMaintenance
// is FAILED_PRECONDITION, since no maintenance is scheduled.
func TestMemorystoreLocationsOperationsMaintenance(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"

	var found bool
	it := c.ListLocations(ctx, &locationpb.ListLocationsRequest{Name: "projects/" + project})
	for {
		l, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLocations: %v", err)
		}
		found = found || l.GetLocationId() == "us-central1"
	}
	if !found {
		t.Error("ListLocations does not list us-central1")
	}
	loc, err := c.GetLocation(ctx, &locationpb.GetLocationRequest{Name: parent})
	md := &redispb.LocationMetadata{}
	if err != nil || loc.GetMetadata().UnmarshalTo(md) != nil || md.GetAvailableZones()["us-central1-a"] == nil {
		t.Errorf("GetLocation = %v, %v", loc, err)
	}

	inst, rc := createBasic(t, h, c, parent, fmt.Sprintf("ops-%d", time.Now().UnixNano()%1e8), nil)
	var createOp string
	ops := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: parent, Filter: `done = true AND metadata.verb = "create"`})
	for {
		op, err := ops.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListOperations: %v", err)
		}
		m := &redispb.OperationMetadata{}
		if op.GetMetadata().UnmarshalTo(m) != nil || m.GetVerb() != "create" || !op.GetDone() {
			t.Errorf("the filter let through %v", op)
		}
		if m.GetTarget() == inst.GetName() {
			createOp = op.GetName()
		}
	}
	if createOp == "" {
		t.Fatal("ListOperations with the filter does not list the create")
	}
	_, err = c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: parent, Filter: "metadata.createTime > 1"}).Next()
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("ListOperations of an unknown field = %v, want INVALID_ARGUMENT", err)
	}
	if err := c.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: createOp}); err != nil {
		t.Errorf("CancelOperation of a done operation: %v", err)
	}
	if err := c.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: createOp}); err != nil {
		t.Fatalf("DeleteOperation: %v", err)
	}
	if _, err := c.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: createOp}); status.Code(err) != codes.NotFound {
		t.Errorf("GetOperation after DeleteOperation = %v, want NOT_FOUND", err)
	}

	op, err := c.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"maintenance_policy", "auth_enabled"}},
		Instance: &redispb.Instance{Name: inst.GetName(), AuthEnabled: false, MaintenancePolicy: &redispb.MaintenancePolicy{Description: "weekly",
			WeeklyMaintenanceWindow: []*redispb.WeeklyMaintenanceWindow{{Day: dayofweek.DayOfWeek_TUESDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	got, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	w := got.GetMaintenancePolicy().GetWeeklyMaintenanceWindow()
	if got.GetAuthEnabled() || len(w) != 1 || w[0].GetDay() != dayofweek.DayOfWeek_TUESDAY || w[0].GetStartTime().GetHours() != 3 ||
		w[0].GetDuration().AsDuration() != time.Hour || got.GetMaintenancePolicy().GetCreateTime() == nil {
		t.Errorf("after the update: auth %v, maintenance_policy %v", got.GetAuthEnabled(), got.GetMaintenancePolicy())
	}
	anon := redis.NewClient(&redis.Options{Addr: rc.Options().Addr})
	defer anon.Close()
	if err := anon.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Errorf("SET without AUTH after auth_enabled false: %v", err)
	}
	op, err = c.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"auth_enabled"}},
		Instance: &redispb.Instance{Name: inst.GetName(), AuthEnabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatal(err)
	}
	// A new connection: one made before keeps the access it had.
	anon2 := redis.NewClient(&redis.Options{Addr: rc.Options().Addr})
	defer anon2.Close()
	if err := anon2.Set(ctx, "k", "v", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Errorf("SET without AUTH after auth_enabled true = %v, want NOAUTH", err)
	}
	if auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: inst.GetName()}); err != nil || auth.GetAuthString() == "" {
		t.Errorf("GetInstanceAuthString = %v, %v", auth, err)
	}

	_, err = c.RescheduleMaintenance(ctx, &redispb.RescheduleMaintenanceRequest{Name: inst.GetName(), RescheduleType: redispb.RescheduleMaintenanceRequest_IMMEDIATE})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("RescheduleMaintenance = %v, want FAILED_PRECONDITION", err)
	}
}
