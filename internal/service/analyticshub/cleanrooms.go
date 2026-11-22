package analyticshub

import (
	"context"
	"regexp"
	"sort"
	"strings"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Data clean rooms and query templates (#2101).
//
// A data clean room is a data exchange whose sharing_environment_config is
// dcr_exchange_config ("Create a data clean room",
// https://docs.cloud.google.com/bigquery/docs/data-clean-rooms, read
// 2026-11-21). As the proto has it, and as "all new DCRs" are:
//
//   - single_selected_resource_sharing_restriction: a listing shares "only a
//     single resource" (one selected_resources entry: a table, a view or,
//     for a query template, a routine), "you can't publish an entire
//     dataset directly", and "no two resources should have the same IDs. So
//     if a contributor adds a view with a conflicting name, the
//     CreateListing API will reject the request";
//   - single_linked_dataset_per_cleanroom: "Subscribing to a data clean room
//     creates one linked dataset in your project", holding every listing's
//     resource; it "has the same name as the data clean room" unless
//     destination_dataset names another. A listing added later is added to
//     it, a listing deleted leaves it ("the linked datasets aren't
//     deleted", and queries of that resource fail);
//   - egress: "Data egress controls are automatically enabled to prevent
//     data clean room subscribers from copying and exporting raw data", so
//     restricted_export_config.enabled is always set; the contributor may
//     add restrict_query_result, and "After a listing is created, you can't
//     change its source resource or data egress controls";
//   - "A maximum of 100 shared resources is allowed per clean room."
//
// The analysis rules are the shared views' privacy_policy, which the
// BigQuery front enforces on a query of the linked dataset as on the view
// itself (bigqueryfront/hublinks.go, privacypolicy.go).
//
// A query template (https://docs.cloud.google.com/bigquery/docs/query-templates,
// read 2026-11-21) is DRAFTED when created, editable and deletable;
// SubmitQueryTemplate makes it PENDING ("Requires review"), which "can no
// longer be modified or deleted"; ApproveQueryTemplate makes it APPROVED.
// Its display name "can't be changed after creation". The approver makes
// the table-valued function with a query (CREATE TABLE FUNCTION) and lists
// it as a routine of the clean room, so subscribers call it in the linked
// dataset: SELECT * FROM `my_project.dcr_linked_dataset.mytvf`(...).
//
// Google documents none of the errors below; their codes and wording are
// CloudBurrow's (UNVERIFIED (docs re-read 2026-11-21: <https://docs.cloud.google.com/bigquery/docs/data-clean-rooms>)).

// maxCleanRoomResources is "A maximum of 100 shared resources is allowed
// per clean room."
const maxCleanRoomResources = 100

func isCleanRoom(e *ahpb.DataExchange) bool {
	return e.GetSharingEnvironmentConfig().GetDcrExchangeConfig() != nil
}

// resourceID is the ID of a listing's one selected resource, and its kind.
func resourceID(l *ahpb.Listing) (id string, routine bool) {
	rs := l.GetBigqueryDataset().GetSelectedResources()
	if len(rs) != 1 {
		return "", false
	}
	if r := rs[0].GetRoutine(); r != "" {
		return r[strings.LastIndex(r, "/")+1:], true
	}
	t := rs[0].GetTable()
	return t[strings.LastIndex(t, "/")+1:], false
}

// cleanRoomListing checks and completes a new listing l of the clean room
// exchange; the caller holds mu.
func (s *Server) cleanRoomListing(l *ahpb.Listing, exchange string) error {
	rs := l.GetBigqueryDataset().GetSelectedResources()
	switch {
	case len(rs) == 0:
		return apierror.InvalidArgument("A listing of a data clean room shares one table, view or routine: set bigquery_dataset.selected_resources. " +
			"An entire dataset can't be published in a data clean room.")
	case len(rs) > 1:
		return apierror.InvalidArgument("A listing of a data clean room shares a single resource (single_selected_resource_sharing_restriction); %d were selected", len(rs))
	}
	id, _ := resourceID(l)
	others := sortedKeys(s.listings, exchange+"/listings/")
	if len(others) >= maxCleanRoomResources {
		return apierror.FailedPrecondition("Data clean room %s already shares %d resources, the maximum allowed per clean room", exchange, maxCleanRoomResources)
	}
	for _, k := range others {
		if other, _ := resourceID(s.listings[k].Listing); strings.EqualFold(other, id) {
			return apierror.AlreadyExists("Data clean room %s already shares a resource named %s (listing %s); no two resources of a clean room may have the same ID", exchange, id, k)
		}
	}
	c := l.GetRestrictedExportConfig()
	l.RestrictedExportConfig = &ahpb.Listing_RestrictedExportConfig{Enabled: true, RestrictQueryResult: c.GetRestrictQueryResult()}
	return nil
}

// cleanRoomUpdate refuses an update of a clean room listing's source
// resource or egress controls.
func cleanRoomUpdate(cur, in *ahpb.Listing, paths []string) error {
	if hasPath(paths, "restricted_export_config") {
		c := in.GetRestrictedExportConfig()
		if !c.GetEnabled() || c.GetRestrictQueryResult() != cur.GetRestrictedExportConfig().GetRestrictQueryResult() {
			return apierror.InvalidArgument("After a listing of a data clean room is created, its data egress controls (restricted_export_config) can't be changed")
		}
	}
	if hasPath(paths, "bigquery_dataset") || hasPath(paths, "bigquery_dataset.selected_resources") {
		a, b := cur.GetBigqueryDataset().GetSelectedResources(), in.GetBigqueryDataset().GetSelectedResources()
		same := len(a) == len(b)
		for i := 0; same && i < len(a); i++ {
			same = proto.Equal(a[i], b[i])
		}
		if !same {
			return apierror.InvalidArgument("After a listing of a data clean room is created, its source resource (bigquery_dataset.selected_resources) can't be changed")
		}
	}
	return nil
}

// exchangeSubs are a clean room's subscriptions; the caller holds mu.
func (s *Server) exchangeSubs(exchange string) []*subscriptionRecord {
	var out []*subscriptionRecord
	for _, k := range sortedKeys(s.subs, "") {
		if r := s.subs[k]; r.Sub.GetDataExchange() == exchange && r.Deleted.IsZero() {
			out = append(out, r)
		}
	}
	return out
}

// exchangeLink is the linked dataset a clean room's subscription has, from
// what base names: every listing's resource.
func (s *Server) exchangeLink(exchange string, base LinkedDataset) LinkedDataset {
	base.CleanRoom = true
	base.Resources, base.Tables = nil, nil
	base.Restricted, base.RestrictQueryResult = true, false
	base.SourceProject, base.SourceDataset = "", ""
	for _, k := range sortedKeys(s.listings, exchange+"/listings/") {
		l := s.listings[k].Listing
		m := datasetRE.FindStringSubmatch(l.GetBigqueryDataset().GetDataset())
		if m == nil {
			continue
		}
		res := LinkedResource{SourceProject: m[1], SourceDataset: m[2], RestrictQueryResult: l.GetRestrictedExportConfig().GetRestrictQueryResult()}
		id, routine := resourceID(l)
		if id == "" {
			continue
		}
		if routine {
			res.Routine = id
		} else {
			res.Table = id
		}
		base.RestrictQueryResult = base.RestrictQueryResult || res.RestrictQueryResult
		if base.SourceProject == "" {
			base.SourceProject, base.SourceDataset = m[1], m[2]
		}
		base.Resources = append(base.Resources, res)
	}
	return base
}

// setExchangeLinked sets a clean room subscription's linked resources:
// each listing's, all in dataset.
func (s *Server) setExchangeLinked(sub *ahpb.Subscription, exchange, dataset string) {
	sub.LinkedDatasetMap, sub.LinkedResources = map[string]*ahpb.Subscription_LinkedResource{}, nil
	for _, k := range sortedKeys(s.listings, exchange+"/listings/") {
		lr := &ahpb.Subscription_LinkedResource{Listing: k, Reference: &ahpb.Subscription_LinkedResource_LinkedDataset{LinkedDataset: dataset}}
		sub.LinkedDatasetMap[k] = lr
		sub.LinkedResources = append(sub.LinkedResources, clone(lr))
	}
}

// relinkExchange brings each active subscription of a clean room up to
// date with its listings; the caller holds mu. Not a clean room: nothing.
func (s *Server) relinkExchange(ctx context.Context, exchange string) error {
	ex, ok := s.exchanges[exchange]
	if !ok || !isCleanRoom(ex.Exchange) {
		return nil
	}
	for _, r := range s.exchangeSubs(exchange) {
		if r.Sub.GetState() != ahpb.Subscription_STATE_ACTIVE {
			continue
		}
		r.Linked = s.exchangeLink(exchange, r.Linked)
		s.setExchangeLinked(r.Sub, exchange, "projects/"+r.Linked.Project+"/datasets/"+r.Linked.Dataset)
		r.Sub.LastModifyTime = timestamppb.New(s.now())
		if s.link != nil {
			if err := s.link.UpdateLinked(ctx, r.Linked); err != nil {
				return err
			}
		}
	}
	return nil
}

// subscriptionIDRE is a subscription's ID: letters, numbers, underscores
// and dashes (CloudBurrow's rule; Google documents none, UNVERIFIED (docs re-read 2026-11-21: <https://docs.cloud.google.com/bigquery/docs/reference/analytics-hub/rest/v1/projects.locations.dataExchanges/subscribe>)).
var subscriptionIDRE = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,100}$`)

// SubscribeDataExchange subscribes to a data clean room: one linked
// dataset of every listing's resource, in the subscriber's project.
func (s *Server) SubscribeDataExchange(ctx context.Context, req *ahpb.SubscribeDataExchangeRequest) (*longrunningpb.Operation, error) {
	normalize(req)
	em := exchangeRE.FindStringSubmatch(req.GetName())
	if em == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/dataExchanges/{data_exchange}", req.GetName())
	}
	dm := locationRE.FindStringSubmatch(req.GetDestination())
	if dm == nil {
		return nil, apierror.InvalidArgument("destination %q must be projects/{project}/locations/{location}", req.GetDestination())
	}
	if !subscriptionIDRE.MatchString(req.GetSubscription()) {
		return nil, apierror.InvalidArgument("subscription %q must be letters, numbers, underscores and dashes, at most 100 characters", req.GetSubscription())
	}
	if dm[2] != em[2] {
		return nil, apierror.InvalidArgument("destination %s must be in the clean room's location, %s", req.GetDestination(), em[2])
	}
	project := dm[1]
	dst := clone(req.GetDestinationDataset())
	if dst == nil {
		dst = &ahpb.DestinationDataset{}
	}
	if dst.DatasetReference == nil {
		dst.DatasetReference = &ahpb.DestinationDatasetReference{}
	}
	if dst.DatasetReference.ProjectId == "" {
		dst.DatasetReference.ProjectId = project
	}
	if dst.DatasetReference.DatasetId == "" {
		// "The linked dataset has the same name as the data clean room."
		dst.DatasetReference.DatasetId = em[3]
	}
	if dst.Location == "" {
		dst.Location = em[2]
	}
	ref := dst.GetDatasetReference()
	switch {
	case ref.GetProjectId() != project:
		return nil, apierror.InvalidArgument("destination_dataset.dataset_reference.project_id %s must be the destination's project, %s", ref.GetProjectId(), project)
	case len(ref.GetDatasetId()) > 1024 || !datasetIDRE.MatchString(ref.GetDatasetId()):
		return nil, apierror.InvalidArgument("destination_dataset.dataset_reference.dataset_id %q must contain only letters, numbers and underscores, at most 1024 characters", ref.GetDatasetId())
	case !strings.EqualFold(dst.GetLocation(), em[2]):
		return nil, apierror.InvalidArgument("destination_dataset.location %s must be the clean room's location, %s", dst.GetLocation(), em[2])
	case len(dst.GetReplicaLocations()) > 0:
		return nil, apierror.Unimplemented("linked dataset replicas (destination_dataset.replica_locations) are not served by CloudBurrow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, err := s.exchange(req.GetName())
	if err != nil {
		return nil, err
	}
	if !isCleanRoom(ex.Exchange) {
		return nil, apierror.FailedPrecondition("DataExchange %s is not a data clean room (sharing_environment_config.dcr_exchange_config); subscribe to its listings with SubscribeListing", req.GetName())
	}
	name := req.GetDestination() + "/subscriptions/" + req.GetSubscription()
	if r, ok := s.subs[name]; ok && r.Deleted.IsZero() {
		return nil, apierror.AlreadyExists("Subscription %s already exists", name)
	}
	if s.link == nil {
		return nil, apierror.FailedPrecondition("BigQuery is not enabled on this instance, so no linked dataset can be made")
	}
	link := s.exchangeLink(req.GetName(), LinkedDataset{Project: project, Dataset: ref.GetDatasetId(), Location: dst.GetLocation(),
		FriendlyName: dst.GetFriendlyName().GetValue(), Description: dst.GetDescription().GetValue(), Labels: dst.GetLabels()})
	if err := s.link.CreateLinked(ctx, link); err != nil {
		return nil, err
	}
	now := timestamppb.New(s.now())
	sub := &ahpb.Subscription{
		Name:                           name,
		ResourceName:                   &ahpb.Subscription_DataExchange{DataExchange: req.GetName()},
		CreationTime:                   now,
		LastModifyTime:                 now,
		State:                          ahpb.Subscription_STATE_ACTIVE,
		ResourceType:                   ahpb.SharedResourceType_BIGQUERY_DATASET,
		DestinationDataset:             dst,
		SubscriberContact:              req.GetSubscriberContact(),
		LogLinkedDatasetQueryUserEmail: proto.Bool(ex.Exchange.GetLogLinkedDatasetQueryUserEmail()),
	}
	s.setExchangeLinked(sub, req.GetName(), "projects/"+project+"/datasets/"+ref.GetDatasetId())
	s.subs[name] = &subscriptionRecord{Sub: sub, Linked: link}
	op, err := s.operation(name, "create", &ahpb.SubscribeDataExchangeResponse{Subscription: clone(sub)})
	if err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return op, nil
}

// ---- query templates ----

func (s *Server) template(name string) (*ahpb.QueryTemplate, error) {
	if !queryTemplateRE.MatchString(name) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/dataExchanges/{data_exchange}/queryTemplates/{query_template}", name)
	}
	t, ok := s.templates[name]
	if !ok {
		return nil, apierror.NotFound("QueryTemplate %s not found", name)
	}
	return t, nil
}

func checkTemplate(t *ahpb.QueryTemplate) error {
	if err := checkDisplayName(t.GetDisplayName()); err != nil {
		return err
	}
	if err := checkText("description", t.GetDescription(), 2000); err != nil {
		return err
	}
	if err := checkText("primary_contact", t.GetPrimaryContact(), 1000); err != nil {
		return err
	}
	if err := checkText("proposer", t.GetProposer(), 1000); err != nil {
		return err
	}
	if r := t.GetRoutine(); r != nil && r.GetRoutineType() != ahpb.Routine_TABLE_VALUED_FUNCTION {
		return apierror.InvalidArgument("routine.routine_type must be TABLE_VALUED_FUNCTION: a query template is a table-valued function")
	}
	return nil
}

func (s *Server) CreateQueryTemplate(_ context.Context, req *ahpb.CreateQueryTemplateRequest) (*ahpb.QueryTemplate, error) {
	normalize(req)
	if err := checkID("query_template_id", req.GetQueryTemplateId()); err != nil {
		return nil, err
	}
	t := req.GetQueryTemplate()
	if t == nil {
		return nil, apierror.InvalidArgument("query_template is required")
	}
	if err := checkTemplate(t); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, err := s.exchange(req.GetParent())
	if err != nil {
		return nil, err
	}
	if !isCleanRoom(ex.Exchange) {
		return nil, apierror.FailedPrecondition("DataExchange %s is not a data clean room: query templates are a data clean room's", req.GetParent())
	}
	name := req.GetParent() + "/queryTemplates/" + req.GetQueryTemplateId()
	if _, ok := s.templates[name]; ok {
		return nil, apierror.AlreadyExists("QueryTemplate %s already exists", name)
	}
	t = clone(t)
	now := timestamppb.New(s.now())
	t.Name, t.State, t.CreateTime, t.UpdateTime = name, ahpb.QueryTemplate_DRAFTED, now, now
	s.templates[name] = t
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(t), nil
}

func (s *Server) GetQueryTemplate(_ context.Context, req *ahpb.GetQueryTemplateRequest) (*ahpb.QueryTemplate, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.template(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(t), nil
}

func (s *Server) ListQueryTemplates(_ context.Context, req *ahpb.ListQueryTemplatesRequest) (*ahpb.ListQueryTemplatesResponse, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.exchange(req.GetParent()); err != nil {
		return nil, err
	}
	var all []*ahpb.QueryTemplate
	for _, k := range sortedKeys(s.templates, req.GetParent()+"/queryTemplates/") {
		all = append(all, clone(s.templates[k]))
	}
	out, next, err := page("ah-templates:"+req.GetParent(), all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &ahpb.ListQueryTemplatesResponse{QueryTemplates: out, NextPageToken: next}, nil
}

// templateMask are the paths UpdateQueryTemplate takes; the display name
// "can't be changed after creation".
var templateMask = map[string]bool{"description": true, "proposer": true, "primary_contact": true, "documentation": true, "routine": true}

func (s *Server) UpdateQueryTemplate(_ context.Context, req *ahpb.UpdateQueryTemplateRequest) (*ahpb.QueryTemplate, error) {
	normalize(req)
	in := req.GetQueryTemplate()
	if in == nil {
		return nil, apierror.InvalidArgument("query_template is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		// The mask is optional: every field the template may change.
		for p := range templateMask {
			paths = append(paths, p)
		}
		sort.Strings(paths)
	}
	for _, p := range paths {
		if p == "display_name" {
			return nil, apierror.InvalidArgument("the display name of a query template can't be changed after creation")
		}
		if !templateMask[p] {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated", p)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.template(in.GetName())
	if err != nil {
		return nil, err
	}
	if cur.GetState() != ahpb.QueryTemplate_DRAFTED {
		return nil, apierror.FailedPrecondition("QueryTemplate %s is %s: only a DRAFTED query template can be modified", in.GetName(), cur.GetState())
	}
	next := clone(cur)
	applyMask(next, in, paths)
	if err := checkTemplate(next); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	s.templates[next.GetName()] = next
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(next), nil
}

func (s *Server) DeleteQueryTemplate(_ context.Context, req *ahpb.DeleteQueryTemplateRequest) (*emptypb.Empty, error) {
	normalize(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.template(req.GetName())
	if err != nil {
		return nil, err
	}
	if t.GetState() != ahpb.QueryTemplate_DRAFTED {
		return nil, apierror.FailedPrecondition("QueryTemplate %s is %s: only a DRAFTED query template can be deleted", req.GetName(), t.GetState())
	}
	delete(s.templates, req.GetName())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// transition moves a template from one state to another.
func (s *Server) transition(name string, from, to ahpb.QueryTemplate_State, verb string) (*ahpb.QueryTemplate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.template(name)
	if err != nil {
		return nil, err
	}
	if t.GetState() != from {
		return nil, apierror.FailedPrecondition("QueryTemplate %s is %s: only a %s query template can be %s", name, t.GetState(), from, verb)
	}
	if to == ahpb.QueryTemplate_PENDING && strings.TrimSpace(t.GetRoutine().GetDefinitionBody()) == "" {
		return nil, apierror.FailedPrecondition("QueryTemplate %s has no routine.definition_body to review", name)
	}
	t.State, t.UpdateTime = to, timestamppb.New(s.now())
	if err := s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(t), nil
}

func (s *Server) SubmitQueryTemplate(_ context.Context, req *ahpb.SubmitQueryTemplateRequest) (*ahpb.QueryTemplate, error) {
	normalize(req)
	return s.transition(req.GetName(), ahpb.QueryTemplate_DRAFTED, ahpb.QueryTemplate_PENDING, "submitted")
}

func (s *Server) ApproveQueryTemplate(_ context.Context, req *ahpb.ApproveQueryTemplateRequest) (*ahpb.QueryTemplate, error) {
	normalize(req)
	return s.transition(req.GetName(), ahpb.QueryTemplate_PENDING, ahpb.QueryTemplate_APPROVED, "approved")
}
