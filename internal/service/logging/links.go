package logging

// The rest of google.logging.v2.ConfigServiceV2 (#1292): the Async bucket
// calls, links and the project's settings.
//
//   - CreateBucketAsync and UpdateBucketAsync do what CreateBucket and
//     UpdateBucket do, and answer an Operation that is already done, with a
//     BucketMetadata and the bucket as its response. A client's Wait returns
//     at once without polling; the Operations mixin is not served.
//   - A link is stored under its bucket, which must have Log Analytics on
//     (analytics_enabled), and names the BigQuery dataset Google would make
//     for it. No dataset is made: the link is configuration. CreateLink and
//     DeleteLink answer done Operations with a LinkMetadata.
//   - Settings and CmekSettings are read and written per parent (#1395). A KMS key
//     is refused UNIMPLEMENTED, as a bucket's cmek_settings is: entries are
//     not encrypted with a customer key here. The service account IDs are
//     made up from the project ID (UNVERIFIED: Google's use the project
//     number).

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// doneOp is a finished Operation under {parent}/locations/{l}.
func (a *Admin) doneOp(project parent, location string, metadata, response proto.Message) (*longrunningpb.Operation, error) {
	md, err := anypb.New(metadata)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "operation metadata"))
	}
	resp, err := anypb.New(response)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "operation response"))
	}
	a.ops++
	return &longrunningpb.Operation{
		Name:     fmt.Sprintf("%s/locations/%s/operations/op-%d-%d", project, location, a.now().UnixNano(), a.ops),
		Metadata: md, Done: true, Result: &longrunningpb.Operation_Response{Response: resp},
	}, nil
}

func (s configServer) bucketOp(b *loggingpb.LogBucket, md *loggingpb.BucketMetadata) (*longrunningpb.Operation, error) {
	ref, _ := parseBucket("name", b.GetName())
	now := timestamppb.New(s.a.now())
	md.StartTime, md.EndTime, md.State = now, now, loggingpb.OperationState_OPERATION_STATE_SUCCEEDED
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	return s.a.doneOp(ref.parent, ref.location, md, b)
}

// CreateBucketAsync is CreateBucket, answered as a done Operation.
func (s configServer) CreateBucketAsync(ctx context.Context, req *loggingpb.CreateBucketRequest) (*longrunningpb.Operation, error) {
	b, err := s.CreateBucket(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.bucketOp(b, &loggingpb.BucketMetadata{Request: &loggingpb.BucketMetadata_CreateBucketRequest{CreateBucketRequest: req}})
}

// UpdateBucketAsync is UpdateBucket, answered as a done Operation.
func (s configServer) UpdateBucketAsync(ctx context.Context, req *loggingpb.UpdateBucketRequest) (*longrunningpb.Operation, error) {
	b, err := s.UpdateBucket(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.bucketOp(b, &loggingpb.BucketMetadata{Request: &loggingpb.BucketMetadata_UpdateBucketRequest{UpdateBucketRequest: req}})
}

// --- links ---------------------------------------------------------------

type linkRef struct {
	bucket bucketRef
	link   string
}

func (l linkRef) name() string { return l.bucket.name() + "/links/" + l.link }

func (l linkRef) key() string {
	return linkPrefix(l.bucket.parent) + hex.EncodeToString([]byte(l.bucket.location+"/"+l.bucket.bucket+"/"+l.link))
}

func linkPrefix(p parent) string { return "logging/links/" + p.seg() + "/" }

func parseLink(field, name string) (linkRef, error) {
	p, err := splitName(field, name, "buckets", "links")
	if err != nil {
		return linkRef{}, err
	}
	if p.location == "-" {
		return linkRef{}, apierror.InvalidArgument("%s %q: name a location, not -", field, name)
	}
	return linkRef{bucketRef{p.parent, p.location, p.ids[0]}, p.ids[1]}, nil
}

func (a *Admin) getLink(ref linkRef) (*loggingpb.Link, error) {
	raw, err := a.db.Get(ref.key())
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("link %s does not exist", ref.name())
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the link")
	}
	l := &loggingpb.Link{}
	if err := protojson.Unmarshal(raw, l); err != nil {
		return nil, apierror.Internal(err, "decoding the link")
	}
	return l, nil
}

// links lists the links of one bucket, in name order.
func (a *Admin) links(bucket bucketRef) ([]*loggingpb.Link, error) {
	keys, err := a.db.List(linkPrefix(bucket.parent))
	if err != nil {
		return nil, apierror.Internal(err, "listing links")
	}
	var out []*loggingpb.Link
	for _, k := range keys {
		raw, err := hex.DecodeString(strings.TrimPrefix(k, linkPrefix(bucket.parent)))
		if err != nil {
			continue
		}
		parts := strings.SplitN(string(raw), "/", 3)
		if len(parts) != 3 || parts[0] != bucket.location || parts[1] != bucket.bucket {
			continue
		}
		if l, err := a.getLink(linkRef{bucket, parts[2]}); err == nil {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

func linkOp(a *Admin, ref linkRef, md *loggingpb.LinkMetadata, response proto.Message) (*longrunningpb.Operation, error) {
	now := timestamppb.New(a.now())
	md.StartTime, md.EndTime, md.State = now, now, loggingpb.OperationState_OPERATION_STATE_SUCCEEDED
	return a.doneOp(ref.bucket.parent, ref.bucket.location, md, response)
}

// CreateLink links a bucket with Log Analytics on to a BigQuery dataset
// named after the link. The dataset is named, not made.
func (s configServer) CreateLink(_ context.Context, req *loggingpb.CreateLinkRequest) (*longrunningpb.Operation, error) {
	bucket, err := parseBucket("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if !bucket.parent.isProject() {
		// The dataset a link makes is in a project; which one Google uses for
		// a folder's, organization's or billing account's bucket is not
		// something this has measured.
		return nil, apierror.Wrap(apierror.Unimplemented("parent %q: links are supported on project buckets only", req.GetParent()))
	}
	id := req.GetLinkId()
	if err := checkID("link", id, configIDRe); err != nil {
		return nil, apierror.Wrap(err)
	}
	in := req.GetLink()
	if in == nil {
		in = &loggingpb.Link{}
	}
	if err := checkDescription(in.GetDescription()); err != nil {
		return nil, apierror.Wrap(err)
	}
	ref := linkRef{bucket, id}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	b, err := s.a.getBucket(bucket)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if !b.GetAnalyticsEnabled() {
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s does not have Log Analytics enabled (analytics_enabled)", bucket.name()))
	}
	if _, err := s.a.getLink(ref); err == nil {
		return nil, apierror.Wrap(apierror.AlreadyExists("link %s already exists", ref.name()))
	}
	l := &loggingpb.Link{
		Name: ref.name(), Description: in.GetDescription(), CreateTime: timestamppb.New(s.a.now()),
		LifecycleState: loggingpb.LifecycleState_ACTIVE,
		BigqueryDataset: &loggingpb.BigQueryDataset{
			DatasetId: "bigquery.googleapis.com/projects/" + bucket.parent.id + "/datasets/" + strings.ReplaceAll(id, "-", "_"),
		},
	}
	raw, err := protojson.Marshal(l)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encoding the link"))
	}
	if err := s.a.db.Put(ref.key(), raw); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "writing the link"))
	}
	return linkOp(s.a, ref, &loggingpb.LinkMetadata{Request: &loggingpb.LinkMetadata_CreateLinkRequest{CreateLinkRequest: req}}, l)
}

// DeleteLink removes a link.
func (s configServer) DeleteLink(_ context.Context, req *loggingpb.DeleteLinkRequest) (*longrunningpb.Operation, error) {
	ref, err := parseLink("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if _, err := s.a.getLink(ref); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.a.db.Delete(ref.key()); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "deleting the link"))
	}
	return linkOp(s.a, ref, &loggingpb.LinkMetadata{Request: &loggingpb.LinkMetadata_DeleteLinkRequest{DeleteLinkRequest: req}}, &emptypb.Empty{})
}

// ListLinks lists a bucket's links, paged.
func (s configServer) ListLinks(_ context.Context, req *loggingpb.ListLinksRequest) (*loggingpb.ListLinksResponse, error) {
	bucket, err := parseBucket("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	all, err := s.a.links(bucket)
	s.a.mu.Unlock()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	byName := map[string]*loggingpb.Link{}
	names := make([]string, 0, len(all))
	for _, l := range all {
		byName[l.GetName()] = l
		names = append(names, l.GetName())
	}
	page, next, err := paging.Page("logging-links|"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("page_token: %v", err))
	}
	resp := &loggingpb.ListLinksResponse{NextPageToken: next}
	for _, n := range page {
		resp.Links = append(resp.Links, byName[n])
	}
	return resp, nil
}

// GetLink reads one link.
func (s configServer) GetLink(_ context.Context, req *loggingpb.GetLinkRequest) (*loggingpb.Link, error) {
	ref, err := parseLink("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	l, err := s.a.getLink(ref)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return l, nil
}

// --- settings ------------------------------------------------------------

// parseSettings takes {parent}/{suffix}, or {parent} as the JSON binding
// {name=*/*}/settings carries it, for any of the four parents.
func (a *Admin) parseSettings(field, name, suffix string) (parent, error) {
	p, rest, err := cutParent(field, name)
	if err != nil {
		return parent{}, err
	}
	if rest != "" && rest != suffix {
		return parent{}, apierror.InvalidArgument("%s %q is not {parent}/%s", field, name, suffix)
	}
	return p, a.check(p)
}

func settingsKey(p parent) string { return "logging/settings/" + p.seg() }

// serviceAccount is made up from the parent's ID (UNVERIFIED: Google's names
// a number).
func serviceAccount(p parent) string {
	id := strings.ToLower(p.id)
	switch p.coll {
	case collFolders:
		id = "fld-" + id
	case collOrgs:
		id = "org-" + id
	case collBilling:
		id = "bac-" + id
	}
	return "service-" + id + "@gcp-sa-logging.iam.gserviceaccount.com"
}

func (a *Admin) settings(project parent) (*loggingpb.Settings, error) {
	st := &loggingpb.Settings{}
	raw, err := a.db.Get(settingsKey(project))
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, apierror.Internal(err, "reading the settings")
	default:
		if err := protojson.Unmarshal(raw, st); err != nil {
			return nil, apierror.Internal(err, "decoding the settings")
		}
	}
	st.Name = project.String() + "/settings"
	st.KmsServiceAccountId = serviceAccount(project)
	return st, nil
}

// GetSettings reads the project's settings; a project never changed has the
// defaults: no storage location, the default sink on.
func (s configServer) GetSettings(_ context.Context, req *loggingpb.GetSettingsRequest) (*loggingpb.Settings, error) {
	project, err := s.a.parseSettings("name", req.GetName(), "settings")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	st, err := s.a.settings(project)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return st, nil
}

// UpdateSettings applies the update_mask (every settable field when it is
// empty): storage_location and disable_default_sink; a kms_key_name is
// refused.
func (s configServer) UpdateSettings(_ context.Context, req *loggingpb.UpdateSettingsRequest) (*loggingpb.Settings, error) {
	project, err := s.a.parseSettings("name", req.GetName(), "settings")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if project.coll == collFolders || project.coll == collBilling {
		// logging_config.proto: settings "can currently only be configured
		// for Google Cloud organizations"; they are read for all four.
		return nil, apierror.Wrap(apierror.FailedPrecondition("%s: Log Router settings can only be configured for organizations (and, here, projects); they are read for folders and billing accounts", req.GetName()))
	}
	in := req.GetSettings()
	if in == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("settings is required"))
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"kms_key_name", "storage_location", "disable_default_sink"}
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	st, err := s.a.settings(project)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	for _, p := range paths {
		switch p {
		case "kms_key_name":
			if in.GetKmsKeyName() != "" {
				return nil, apierror.Wrap(apierror.Unimplemented("kms_key_name: customer-managed encryption of log entries is not supported"))
			}
		case "storage_location":
			if l := in.GetStorageLocation(); l != "" && !locationRe.MatchString(l) {
				return nil, apierror.Wrap(apierror.InvalidArgument("storage_location %q is not a location", l))
			}
			st.StorageLocation = in.GetStorageLocation()
		case "disable_default_sink":
			st.DisableDefaultSink = in.GetDisableDefaultSink()
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("update_mask: %q is not a field UpdateSettings changes", p))
		}
	}
	raw, err := protojson.Marshal(&loggingpb.Settings{StorageLocation: st.GetStorageLocation(), DisableDefaultSink: st.GetDisableDefaultSink()})
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encoding the settings"))
	}
	if err := s.a.db.Put(settingsKey(project), raw); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "writing the settings"))
	}
	return st, nil
}

// GetCmekSettings answers the project's CMEK settings: no key, and the
// service account a key would be granted to.
func (s configServer) GetCmekSettings(_ context.Context, req *loggingpb.GetCmekSettingsRequest) (*loggingpb.CmekSettings, error) {
	project, err := s.a.parseSettings("name", req.GetName(), "cmekSettings")
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &loggingpb.CmekSettings{Name: project.String() + "/cmekSettings", ServiceAccountId: serviceAccount(project)}, nil
}

// UpdateCmekSettings accepts only clearing the key, which is never set.
func (s configServer) UpdateCmekSettings(ctx context.Context, req *loggingpb.UpdateCmekSettingsRequest) (*loggingpb.CmekSettings, error) {
	if _, err := s.a.parseSettings("name", req.GetName(), "cmekSettings"); err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetCmekSettings().GetKmsKeyName() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("kms_key_name: customer-managed encryption of log entries is not supported"))
	}
	return s.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: req.GetName()})
}
