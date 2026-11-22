package analyticshub

import (
	"context"
	"path/filepath"
	"testing"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func dcrListing(dataset, resource string) *ahpb.Listing {
	sel := &ahpb.Listing_BigQueryDatasetSource_SelectedResource{Resource: &ahpb.Listing_BigQueryDatasetSource_SelectedResource_Table{Table: resource}}
	if filepath.Base(filepath.Dir(resource)) == "routines" {
		sel = &ahpb.Listing_BigQueryDatasetSource_SelectedResource{Resource: &ahpb.Listing_BigQueryDatasetSource_SelectedResource_Routine{Routine: resource}}
	}
	return &ahpb.Listing{DisplayName: "Shared", Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{
		Dataset: dataset, SelectedResources: []*ahpb.Listing_BigQueryDatasetSource_SelectedResource{sel}}}}
}

func TestCleanRoomListingsAndSubscription(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "ah.json")
	s, link, _ := setup(t, state)
	room := "projects/pub/locations/us/dataExchanges/room"
	ex, err := s.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/pub/locations/us", DataExchangeId: "room",
		DataExchange: &ahpb.DataExchange{DisplayName: "Room", SharingEnvironmentConfig: &ahpb.SharingEnvironmentConfig{
			Environment: &ahpb.SharingEnvironmentConfig_DcrExchangeConfig_{DcrExchangeConfig: &ahpb.SharingEnvironmentConfig_DcrExchangeConfig{}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !ex.GetSharingEnvironmentConfig().GetDcrExchangeConfig().GetSingleLinkedDatasetPerCleanroom() {
		t.Errorf("clean room output fields: %v", ex.GetSharingEnvironmentConfig())
	}
	// A whole dataset is refused, and two resources in one listing.
	whole := dcrListing("projects/pub/datasets/shared", "projects/pub/datasets/shared/tables/v")
	whole.GetBigqueryDataset().SelectedResources = nil
	if _, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: room, ListingId: "whole", Listing: whole}); code(err) != codes.InvalidArgument {
		t.Errorf("whole dataset: %v", err)
	}
	l, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: room, ListingId: "v", Listing: dcrListing("projects/pub/datasets/shared", "projects/pub/datasets/shared/tables/v")})
	if err != nil {
		t.Fatal(err)
	}
	if c := l.GetRestrictedExportConfig(); !c.GetEnabled() || !c.GetRestrictDirectTableAccess() {
		t.Errorf("egress is not automatic: %v", c)
	}
	if _, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: room, ListingId: "v2", Listing: dcrListing("projects/pub/datasets/shared", "projects/pub/datasets/shared/tables/v")}); code(err) != codes.AlreadyExists {
		t.Errorf("a second resource named v: %v", err)
	}
	// Egress can't change.
	if _, err := s.UpdateListing(ctx, &ahpb.UpdateListingRequest{Listing: &ahpb.Listing{Name: l.GetName(),
		RestrictedExportConfig: &ahpb.Listing_RestrictedExportConfig{Enabled: true, RestrictQueryResult: true}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"restricted_export_config"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("egress update: %v", err)
	}
	if _, err := s.SubscribeListing(ctx, &ahpb.SubscribeListingRequest{Name: l.GetName(), Destination: &ahpb.SubscribeListingRequest_DestinationDataset{
		DestinationDataset: &ahpb.DestinationDataset{Location: "us", DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: "sub", DatasetId: "x"}}}}); code(err) != codes.FailedPrecondition {
		t.Errorf("listing subscription in a clean room: %v", err)
	}
	if _, err := s.SubscribeDataExchange(ctx, &ahpb.SubscribeDataExchangeRequest{Name: "projects/pub/locations/us/dataExchanges/ex1",
		Destination: "projects/sub/locations/us", Subscription: "s0"}); code(err) != codes.FailedPrecondition {
		t.Errorf("subscribe to a default exchange: %v", err)
	}
	op, err := s.SubscribeDataExchange(ctx, &ahpb.SubscribeDataExchangeRequest{Name: room, Destination: "projects/sub/locations/US", Subscription: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	var resp ahpb.SubscribeDataExchangeResponse
	if err := op.GetResponse().UnmarshalTo(&resp); err != nil || !op.GetDone() {
		t.Fatal(err)
	}
	sub := resp.GetSubscription()
	if sub.GetName() != "projects/sub/locations/us/subscriptions/s1" || sub.GetDataExchange() != room || len(sub.GetLinkedResources()) != 1 {
		t.Errorf("subscription: %v", sub)
	}
	got := link.linked["sub.room"]
	if !got.CleanRoom || len(got.Resources) != 1 || got.Resources[0].Table != "v" || !got.Restricted {
		t.Errorf("linked dataset: %+v", got)
	}
	// A listing added later is in the linked dataset; one deleted leaves it.
	if _, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: room, ListingId: "tvf", Listing: dcrListing("projects/pub/datasets/shared", "projects/pub/datasets/shared/routines/tvf")}); err != nil {
		t.Fatal(err)
	}
	if got := link.linked["sub.room"]; len(got.Resources) != 2 || got.Resources[0].Routine != "tvf" {
		t.Errorf("after a routine listing: %+v", got.Resources)
	}
	if _, err := s.DeleteListing(ctx, &ahpb.DeleteListingRequest{Name: l.GetName()}); err != nil {
		t.Fatal(err)
	}
	if got, ok := link.linked["sub.room"]; !ok || len(got.Resources) != 1 {
		t.Errorf("after deleting a listing: %+v", got)
	}
	shared, err := s.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: room})
	if err != nil || len(shared.GetSharedResourceSubscriptions()) != 1 {
		t.Errorf("shared subscriptions: %v %v", shared, err)
	}
	// It is kept across a restart.
	s2, err := NewServer(state, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g, err := s2.GetSubscription(ctx, &ahpb.GetSubscriptionRequest{Name: sub.GetName()}); err != nil || g.GetDataExchange() != room {
		t.Errorf("after a restart: %v %v", g, err)
	}
}

func TestQueryTemplateLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _, _ := setup(t, "")
	room := "projects/pub/locations/us/dataExchanges/room"
	if _, err := s.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/pub/locations/us", DataExchangeId: "room",
		DataExchange: &ahpb.DataExchange{DisplayName: "Room", SharingEnvironmentConfig: &ahpb.SharingEnvironmentConfig{
			Environment: &ahpb.SharingEnvironmentConfig_DcrExchangeConfig_{DcrExchangeConfig: &ahpb.SharingEnvironmentConfig_DcrExchangeConfig{}}}}}); err != nil {
		t.Fatal(err)
	}
	tmpl := &ahpb.QueryTemplate{DisplayName: "By year", Routine: &ahpb.Routine{RoutineType: ahpb.Routine_TABLE_VALUED_FUNCTION,
		DefinitionBody: "by_year(y INT64) AS (SELECT COUNT(*) n FROM `pub.shared.t` WHERE year = y)"}}
	if _, err := s.CreateQueryTemplate(ctx, &ahpb.CreateQueryTemplateRequest{Parent: "projects/pub/locations/us/dataExchanges/ex1", QueryTemplateId: "q", QueryTemplate: tmpl}); code(err) != codes.FailedPrecondition {
		t.Errorf("template in a default exchange: %v", err)
	}
	q, err := s.CreateQueryTemplate(ctx, &ahpb.CreateQueryTemplateRequest{Parent: room, QueryTemplateId: "q", QueryTemplate: tmpl})
	if err != nil || q.GetState() != ahpb.QueryTemplate_DRAFTED {
		t.Fatalf("create: %v %v", q, err)
	}
	if _, err := s.UpdateQueryTemplate(ctx, &ahpb.UpdateQueryTemplateRequest{QueryTemplate: &ahpb.QueryTemplate{Name: q.GetName(), DisplayName: "Other"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("display name update: %v", err)
	}
	if u, err := s.UpdateQueryTemplate(ctx, &ahpb.UpdateQueryTemplateRequest{QueryTemplate: &ahpb.QueryTemplate{Name: q.GetName(), Description: "Rows per year"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); err != nil || u.GetDescription() != "Rows per year" || u.GetRoutine() == nil {
		t.Errorf("update: %v %v", u, err)
	}
	if _, err := s.ApproveQueryTemplate(ctx, &ahpb.ApproveQueryTemplateRequest{Name: q.GetName()}); code(err) != codes.FailedPrecondition {
		t.Errorf("approve a draft: %v", err)
	}
	if p, err := s.SubmitQueryTemplate(ctx, &ahpb.SubmitQueryTemplateRequest{Name: q.GetName()}); err != nil || p.GetState() != ahpb.QueryTemplate_PENDING {
		t.Fatalf("submit: %v %v", p, err)
	}
	if _, err := s.UpdateQueryTemplate(ctx, &ahpb.UpdateQueryTemplateRequest{QueryTemplate: &ahpb.QueryTemplate{Name: q.GetName(), Description: "x"}}); code(err) != codes.FailedPrecondition {
		t.Errorf("update a pending template: %v", err)
	}
	if _, err := s.DeleteQueryTemplate(ctx, &ahpb.DeleteQueryTemplateRequest{Name: q.GetName()}); code(err) != codes.FailedPrecondition {
		t.Errorf("delete a pending template: %v", err)
	}
	if a, err := s.ApproveQueryTemplate(ctx, &ahpb.ApproveQueryTemplateRequest{Name: q.GetName()}); err != nil || a.GetState() != ahpb.QueryTemplate_APPROVED {
		t.Fatalf("approve: %v %v", a, err)
	}
	list, err := s.ListQueryTemplates(ctx, &ahpb.ListQueryTemplatesRequest{Parent: room})
	if err != nil || len(list.GetQueryTemplates()) != 1 {
		t.Errorf("list: %v %v", list, err)
	}
	if _, err := s.DeleteDataExchange(ctx, &ahpb.DeleteDataExchangeRequest{Name: room}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetQueryTemplate(ctx, &ahpb.GetQueryTemplateRequest{Name: q.GetName()}); code(err) != codes.NotFound {
		t.Errorf("template after the clean room's deletion: %v", err)
	}
}
