package analyticshub

import (
	"context"
	"path/filepath"
	"testing"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

type fakeLinker struct {
	locations map[string]string
	linked    map[string]LinkedDataset
}

func (f *fakeLinker) DatasetLocation(_ context.Context, project, dataset string) (string, error) {
	if l, ok := f.locations[project+"."+dataset]; ok {
		return l, nil
	}
	return "", apierror.NotFound("dataset %s.%s not found", project, dataset)
}

func (f *fakeLinker) CreateLinked(_ context.Context, l LinkedDataset) error {
	if _, ok := f.linked[l.Project+"."+l.Dataset]; ok {
		return apierror.AlreadyExists("Already Exists: Dataset %s:%s", l.Project, l.Dataset)
	}
	f.linked[l.Project+"."+l.Dataset] = l
	return nil
}

func (f *fakeLinker) UpdateLinked(_ context.Context, l LinkedDataset) error {
	f.linked[l.Project+"."+l.Dataset] = l
	return nil
}

func (f *fakeLinker) DeleteLinked(_ context.Context, project, dataset string) error {
	delete(f.linked, project+"."+dataset)
	return nil
}

func code(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	return apierror.From(err).Code
}

func setup(t *testing.T, state string) (*Server, *fakeLinker, *ahpb.Listing) {
	t.Helper()
	link := &fakeLinker{locations: map[string]string{"pub.shared": "US", "pub.eu": "EU"}, linked: map[string]LinkedDataset{}}
	s, err := NewServer(state, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/pub/locations/us", DataExchangeId: "ex1",
		DataExchange: &ahpb.DataExchange{DisplayName: "My exchange"}}); err != nil {
		t.Fatal(err)
	}
	l, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: "projects/pub/locations/us/dataExchanges/ex1", ListingId: "l1",
		Listing: &ahpb.Listing{DisplayName: "L one", Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{Dataset: "projects/pub/datasets/shared"}},
			RestrictedExportConfig: &ahpb.Listing_RestrictedExportConfig{Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	return s, link, l
}

func TestSubscribeRevokeDelete(t *testing.T) {
	ctx := context.Background()
	s, link, l := setup(t, filepath.Join(t.TempDir(), "ah.json"))
	if !l.GetRestrictedExportConfig().GetRestrictDirectTableAccess() || l.GetState() != ahpb.Listing_ACTIVE {
		t.Errorf("listing output fields: %v", l)
	}
	resp, err := s.SubscribeListing(ctx, &ahpb.SubscribeListingRequest{Name: l.GetName(), Destination: &ahpb.SubscribeListingRequest_DestinationDataset{
		DestinationDataset: &ahpb.DestinationDataset{Location: "US", DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: "sub", DatasetId: "linked"}}}})
	if err != nil {
		t.Fatal(err)
	}
	got := link.linked["sub.linked"]
	if got.SourceProject != "pub" || got.SourceDataset != "shared" || !got.Restricted {
		t.Errorf("linked dataset = %+v", got)
	}
	sub := resp.GetSubscription()
	if sub.GetState() != ahpb.Subscription_STATE_ACTIVE || sub.GetLinkedDatasetMap()[l.GetName()].GetLinkedDataset() != "projects/sub/datasets/linked" {
		t.Errorf("subscription = %v", sub)
	}
	// The same destination again: BigQuery's ALREADY_EXISTS.
	_, err = s.SubscribeListing(ctx, &ahpb.SubscribeListingRequest{Name: l.GetName(), Destination: &ahpb.SubscribeListingRequest_DestinationDataset{
		DestinationDataset: &ahpb.DestinationDataset{Location: "US", DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: "sub", DatasetId: "linked"}}}})
	if code(err) != codes.AlreadyExists {
		t.Errorf("second subscribe: %v", err)
	}
	list, err := s.ListSubscriptions(ctx, &ahpb.ListSubscriptionsRequest{Parent: "projects/sub/locations/us", Filter: `listing="` + l.GetName() + `"`})
	if err != nil || len(list.GetSubscriptions()) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	shared, err := s.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: "projects/pub/locations/us/dataExchanges/ex1"})
	if err != nil || len(shared.GetSharedResourceSubscriptions()) != 1 {
		t.Fatalf("shared = %v, %v", shared, err)
	}
	// A restart reads everything back.
	s2, err := NewServer(s.state, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetSubscription(ctx, &ahpb.GetSubscriptionRequest{Name: sub.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeSubscription(ctx, &ahpb.RevokeSubscriptionRequest{Name: sub.GetName()}); err != nil {
		t.Fatal(err)
	}
	if !link.linked["sub.linked"].Revoked {
		t.Error("revoke left the linked dataset readable")
	}
	if _, err := s.RevokeSubscription(ctx, &ahpb.RevokeSubscriptionRequest{Name: sub.GetName()}); code(err) != codes.FailedPrecondition {
		t.Errorf("second revoke: %v", err)
	}
	if _, err := s.RefreshSubscription(ctx, &ahpb.RefreshSubscriptionRequest{Name: sub.GetName()}); code(err) != codes.FailedPrecondition {
		t.Errorf("refresh of a revoked subscription: %v", err)
	}
	op, err := s.DeleteSubscription(ctx, &ahpb.DeleteSubscriptionRequest{Name: sub.GetName()})
	if err != nil || !op.GetDone() {
		t.Fatalf("delete = %v, %v", op, err)
	}
	if _, ok := link.linked["sub.linked"]; ok {
		t.Error("the linked dataset outlived its subscription")
	}
	if _, err := s.GetSubscription(ctx, &ahpb.GetSubscriptionRequest{Name: sub.GetName()}); code(err) != codes.NotFound {
		t.Errorf("get after delete: %v", err)
	}
	shared, _ = s.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: l.GetName(), IncludeDeletedSubscriptions: true})
	if len(shared.GetSharedResourceSubscriptions()) != 1 {
		t.Errorf("include_deleted_subscriptions listed %d", len(shared.GetSharedResourceSubscriptions()))
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	s, _, l := setup(t, "")
	for _, tc := range []struct {
		name string
		err  error
		want codes.Code
	}{
		{"bad exchange id", func() error {
			_, err := s.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/pub/locations/us", DataExchangeId: "a-b", DataExchange: &ahpb.DataExchange{DisplayName: "x"}})
			return err
		}(), codes.InvalidArgument},
		{"duplicate exchange", func() error {
			_, err := s.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/pub/locations/us", DataExchangeId: "ex1", DataExchange: &ahpb.DataExchange{DisplayName: "x"}})
			return err
		}(), codes.AlreadyExists},
		{"display name with a leading space", func() error {
			_, err := s.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/pub/locations/us", DataExchangeId: "ex2", DataExchange: &ahpb.DataExchange{DisplayName: " x"}})
			return err
		}(), codes.InvalidArgument},
		{"unknown location", func() error {
			_, err := s.CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/pub/locations/mars", DataExchangeId: "ex2", DataExchange: &ahpb.DataExchange{DisplayName: "x"}})
			return err
		}(), codes.InvalidArgument},
		{"missing shared dataset", func() error {
			_, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: "projects/pub/locations/us/dataExchanges/ex1", ListingId: "l2",
				Listing: &ahpb.Listing{DisplayName: "x", Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{Dataset: "projects/pub/datasets/nope"}}}})
			return err
		}(), codes.NotFound},
		{"dataset in another location", func() error {
			_, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: "projects/pub/locations/us/dataExchanges/ex1", ListingId: "l2",
				Listing: &ahpb.Listing{DisplayName: "x", Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{Dataset: "projects/pub/datasets/eu"}}}})
			return err
		}(), codes.InvalidArgument},
		{"pubsub listing", func() error {
			_, err := s.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: "projects/pub/locations/us/dataExchanges/ex1", ListingId: "l2",
				Listing: &ahpb.Listing{DisplayName: "x", Source: &ahpb.Listing_PubsubTopic{PubsubTopic: &ahpb.Listing_PubSubTopicSource{Topic: "projects/pub/topics/t"}}}})
			return err
		}(), codes.Unimplemented},
		{"change the shared dataset", func() error {
			in := &ahpb.Listing{Name: l.GetName(), Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{Dataset: "projects/pub/datasets/eu"}}}
			_, err := s.UpdateListing(ctx, &ahpb.UpdateListingRequest{Listing: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bigquery_dataset"}}})
			return err
		}(), codes.InvalidArgument},
		{"no update mask", func() error {
			_, err := s.UpdateListing(ctx, &ahpb.UpdateListingRequest{Listing: &ahpb.Listing{Name: l.GetName()}})
			return err
		}(), codes.InvalidArgument},
		{"subscribe in another location", func() error {
			_, err := s.SubscribeListing(ctx, &ahpb.SubscribeListingRequest{Name: l.GetName(), Destination: &ahpb.SubscribeListingRequest_DestinationDataset{
				DestinationDataset: &ahpb.DestinationDataset{Location: "EU", DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: "sub", DatasetId: "x"}}}})
			return err
		}(), codes.InvalidArgument},
		{"subscribe to a missing listing", func() error {
			_, err := s.SubscribeListing(ctx, &ahpb.SubscribeListingRequest{Name: l.GetName() + "x", Destination: &ahpb.SubscribeListingRequest_DestinationDataset{
				DestinationDataset: &ahpb.DestinationDataset{Location: "US", DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: "sub", DatasetId: "x"}}}})
			return err
		}(), codes.NotFound},
	} {
		if got := code(tc.err); got != tc.want {
			t.Errorf("%s: %v (%v), want %v", tc.name, got, tc.err, tc.want)
		}
	}
}

func TestExchangeDiscoveryTypeOverwritesListingsAndDeleteRevokes(t *testing.T) {
	ctx := context.Background()
	s, link, l := setup(t, "")
	resp, err := s.SubscribeListing(ctx, &ahpb.SubscribeListingRequest{Name: l.GetName(), Destination: &ahpb.SubscribeListingRequest_DestinationDataset{
		DestinationDataset: &ahpb.DestinationDataset{Location: "us", DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: "sub", DatasetId: "linked"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateDataExchange(ctx, &ahpb.UpdateDataExchangeRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"discovery_type"}},
		DataExchange: &ahpb.DataExchange{Name: "projects/pub/locations/us/dataExchanges/ex1", DiscoveryType: ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC.Enum()}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetListing(ctx, &ahpb.GetListingRequest{Name: l.GetName()})
	if got.GetDiscoveryType() != ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC {
		t.Errorf("listing discovery_type = %v", got.GetDiscoveryType())
	}
	if _, err := s.DeleteDataExchange(ctx, &ahpb.DeleteDataExchangeRequest{Name: "projects/pub/locations/us/dataExchanges/ex1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetListing(ctx, &ahpb.GetListingRequest{Name: l.GetName()}); code(err) != codes.NotFound {
		t.Errorf("listing outlived its exchange: %v", err)
	}
	sub, err := s.GetSubscription(ctx, &ahpb.GetSubscriptionRequest{Name: resp.GetSubscription().GetName()})
	if err != nil || sub.GetState() != ahpb.Subscription_STATE_INACTIVE || !link.linked["sub.linked"].Revoked {
		t.Errorf("after the exchange's deletion: %v, %v, %+v", sub, err, link.linked["sub.linked"])
	}
}
