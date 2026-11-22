package analyticshub

import (
	"context"
	"regexp"
	"strings"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// knownLocation reports whether id is a BigQuery location: a
// multi-region or a Google Cloud region. Analytics Hub resources live in
// BigQuery's locations.
func knownLocation(id string) bool {
	if id == "us" || id == "eu" {
		return true
	}
	for _, r := range locations.Regions {
		if r == id {
			return true
		}
	}
	return false
}

func checkParent(parent string) error {
	m := locationRE.FindStringSubmatch(parent)
	if m == nil {
		return apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", parent)
	}
	if !knownLocation(m[2]) {
		return apierror.InvalidArgument("location %q is not a BigQuery location", m[2])
	}
	return nil
}

func page[T proto.Message](scope string, items []T, token string, size int32) ([]T, string, error) {
	out, next, err := aipfilter.Page(scope, items, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	return out, next, nil
}

// ---- data exchanges ----

func (s *Server) exchange(name string) (*exchangeRecord, error) {
	if !exchangeRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/dataExchanges/{data_exchange}", name)
	}
	e, ok := s.exchanges[name]
	if !ok {
		return nil, apierror.NotFound("DataExchange %s not found", name)
	}
	return e, nil
}

// exchangeOut is an exchange with its listing_count.
func (s *Server) exchangeOut(e *exchangeRecord) *ahpb.DataExchange {
	out := clone(e.Exchange)
	out.ListingCount = int32(len(sortedKeys(s.listings, out.GetName()+"/listings/")))
	return out
}

func checkExchange(e *ahpb.DataExchange) error {
	if err := checkDisplayName(e.GetDisplayName()); err != nil {
		return err
	}
	if err := checkText("description", e.GetDescription(), 2000); err != nil {
		return err
	}
	if err := checkText("primary_contact", e.GetPrimaryContact(), 1000); err != nil {
		return err
	}
	return checkIcon(e.GetIcon())
}

func (s *Server) CreateDataExchange(_ context.Context, req *ahpb.CreateDataExchangeRequest) (*ahpb.DataExchange, error) {
	normalize(req)
	if err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	if err := checkID("data_exchange_id", req.GetDataExchangeId()); err != nil {
		return nil, err
	}
	e := req.GetDataExchange()
	if e == nil {
		return nil, apierror.InvalidArgument("data_exchange is required")
	}
	if err := checkExchange(e); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/dataExchanges/" + req.GetDataExchangeId()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.exchanges[name]; ok {
		return nil, apierror.AlreadyExists("DataExchange %s already exists", name)
	}
	e = clone(e)
	e.Name = name
	e.ListingCount = 0
	if e.SharingEnvironmentConfig == nil {
		e.SharingEnvironmentConfig = &ahpb.SharingEnvironmentConfig{Environment: &ahpb.SharingEnvironmentConfig_DefaultExchangeConfig_{
			DefaultExchangeConfig: &ahpb.SharingEnvironmentConfig_DefaultExchangeConfig{}}}
	}
	if dcr := e.GetSharingEnvironmentConfig().GetDcrExchangeConfig(); dcr != nil {
		// Output only, and true for "all new DCRs" (cleanrooms.go).
		dcr.SingleSelectedResourceSharingRestriction = proto.Bool(true)
		dcr.SingleLinkedDatasetPerCleanroom = proto.Bool(true)
	}
	if e.DiscoveryType == nil {
		e.DiscoveryType = ahpb.DiscoveryType_DISCOVERY_TYPE_PRIVATE.Enum()
	}
	rec := &exchangeRecord{Exchange: e}
	s.exchanges[name] = rec
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.exchangeOut(rec), nil
}

func (s *Server) GetDataExchange(_ context.Context, req *ahpb.GetDataExchangeRequest) (*ahpb.DataExchange, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.exchange(req.GetName())
	if err != nil {
		return nil, err
	}
	return s.exchangeOut(e), nil
}

func (s *Server) ListDataExchanges(_ context.Context, req *ahpb.ListDataExchangesRequest) (*ahpb.ListDataExchangesResponse, error) {
	normalize(req)
	if err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []*ahpb.DataExchange
	for _, k := range sortedKeys(s.exchanges, req.GetParent()+"/dataExchanges/") {
		all = append(all, s.exchangeOut(s.exchanges[k]))
	}
	out, next, err := page("ah-exchanges:"+req.GetParent(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &ahpb.ListDataExchangesResponse{DataExchanges: out, NextPageToken: next}, nil
}

func (s *Server) ListOrgDataExchanges(_ context.Context, req *ahpb.ListOrgDataExchangesRequest) (*ahpb.ListOrgDataExchangesResponse, error) {
	normalize(req)
	m := orgLocationRE.FindStringSubmatch(req.GetOrganization())
	if m == nil {
		return nil, apierror.InvalidArgument("organization %q must be organizations/{organization}/locations/{location}", req.GetOrganization())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []*ahpb.DataExchange
	for _, k := range sortedKeys(s.exchanges, "projects/") {
		em := exchangeRE.FindStringSubmatch(k)
		if em == nil || em[2] != m[2] || s.orgOf == nil || s.orgOf(em[1]) != "organizations/"+m[1] {
			continue
		}
		all = append(all, s.exchangeOut(s.exchanges[k]))
	}
	out, next, err := page("ah-org-exchanges:"+req.GetOrganization(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &ahpb.ListOrgDataExchangesResponse{DataExchanges: out, NextPageToken: next}, nil
}

// exchangeMask are the paths UpdateDataExchange takes, as the patch
// method documents them.
var exchangeMask = map[string]bool{"display_name": true, "description": true, "primary_contact": true, "documentation": true,
	"icon": true, "discovery_type": true, "log_linked_dataset_query_user_email": true}

func (s *Server) UpdateDataExchange(ctx context.Context, req *ahpb.UpdateDataExchangeRequest) (*ahpb.DataExchange, error) {
	normalize(req)
	in := req.GetDataExchange()
	if in == nil {
		return nil, apierror.InvalidArgument("data_exchange is required")
	}
	paths, err := maskPaths(req.GetUpdateMask(), exchangeMask)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.exchange(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(rec.Exchange)
	if next.GetLogLinkedDatasetQueryUserEmail() && hasPath(paths, "log_linked_dataset_query_user_email") && !in.GetLogLinkedDatasetQueryUserEmail() {
		return nil, apierror.InvalidArgument("log_linked_dataset_query_user_email cannot be turned off once it is enabled")
	}
	applyMask(next, in, paths)
	if err := checkExchange(next); err != nil {
		return nil, err
	}
	rec.Exchange = next
	if hasPath(paths, "discovery_type") {
		// "Updating this field also updates (overwrites) the
		// discovery_type field for all the listings under this exchange."
		for _, k := range sortedKeys(s.listings, next.GetName()+"/listings/") {
			s.listings[k].Listing.DiscoveryType = proto.Clone(next).(*ahpb.DataExchange).DiscoveryType
		}
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.exchangeOut(rec), nil
}

// DeleteDataExchange deletes the exchange and its listings: "When you
// delete a data exchange, all listings within the exchange are also
// deleted. Shared and linked datasets aren't deleted." Their subscriptions
// are revoked: the linked datasets stay in the subscribers' projects, and
// can no longer be queried.
func (s *Server) DeleteDataExchange(ctx context.Context, req *ahpb.DeleteDataExchangeRequest) (*emptypb.Empty, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.exchange(req.GetName()); err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(s.listings, req.GetName()+"/listings/") {
		for _, sub := range s.listingSubs(k, false) {
			if err := s.revokeLocked(ctx, sub); err != nil {
				return nil, err
			}
		}
		delete(s.listings, k)
	}
	// A clean room's subscriptions are unlinked too, so "queries on the
	// linked datasets start to fail"; its query templates go with it.
	for _, sub := range s.exchangeSubs(req.GetName()) {
		if err := s.revokeLocked(ctx, sub); err != nil {
			return nil, err
		}
	}
	for _, k := range sortedKeys(s.templates, req.GetName()+"/queryTemplates/") {
		delete(s.templates, k)
	}
	delete(s.exchanges, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// ---- listings ----

func (s *Server) listing(name string) (*listingRecord, error) {
	if !listingRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/dataExchanges/{data_exchange}/listings/{listing}", name)
	}
	l, ok := s.listings[name]
	if !ok {
		return nil, apierror.NotFound("Listing %s not found", name)
	}
	return l, nil
}

// listingOut is a listing with its output-only fields.
func listingOut(l *ahpb.Listing) *ahpb.Listing {
	out := clone(l)
	out.State = ahpb.Listing_ACTIVE
	out.ResourceType = ahpb.SharedResourceType_BIGQUERY_DATASET
	if c := out.GetRestrictedExportConfig(); c != nil {
		// Output only: "restrict direct table access (read
		// api/tabledata.list) on linked table", which restricted export
		// turns on.
		c.RestrictDirectTableAccess = c.GetEnabled()
		if src := out.GetBigqueryDataset(); src != nil {
			src.RestrictedExportPolicy = &ahpb.Listing_BigQueryDatasetSource_RestrictedExportPolicy{
				Enabled:                   wrapperspb.Bool(c.GetEnabled()),
				RestrictDirectTableAccess: wrapperspb.Bool(c.GetEnabled()),
				RestrictQueryResult:       wrapperspb.Bool(c.GetRestrictQueryResult()),
			}
		}
	}
	return out
}

// checkListing checks a listing's fields, and its shared dataset in
// BigQuery when ctx allows a call; location is the exchange's.
func (s *Server) checkListing(ctx context.Context, l *ahpb.Listing, location string, create, dcr bool) error {
	if err := checkDisplayName(l.GetDisplayName()); err != nil {
		return err
	}
	if err := checkText("description", l.GetDescription(), 2000); err != nil {
		return err
	}
	if err := checkText("primary_contact", l.GetPrimaryContact(), 1000); err != nil {
		return err
	}
	if err := checkText("request_access", l.GetRequestAccess(), 1000); err != nil {
		return err
	}
	if err := checkIcon(l.GetIcon()); err != nil {
		return err
	}
	if len(l.GetCategories()) > 5 {
		return apierror.InvalidArgument("a listing has at most five categories; %d were given", len(l.GetCategories()))
	}
	if l.GetPubsubTopic() != nil {
		return apierror.Unimplemented("Pub/Sub topic listings are not served by CloudBurrow; share a BigQuery dataset")
	}
	if l.GetCommercialInfo() != nil {
		return apierror.Unimplemented("Google Cloud Marketplace commercial listings are not served by CloudBurrow")
	}
	src := l.GetBigqueryDataset()
	if src == nil || src.GetDataset() == "" {
		return apierror.InvalidArgument("listing.bigquery_dataset.dataset is required")
	}
	m := datasetRE.FindStringSubmatch(src.GetDataset())
	if m == nil {
		return apierror.InvalidArgument("bigquery_dataset.dataset %q must be projects/{project}/datasets/{dataset}", src.GetDataset())
	}
	for _, r := range src.GetSelectedResources() {
		tm := tableRE.FindStringSubmatch(r.GetTable())
		if r.GetRoutine() != "" {
			// A clean room shares a query template's table-valued
			// function as a routine listing (cleanrooms.go).
			rm := routineRE.FindStringSubmatch(r.GetRoutine())
			switch {
			case !dcr:
				return apierror.Unimplemented("sharing a routine (selected_resources.routine) outside a data clean room is not served by CloudBurrow")
			case rm == nil || rm[1] != m[1] || rm[2] != m[2]:
				return apierror.InvalidArgument("selected_resources.routine %q must be a routine of %s: projects/{project}/datasets/{dataset}/routines/{routine}", r.GetRoutine(), src.GetDataset())
			}
			continue
		}
		if tm == nil || tm[1] != m[1] || tm[2] != m[2] {
			return apierror.InvalidArgument("selected_resources.table %q must be a table of %s: projects/{project}/datasets/{dataset}/tables/{table}", r.GetTable(), src.GetDataset())
		}
	}
	if len(src.GetReplicaLocations()) > 0 {
		return apierror.Unimplemented("multi-region listings (bigquery_dataset.replica_locations) are not served by CloudBurrow")
	}
	if !create || s.link == nil {
		return nil
	}
	loc, err := s.link.DatasetLocation(ctx, m[1], m[2])
	if err != nil {
		return err
	}
	if !strings.EqualFold(loc, location) {
		return apierror.InvalidArgument("The shared dataset %s is in %s; a listing must be in the shared dataset's location, and this data exchange is in %s", src.GetDataset(), loc, location)
	}
	return nil
}

func (s *Server) CreateListing(ctx context.Context, req *ahpb.CreateListingRequest) (*ahpb.Listing, error) {
	normalize(req)
	em := exchangeRE.FindStringSubmatch(req.GetParent())
	if em == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}/dataExchanges/{data_exchange}", req.GetParent())
	}
	if err := checkID("listing_id", req.GetListingId()); err != nil {
		return nil, err
	}
	l := req.GetListing()
	if l == nil {
		return nil, apierror.InvalidArgument("listing is required")
	}
	s.mu.Lock()
	ex0, ok := s.exchanges[req.GetParent()]
	s.mu.Unlock()
	if !ok {
		return nil, apierror.NotFound("DataExchange %s not found", req.GetParent())
	}
	dcr := isCleanRoom(ex0.Exchange)
	if err := s.checkListing(ctx, l, em[2], true, dcr); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/listings/" + req.GetListingId()
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok := s.exchanges[req.GetParent()]
	if !ok {
		return nil, apierror.NotFound("DataExchange %s not found", req.GetParent())
	}
	if _, ok := s.listings[name]; ok {
		return nil, apierror.AlreadyExists("Listing %s already exists", name)
	}
	l = clone(l)
	l.Name = name
	if l.DiscoveryType == nil {
		l.DiscoveryType = proto.Clone(ex.Exchange).(*ahpb.DataExchange).DiscoveryType
	}
	if dcr {
		if err := s.cleanRoomListing(l, req.GetParent()); err != nil {
			return nil, err
		}
	}
	rec := &listingRecord{Listing: l}
	s.listings[name] = rec
	if err := s.relinkExchange(ctx, req.GetParent()); err != nil {
		delete(s.listings, name)
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return listingOut(l), nil
}

func (s *Server) GetListing(_ context.Context, req *ahpb.GetListingRequest) (*ahpb.Listing, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	l, err := s.listing(req.GetName())
	if err != nil {
		return nil, err
	}
	return listingOut(l.Listing), nil
}

func (s *Server) ListListings(_ context.Context, req *ahpb.ListListingsRequest) (*ahpb.ListListingsResponse, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.exchange(req.GetParent()); err != nil {
		return nil, err
	}
	var all []*ahpb.Listing
	for _, k := range sortedKeys(s.listings, req.GetParent()+"/listings/") {
		all = append(all, listingOut(s.listings[k].Listing))
	}
	out, next, err := page("ah-listings:"+req.GetParent(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &ahpb.ListListingsResponse{Listings: out, NextPageToken: next}, nil
}

// listingMask are the paths UpdateListing takes: every field but the
// shared dataset ("You can modify all values except the listing's shared
// dataset").
var listingMask = map[string]bool{"display_name": true, "description": true, "primary_contact": true, "documentation": true,
	"icon": true, "data_provider": true, "categories": true, "publisher": true, "request_access": true,
	"restricted_export_config": true, "discovery_type": true, "log_linked_dataset_query_user_email": true,
	"allow_only_metadata_sharing": true, "stored_procedure_config": true, "bigquery_dataset": true,
	"bigquery_dataset.selected_resources": true}

func (s *Server) UpdateListing(ctx context.Context, req *ahpb.UpdateListingRequest) (*ahpb.Listing, error) {
	normalize(req)
	in := req.GetListing()
	if in == nil {
		return nil, apierror.InvalidArgument("listing is required")
	}
	paths, err := maskPaths(req.GetUpdateMask(), listingMask)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.listing(in.GetName())
	if err != nil {
		return nil, err
	}
	next := clone(rec.Listing)
	if hasPath(paths, "bigquery_dataset") && in.GetBigqueryDataset().GetDataset() != next.GetBigqueryDataset().GetDataset() {
		return nil, apierror.InvalidArgument("the shared dataset of a listing cannot be changed: bigquery_dataset.dataset is %s", next.GetBigqueryDataset().GetDataset())
	}
	if next.GetLogLinkedDatasetQueryUserEmail() && hasPath(paths, "log_linked_dataset_query_user_email") && !in.GetLogLinkedDatasetQueryUserEmail() {
		return nil, apierror.InvalidArgument("log_linked_dataset_query_user_email cannot be turned off once it is enabled")
	}
	exName := next.GetName()[:strings.Index(next.GetName(), "/listings/")]
	dcr := s.exchanges[exName] != nil && isCleanRoom(s.exchanges[exName].Exchange)
	if dcr {
		if err := cleanRoomUpdate(rec.Listing, in, paths); err != nil {
			return nil, err
		}
	}
	for _, p := range paths {
		if p == "bigquery_dataset.selected_resources" {
			if next.GetBigqueryDataset() != nil {
				next.GetBigqueryDataset().SelectedResources = in.GetBigqueryDataset().GetSelectedResources()
			}
			continue
		}
		if p == "bigquery_dataset" {
			if next.GetBigqueryDataset() != nil {
				next.GetBigqueryDataset().SelectedResources = in.GetBigqueryDataset().GetSelectedResources()
			}
			continue
		}
		applyMask(next, in, []string{p})
	}
	if next.GetBigqueryDataset() != nil {
		next.GetBigqueryDataset().RestrictedExportPolicy = nil
	}
	lm := listingRE.FindStringSubmatch(next.GetName())
	if err := s.checkListing(ctx, next, lm[2], false, dcr); err != nil {
		return nil, err
	}
	rec.Listing = next
	if err := s.relinkExchange(ctx, exName); err != nil {
		return nil, err
	}
	// "If set, restricted export configuration will be propagated and
	// enforced on the linked dataset", and so are the selected tables.
	for _, sub := range s.listingSubs(next.GetName(), false) {
		if sub.Sub.GetState() != ahpb.Subscription_STATE_ACTIVE {
			continue
		}
		sub.Linked = linkFor(next, sub.Linked)
		if s.link != nil {
			if err := s.link.UpdateLinked(ctx, sub.Linked); err != nil {
				return nil, err
			}
		}
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return listingOut(next), nil
}

// DeleteListing deletes the listing: "Deleting a listing also deletes all
// linked datasets and removes all subscriptions from subscriber projects."
func (s *Server) DeleteListing(ctx context.Context, req *ahpb.DeleteListingRequest) (*emptypb.Empty, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.listing(req.GetName()); err != nil {
		return nil, err
	}
	for _, sub := range s.listingSubs(req.GetName(), false) {
		if err := s.deleteSubLocked(ctx, sub); err != nil {
			return nil, err
		}
	}
	delete(s.listings, req.GetName())
	// A clean room's linked datasets lose the listing's resource: "the
	// linked datasets aren't deleted", and queries of it fail.
	if err := s.relinkExchange(ctx, req.GetName()[:strings.Index(req.GetName(), "/listings/")]); err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// ---- subscriptions ----

// linkFor is the linked dataset l makes, from what base names.
func linkFor(l *ahpb.Listing, base LinkedDataset) LinkedDataset {
	m := datasetRE.FindStringSubmatch(l.GetBigqueryDataset().GetDataset())
	base.SourceProject, base.SourceDataset = m[1], m[2]
	base.Tables = nil
	for _, r := range l.GetBigqueryDataset().GetSelectedResources() {
		if tm := tableRE.FindStringSubmatch(r.GetTable()); tm != nil {
			base.Tables = append(base.Tables, tm[3])
		}
	}
	c := l.GetRestrictedExportConfig()
	base.Restricted = c.GetEnabled()
	base.RestrictQueryResult = c.GetEnabled() && c.GetRestrictQueryResult()
	return base
}

// listingSubs are the subscriptions of a listing; the caller holds mu.
func (s *Server) listingSubs(listing string, deleted bool) []*subscriptionRecord {
	var out []*subscriptionRecord
	for _, k := range sortedKeys(s.subs, "") {
		r := s.subs[k]
		if r.Sub.GetListing() == listing && (deleted || r.Deleted.IsZero()) {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) SubscribeListing(ctx context.Context, req *ahpb.SubscribeListingRequest) (*ahpb.SubscribeListingResponse, error) {
	normalize(req)
	lm := listingRE.FindStringSubmatch(req.GetName())
	if lm == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/dataExchanges/{data_exchange}/listings/{listing}", req.GetName())
	}
	if req.GetDestinationPubsubSubscription() != nil {
		return nil, apierror.Unimplemented("Pub/Sub listings are not served by CloudBurrow, so destination_pubsub_subscription is not either")
	}
	dst := req.GetDestinationDataset()
	if dst == nil {
		return nil, apierror.InvalidArgument("destination_dataset is required")
	}
	ref := dst.GetDatasetReference()
	if ref.GetProjectId() == "" || ref.GetDatasetId() == "" {
		return nil, apierror.InvalidArgument("destination_dataset.dataset_reference.project_id and dataset_id are required")
	}
	if len(ref.GetDatasetId()) > 1024 || !datasetIDRE.MatchString(ref.GetDatasetId()) {
		return nil, apierror.InvalidArgument("destination_dataset.dataset_reference.dataset_id %q must contain only letters, numbers and underscores, at most 1024 characters", ref.GetDatasetId())
	}
	if dst.GetLocation() == "" {
		return nil, apierror.InvalidArgument("destination_dataset.location is required")
	}
	if len(dst.GetReplicaLocations()) > 0 {
		return nil, apierror.Unimplemented("linked dataset replicas (destination_dataset.replica_locations) are not served by CloudBurrow")
	}
	if !strings.EqualFold(dst.GetLocation(), lm[2]) {
		return nil, apierror.InvalidArgument("destination_dataset.location %s must be the listing's location, %s", dst.GetLocation(), lm[2])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.listing(req.GetName())
	if err != nil {
		return nil, err
	}
	if ex := s.exchanges[req.GetName()[:strings.Index(req.GetName(), "/listings/")]]; ex != nil && isCleanRoom(ex.Exchange) {
		return nil, apierror.FailedPrecondition("Listing %s is in a data clean room: subscribe to the clean room (SubscribeDataExchange), "+
			"whose one linked dataset holds every listing's resource", req.GetName())
	}
	if rec.Listing.GetAllowOnlyMetadataSharing() {
		return nil, apierror.FailedPrecondition("Listing %s shares only its metadata (allow_only_metadata_sharing) and cannot be subscribed to", req.GetName())
	}
	if s.link == nil {
		return nil, apierror.FailedPrecondition("BigQuery is not enabled on this instance, so no linked dataset can be made")
	}
	project := ref.GetProjectId()
	link := linkFor(rec.Listing, LinkedDataset{Project: project, Dataset: ref.GetDatasetId(), Location: dst.GetLocation(),
		FriendlyName: dst.GetFriendlyName().GetValue(), Description: dst.GetDescription().GetValue(), Labels: dst.GetLabels()})
	if err := s.link.CreateLinked(ctx, link); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.now())
	name := "projects/" + project + "/locations/" + lm[2] + "/subscriptions/" + randomHex(16)
	linked := "projects/" + project + "/datasets/" + ref.GetDatasetId()
	sub := &ahpb.Subscription{
		Name:                           name,
		ResourceName:                   &ahpb.Subscription_Listing{Listing: req.GetName()},
		CreationTime:                   now,
		LastModifyTime:                 now,
		State:                          ahpb.Subscription_STATE_ACTIVE,
		ResourceType:                   ahpb.SharedResourceType_BIGQUERY_DATASET,
		DestinationDataset:             clone(dst),
		LogLinkedDatasetQueryUserEmail: proto.Bool(rec.Listing.GetLogLinkedDatasetQueryUserEmail()),
	}
	setLinked(sub, req.GetName(), linked)
	s.subs[name] = &subscriptionRecord{Sub: sub, Linked: link}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &ahpb.SubscribeListingResponse{Subscription: clone(sub)}, nil
}

func setLinked(sub *ahpb.Subscription, listing, dataset string) {
	if dataset == "" {
		sub.LinkedDatasetMap, sub.LinkedResources = nil, nil
		return
	}
	lr := &ahpb.Subscription_LinkedResource{Listing: listing,
		Reference: &ahpb.Subscription_LinkedResource_LinkedDataset{LinkedDataset: dataset}}
	sub.LinkedDatasetMap = map[string]*ahpb.Subscription_LinkedResource{listing: lr}
	sub.LinkedResources = []*ahpb.Subscription_LinkedResource{clone(lr)}
}

func (s *Server) subscription(name string) (*subscriptionRecord, error) {
	if !subscriptionRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/subscriptions/{subscription}", name)
	}
	r, ok := s.subs[name]
	if !ok || !r.Deleted.IsZero() {
		return nil, apierror.NotFound("Subscription %s not found", name)
	}
	return r, nil
}

func (s *Server) GetSubscription(_ context.Context, req *ahpb.GetSubscriptionRequest) (*ahpb.Subscription, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.subscription(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(r.Sub), nil
}

// subscriptionFilterRE is ListSubscriptions' filter: listing="…",
// data_exchange="…" or a bare "…" matching either.
var subscriptionFilterRE = regexp.MustCompile(`^\s*(?:(listing|data_exchange)\s*=\s*)?"([^"]+)"\s*$`)

func (s *Server) ListSubscriptions(_ context.Context, req *ahpb.ListSubscriptionsRequest) (*ahpb.ListSubscriptionsResponse, error) {
	normalize(req)
	if err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	var field, value string
	if strings.TrimSpace(req.GetFilter()) != "" {
		m := subscriptionFilterRE.FindStringSubmatch(req.GetFilter())
		if m == nil {
			return nil, apierror.InvalidArgument("filter %q must be listing=\"…\", data_exchange=\"…\" or a quoted resource name", req.GetFilter())
		}
		field, value = m[1], m[2]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []*ahpb.Subscription
	for _, k := range sortedKeys(s.subs, req.GetParent()+"/subscriptions/") {
		r := s.subs[k]
		if !r.Deleted.IsZero() {
			continue
		}
		listing := r.Sub.GetListing()
		exchange := listing
		if i := strings.Index(listing, "/listings/"); i >= 0 {
			exchange = listing[:i]
		}
		if e := r.Sub.GetDataExchange(); e != "" {
			exchange = e
		}
		switch {
		case value == "":
		case field == "listing" && listing != value, field == "data_exchange" && exchange != value,
			field == "" && listing != value && exchange != value:
			continue
		}
		all = append(all, clone(r.Sub))
	}
	out, next, err := page("ah-subs:"+req.GetParent()+"|"+req.GetFilter(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &ahpb.ListSubscriptionsResponse{Subscriptions: out, NextPageToken: next}, nil
}

func (s *Server) ListSharedResourceSubscriptions(_ context.Context, req *ahpb.ListSharedResourceSubscriptionsRequest) (*ahpb.ListSharedResourceSubscriptionsResponse, error) {
	normalize(req)
	res := req.GetResource()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case listingRE.MatchString(res):
		if _, err := s.listing(res); err != nil {
			return nil, err
		}
	case exchangeRE.MatchString(res):
		if _, err := s.exchange(res); err != nil {
			return nil, err
		}
	default:
		return nil, apierror.InvalidArgument("resource %q must be a data exchange or a listing", res)
	}
	var all []*ahpb.Subscription
	for _, k := range sortedKeys(s.subs, "") {
		r := s.subs[k]
		l := r.Sub.GetListing()
		if e := r.Sub.GetDataExchange(); e != "" {
			// A clean room's subscription is the clean room's, and of
			// each of its listings.
			l = e
			if strings.HasPrefix(res, e+"/listings/") {
				l = res
			}
		}
		if (l != res && !strings.HasPrefix(l, res+"/listings/")) || (!r.Deleted.IsZero() && !req.GetIncludeDeletedSubscriptions()) {
			continue
		}
		all = append(all, clone(r.Sub))
	}
	out, next, err := page("ah-shared:"+res, all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &ahpb.ListSharedResourceSubscriptionsResponse{SharedResourceSubscriptions: out, NextPageToken: next}, nil
}

// revokeLocked makes a subscription INACTIVE: "the subscriber can no
// longer query the linked dataset", which "remains in the subscriber's
// project".
func (s *Server) revokeLocked(ctx context.Context, r *subscriptionRecord) error {
	if r.Sub.GetState() == ahpb.Subscription_STATE_INACTIVE {
		return nil
	}
	r.Linked.Revoked = true
	if s.link != nil {
		if err := s.link.UpdateLinked(ctx, r.Linked); err != nil && apierror.From(err).Code != codes.NotFound {
			return err
		}
	}
	r.Sub.State = ahpb.Subscription_STATE_INACTIVE
	r.Sub.LastModifyTime = timestamppb.New(s.now())
	setLinked(r.Sub, "", "")
	return nil
}

func (s *Server) RevokeSubscription(ctx context.Context, req *ahpb.RevokeSubscriptionRequest) (*ahpb.RevokeSubscriptionResponse, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.subscription(req.GetName())
	if err != nil {
		return nil, err
	}
	if r.Sub.GetState() == ahpb.Subscription_STATE_INACTIVE {
		return nil, apierror.FailedPrecondition("Subscription %s is already revoked", req.GetName())
	}
	if err := s.revokeLocked(ctx, r); err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &ahpb.RevokeSubscriptionResponse{}, nil
}

// deleteSubLocked deletes the linked dataset and marks the subscription
// deleted.
func (s *Server) deleteSubLocked(ctx context.Context, r *subscriptionRecord) error {
	if s.link != nil && r.Linked.Dataset != "" {
		if err := s.link.DeleteLinked(ctx, r.Linked.Project, r.Linked.Dataset); err != nil {
			return err
		}
	}
	r.Deleted = s.now()
	r.Sub.State = ahpb.Subscription_STATE_INACTIVE
	setLinked(r.Sub, "", "")
	return nil
}

// operation records a finished operation of verb on target.
func (s *Server) operation(target, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	m := subscriptionRE.FindStringSubmatch(target)
	now := timestamppb.New(s.now())
	md, err := anypb.New(&ahpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op := &longrunningpb.Operation{Name: "projects/" + m[1] + "/locations/" + m[2] + "/operations/" + randomHex(16), Done: true, Metadata: md,
		Result: &longrunningpb.Operation_Response{Response: r}}
	s.ops[op.Name] = op
	return clone(op), nil
}

// DeleteSubscription deletes it: "When you delete a subscription, the
// linked dataset is also deleted from your project."
func (s *Server) DeleteSubscription(ctx context.Context, req *ahpb.DeleteSubscriptionRequest) (*longrunningpb.Operation, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.subscription(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.deleteSubLocked(ctx, r); err != nil {
		return nil, err
	}
	op, err := s.operation(req.GetName(), "delete", &emptypb.Empty{})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}

// RefreshSubscription brings a subscription's linked dataset up to date
// with its listing. The front already keeps a linked dataset's tables in
// step with the shared dataset on every read, so this re-sends the
// listing's selected tables and restrictions.
func (s *Server) RefreshSubscription(ctx context.Context, req *ahpb.RefreshSubscriptionRequest) (*longrunningpb.Operation, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.subscription(req.GetName())
	if err != nil {
		return nil, err
	}
	if r.Sub.GetState() != ahpb.Subscription_STATE_ACTIVE {
		return nil, apierror.FailedPrecondition("Subscription %s is not active and cannot be refreshed", req.GetName())
	}
	if l, ok := s.listings[r.Sub.GetListing()]; ok {
		r.Linked = linkFor(l.Listing, r.Linked)
	}
	if e := r.Sub.GetDataExchange(); e != "" {
		r.Linked = s.exchangeLink(e, r.Linked)
		s.setExchangeLinked(r.Sub, e, "projects/"+r.Linked.Project+"/datasets/"+r.Linked.Dataset)
	}
	if s.link != nil {
		if err := s.link.UpdateLinked(ctx, r.Linked); err != nil {
			return nil, err
		}
	}
	r.Sub.LastModifyTime = timestamppb.New(s.now())
	op, err := s.operation(req.GetName(), "refresh", &ahpb.RefreshSubscriptionResponse{Subscription: clone(r.Sub)})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}

// ---- IAM ----

// policy is where resource's policy is kept: a data exchange's, a
// listing's or a subscription's. The caller holds mu.
func (s *Server) policy(resource string) (**iampolicy.Stored, error) {
	switch {
	case listingRE.MatchString(resource):
		l, err := s.listing(resource)
		if err != nil {
			return nil, err
		}
		return &l.Policy, nil
	case exchangeRE.MatchString(resource):
		e, err := s.exchange(resource)
		if err != nil {
			return nil, err
		}
		return &e.Policy, nil
	case subscriptionRE.MatchString(resource):
		r, err := s.subscription(resource)
		if err != nil {
			return nil, err
		}
		return &r.Policy, nil
	}
	return nil, apierror.InvalidArgument("resource %q must be a data exchange, a listing or a subscription", resource)
}

func (s *Server) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.policy(req.GetResource())
	if err != nil {
		return nil, err
	}
	out, err := iampolicy.Get(*p, req.GetOptions())
	return out, apierror.Wrap(err)
}

func (s *Server) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.policy(req.GetResource())
	if err != nil {
		return nil, err
	}
	next, err := iampolicy.SetConditional(*p, req)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	*p = &next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return next.Proto(), nil
}

func (s *Server) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.policy(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// ---- update masks ----

// maskPaths are a mask's paths, each in allowed; a missing or empty mask
// is refused, as the mask is required.
func maskPaths(mask *fieldmaskpb.FieldMask, allowed map[string]bool) ([]string, error) {
	if len(mask.GetPaths()) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	for _, p := range mask.GetPaths() {
		if !allowed[p] {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated", p)
		}
	}
	return mask.GetPaths(), nil
}

func hasPath(paths []string, p string) bool {
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}

// applyMask copies each top-level field paths names from in to dst.
func applyMask(dst, in proto.Message, paths []string) {
	d, src := dst.ProtoReflect(), in.ProtoReflect()
	fields := d.Descriptor().Fields()
	for _, p := range paths {
		f := fields.ByName(protoreflect.Name(p))
		if f == nil {
			continue
		}
		if src.Has(f) {
			d.Set(f, src.Get(f))
		} else {
			d.Clear(f)
		}
	}
}

// ---- google.longrunning.Operations ----

type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, ok := o.s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("operation %s not found", req.GetName())
	}
	return clone(op), nil
}

func (o *operations) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	return o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
}

func (o *operations) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if _, ok := o.s.ops[req.GetName()]; !ok {
		return nil, apierror.NotFound("operation %s not found", req.GetName())
	}
	delete(o.s.ops, req.GetName())
	return &emptypb.Empty{}, apierror.Wrap(o.s.save())
}

// normalize lowercases the location of every name a request gives, its
// own and its resource's: BigQuery's locations are case-insensitive
// ("US" and "us"), and a resource is named by the lowercase one.
func normalize(m proto.Message) {
	var walk func(protoreflect.Message, int)
	walk = func(r protoreflect.Message, depth int) {
		fields := r.Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			switch {
			case f.Kind() == protoreflect.StringKind && !f.IsList() && !f.IsMap():
				switch f.Name() {
				case "name", "parent", "resource", "organization", "listing", "data_exchange", "location", "destination":
				default:
					continue
				}
				v := r.Get(f).String()
				if f.Name() == "location" {
					continue
				}
				if i := strings.Index(v, "/locations/"); i >= 0 {
					rest := v[i+len("/locations/"):]
					loc, tail, _ := strings.Cut(rest, "/")
					nv := v[:i] + "/locations/" + strings.ToLower(loc)
					if tail != "" || strings.Contains(rest, "/") {
						nv += "/" + tail
					}
					if nv != v {
						r.Set(f, protoreflect.ValueOfString(nv))
					}
				}
			case f.Kind() == protoreflect.MessageKind && !f.IsList() && !f.IsMap() && depth == 0 && r.Has(f):
				walk(r.Mutable(f).Message(), depth+1)
			}
		}
	}
	if m != nil {
		walk(m.ProtoReflect(), 0)
	}
}
