package logging

// Log buckets and views of google.logging.v2.ConfigServiceV2 (#1264), the
// resources `gcloud logging buckets` and Terraform's
// google_logging_project_bucket_config manage.
//
// Every project has the two buckets Google creates for it, each with its
// _AllLogs view, in location global:
//   - _Default, retention 30 days, whose description and retention can be
//     changed but which cannot be deleted;
//   - _Required, retention 400 days and locked, which cannot be changed or
//     deleted.
//
// They are synthesized until a change to _Default is stored. A user bucket is
// created, updated (description, retention_days, locked, restricted_fields,
// index_configs, analytics_enabled), deleted (kept in DELETE_REQUESTED, as
// Google keeps it for 7 days, and purged when that has passed) and undeleted.
// A locked bucket refuses retention changes, unlocking and deletion. Buckets
// hold no entries here: every entry is stored once, as _Default's, so a
// bucket's retention and a view's filter change nothing that is read. They are
// configuration, kept in the Admin's store (durable in persistent mode).
//
// The Async variants, links and settings are in links.go (#1292); a bucket's
// cmek_settings stays UNIMPLEMENTED. Where
// Google documents no code for a refusal it is FAILED_PRECONDITION
// (UNVERIFIED).

import (
	"context"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const (
	bucketDefault  = "_Default"
	bucketRequired = "_Required"
	viewAllLogs    = "_AllLogs"
	// Retention limits logging_config.proto documents: 1 to 3650 days; 0
	// means the default of 30.
	defaultRetention  = 30
	requiredRetention = 400
	maxRetention      = 3650
	// bucketPurge is how long a deleted bucket is kept (DELETE_REQUESTED).
	bucketPurge = 7 * 24 * time.Hour
)

// locationRe is a location ID: global, or a region such as us-central1.
var locationRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// bucketUpdatable are the bucket fields an update_mask may name.
var bucketUpdatable = map[string]bool{
	"description": true, "retention_days": true, "locked": true,
	"restricted_fields": true, "index_configs": true, "analytics_enabled": true,
}

// viewUpdatable are the view fields an update_mask may name.
var viewUpdatable = map[string]bool{"description": true, "filter": true}

// bucketRef is projects/{project}/locations/{location}/buckets/{bucket}.
type bucketRef struct{ project, location, bucket string }

func (b bucketRef) name() string {
	return "projects/" + b.project + "/locations/" + b.location + "/buckets/" + b.bucket
}

func (b bucketRef) key() string {
	return "logging/buckets/" + b.project + "/" + hex.EncodeToString([]byte(b.location+"/"+b.bucket))
}

func builtinBucket(id string) bool { return id == bucketDefault || id == bucketRequired }

// splitName checks a name against projects/{p}/locations/{l}[/buckets/{b}[/views/{v}]]:
// want is the number of segments after the project.
func splitName(field, name string, collections ...string) ([]string, error) {
	for _, p := range []string{"organizations/", "folders/", "billingAccounts/"} {
		if strings.HasPrefix(name, p) {
			return nil, apierror.Unimplemented("%s %q: only projects/{project}/... is supported", field, name)
		}
	}
	parts := strings.Split(name, "/")
	shape := "projects/{project}/locations/{location}"
	for _, c := range collections {
		shape += "/" + c + "/{" + strings.TrimSuffix(c, "s") + "}"
	}
	if len(parts) != 4+2*len(collections) || parts[0] != "projects" || parts[2] != "locations" {
		return nil, apierror.InvalidArgument("%s %q is not %s", field, name, shape)
	}
	for i, c := range collections {
		if parts[4+2*i] != c {
			return nil, apierror.InvalidArgument("%s %q is not %s", field, name, shape)
		}
	}
	out := []string{}
	for i := 1; i < len(parts); i += 2 {
		if parts[i] == "" {
			return nil, apierror.InvalidArgument("%s %q is not %s", field, name, shape)
		}
		out = append(out, parts[i])
	}
	if _, err := parseProjectParent(field, "projects/"+out[0]); err != nil {
		return nil, err
	}
	if out[1] != "-" && !locationRe.MatchString(out[1]) {
		return nil, apierror.InvalidArgument("%s %q: %q is not a location", field, name, out[1])
	}
	return out, nil
}

func parseBucket(field, name string) (bucketRef, error) {
	p, err := splitName(field, name, "buckets")
	if err != nil {
		return bucketRef{}, err
	}
	if p[1] == "-" {
		return bucketRef{}, apierror.InvalidArgument("%s %q: name a location, not -", field, name)
	}
	return bucketRef{p[0], p[1], p[2]}, nil
}

func (a *Admin) defaultBucket(ref bucketRef) *loggingpb.LogBucket {
	b := &loggingpb.LogBucket{
		Name:           ref.name(),
		RetentionDays:  defaultRetention,
		LifecycleState: loggingpb.LifecycleState_ACTIVE,
		CreateTime:     timestamppb.New(time.Unix(0, 0)),
		UpdateTime:     timestamppb.New(time.Unix(0, 0)),
	}
	if ref.bucket == bucketRequired {
		b.RetentionDays, b.Locked = requiredRetention, true
		b.Description = "Audit bucket"
	} else {
		b.Description = "Default bucket"
	}
	return b
}

// getBucket reads a bucket, the synthesized built-ins included. A deleted
// bucket past its purge time is removed and NOT_FOUND.
func (a *Admin) getBucket(ref bucketRef) (*loggingpb.LogBucket, error) {
	raw, err := a.db.Get(ref.key())
	if errors.Is(err, store.ErrNotFound) {
		if ref.location == "global" && builtinBucket(ref.bucket) {
			return a.defaultBucket(ref), nil
		}
		return nil, apierror.NotFound("bucket %s does not exist", ref.name())
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the bucket")
	}
	b := &loggingpb.LogBucket{}
	if err := protojson.Unmarshal(raw, b); err != nil {
		return nil, apierror.Internal(err, "decoding the bucket")
	}
	if b.GetLifecycleState() == loggingpb.LifecycleState_DELETE_REQUESTED && a.now().Sub(b.GetUpdateTime().AsTime()) > bucketPurge {
		_ = a.purgeBucket(ref)
		return nil, apierror.NotFound("bucket %s does not exist", ref.name())
	}
	return b, nil
}

func (a *Admin) putBucket(ref bucketRef, b *loggingpb.LogBucket) error {
	raw, err := protojson.Marshal(b)
	if err != nil {
		return apierror.Internal(err, "encoding the bucket")
	}
	if err := a.db.Put(ref.key(), raw); err != nil {
		return apierror.Internal(err, "writing the bucket")
	}
	return nil
}

// purgeBucket removes a bucket and its views.
func (a *Admin) purgeBucket(ref bucketRef) error {
	keys, _ := a.db.List(viewPrefix(ref))
	for _, k := range keys {
		_ = a.db.Delete(k)
	}
	return a.db.Delete(ref.key())
}

func checkBucket(b *loggingpb.LogBucket) error {
	if err := checkDescription(b.GetDescription()); err != nil {
		return err
	}
	if d := b.GetRetentionDays(); d < 0 || d > maxRetention {
		return apierror.InvalidArgument("retention_days must be between 1 and %d, or 0 for the default of %d", maxRetention, defaultRetention)
	}
	if b.GetCmekSettings() != nil {
		return apierror.Unimplemented("cmek_settings: customer-managed encryption of a log bucket is not supported")
	}
	return nil
}

// buckets lists the buckets of a project in one location, or every location
// for "-", the built-ins first-class among them, in name order.
func (a *Admin) buckets(project, location string) ([]*loggingpb.LogBucket, error) {
	keys, err := a.db.List("logging/buckets/" + project + "/")
	if err != nil {
		return nil, apierror.Internal(err, "listing buckets")
	}
	seen := map[string]bool{}
	var out []*loggingpb.LogBucket
	for _, k := range keys {
		raw, err := hex.DecodeString(strings.TrimPrefix(k, "logging/buckets/"+project+"/"))
		if err != nil {
			continue
		}
		loc, id, _ := strings.Cut(string(raw), "/")
		if location != "-" && loc != location {
			continue
		}
		ref := bucketRef{project, loc, id}
		b, err := a.getBucket(ref)
		if err != nil {
			continue
		}
		seen[ref.name()] = true
		out = append(out, b)
	}
	if location == "-" || location == "global" {
		for _, id := range []string{bucketDefault, bucketRequired} {
			ref := bucketRef{project, "global", id}
			if !seen[ref.name()] {
				out = append(out, a.defaultBucket(ref))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

func (s configServer) ListBuckets(_ context.Context, req *loggingpb.ListBucketsRequest) (*loggingpb.ListBucketsResponse, error) {
	p, err := splitName("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	all, err := s.a.buckets(p[0], p[1])
	s.a.mu.Unlock()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	byName := map[string]*loggingpb.LogBucket{}
	names := make([]string, 0, len(all))
	for _, b := range all {
		byName[b.GetName()] = b
		names = append(names, b.GetName())
	}
	page, next, err := paging.Page("logging-buckets|"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("page_token: %v", err))
	}
	resp := &loggingpb.ListBucketsResponse{NextPageToken: next}
	for _, n := range page {
		resp.Buckets = append(resp.Buckets, byName[n])
	}
	return resp, nil
}

func (s configServer) GetBucket(_ context.Context, req *loggingpb.GetBucketRequest) (*loggingpb.LogBucket, error) {
	ref, err := parseBucket("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	b, err := s.a.getBucket(ref)
	return b, apierror.Wrap(err)
}

func (s configServer) CreateBucket(_ context.Context, req *loggingpb.CreateBucketRequest) (*loggingpb.LogBucket, error) {
	p, err := splitName("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if p[1] == "-" {
		return nil, apierror.Wrap(apierror.InvalidArgument("parent %q: name a location, not -", req.GetParent()))
	}
	id := req.GetBucketId()
	if strings.HasPrefix(id, "_") {
		return nil, apierror.Wrap(apierror.InvalidArgument("bucket_id %q: an ID starting with _ is reserved", id))
	}
	if err := checkID("bucket", id, configIDRe); err != nil {
		return nil, apierror.Wrap(err)
	}
	b := proto.Clone(req.GetBucket()).(*loggingpb.LogBucket)
	if b == nil {
		b = &loggingpb.LogBucket{}
	}
	if err := checkBucket(b); err != nil {
		return nil, apierror.Wrap(err)
	}
	ref := bucketRef{p[0], p[1], id}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if _, err := s.a.getBucket(ref); err == nil {
		return nil, apierror.Wrap(apierror.AlreadyExists("bucket %s already exists", ref.name()))
	}
	now := timestamppb.New(s.a.now())
	b.Name, b.CreateTime, b.UpdateTime = ref.name(), now, now
	b.LifecycleState = loggingpb.LifecycleState_ACTIVE
	if b.GetRetentionDays() == 0 {
		b.RetentionDays = defaultRetention
	}
	if err := s.a.putBucket(ref, b); err != nil {
		return nil, apierror.Wrap(err)
	}
	return b, nil
}

// UpdateBucket applies the update_mask, which is required.
func (s configServer) UpdateBucket(_ context.Context, req *loggingpb.UpdateBucketRequest) (*loggingpb.LogBucket, error) {
	ref, err := parseBucket("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetBucket() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("bucket is required"))
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required: name the fields to change"))
	}
	if ref.bucket == bucketRequired {
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s cannot be modified", ref.name()))
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	old, err := s.a.getBucket(ref)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if old.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE {
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s is %s; undelete it before updating it", ref.name(), old.GetLifecycleState()))
	}
	b := proto.Clone(old).(*loggingpb.LogBucket)
	if err := applyMask(b, req.GetBucket(), req.GetUpdateMask(), bucketUpdatable); err != nil {
		return nil, apierror.Wrap(err)
	}
	if old.GetLocked() {
		switch {
		case !b.GetLocked():
			return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s is locked: a lock cannot be removed", ref.name()))
		case b.GetRetentionDays() != old.GetRetentionDays():
			return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s is locked: its retention cannot be changed", ref.name()))
		}
	}
	if err := checkBucket(b); err != nil {
		return nil, apierror.Wrap(err)
	}
	if b.GetRetentionDays() == 0 {
		b.RetentionDays = defaultRetention
	}
	b.UpdateTime = timestamppb.New(s.a.now())
	if err := s.a.putBucket(ref, b); err != nil {
		return nil, apierror.Wrap(err)
	}
	return b, nil
}

func (s configServer) DeleteBucket(_ context.Context, req *loggingpb.DeleteBucketRequest) (*emptypb.Empty, error) {
	ref, err := parseBucket("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if builtinBucket(ref.bucket) && ref.location == "global" {
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s cannot be deleted", ref.name()))
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	b, err := s.a.getBucket(ref)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	switch {
	case b.GetLifecycleState() == loggingpb.LifecycleState_DELETE_REQUESTED:
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s is already being deleted", ref.name()))
	case b.GetLocked():
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s is locked and cannot be deleted", ref.name()))
	}
	b.LifecycleState = loggingpb.LifecycleState_DELETE_REQUESTED
	b.UpdateTime = timestamppb.New(s.a.now())
	if err := s.a.putBucket(ref, b); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

func (s configServer) UndeleteBucket(_ context.Context, req *loggingpb.UndeleteBucketRequest) (*emptypb.Empty, error) {
	ref, err := parseBucket("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	b, err := s.a.getBucket(ref)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if b.GetLifecycleState() != loggingpb.LifecycleState_DELETE_REQUESTED {
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s is not being deleted", ref.name()))
	}
	b.LifecycleState = loggingpb.LifecycleState_ACTIVE
	b.UpdateTime = timestamppb.New(s.a.now())
	if err := s.a.putBucket(ref, b); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// ---- Views ----

func viewPrefix(b bucketRef) string {
	return "logging/views/" + b.project + "/" + hex.EncodeToString([]byte(b.location+"/"+b.bucket)) + "/"
}

func viewKey(b bucketRef, view string) string {
	return viewPrefix(b) + hex.EncodeToString([]byte(view))
}

func viewName(b bucketRef, view string) string { return b.name() + "/views/" + view }

func parseView(field, name string) (bucketRef, string, error) {
	p, err := splitName(field, name, "buckets", "views")
	if err != nil {
		return bucketRef{}, "", err
	}
	if p[1] == "-" {
		return bucketRef{}, "", apierror.InvalidArgument("%s %q: name a location, not -", field, name)
	}
	return bucketRef{p[0], p[1], p[2]}, p[3], nil
}

func allLogsView(b bucketRef) *loggingpb.LogView {
	epoch := timestamppb.New(time.Unix(0, 0))
	return &loggingpb.LogView{Name: viewName(b, viewAllLogs), Description: "All logs in the bucket", CreateTime: epoch, UpdateTime: epoch}
}

// activeBucket reads a bucket a view call needs, refusing a deleted one.
func (a *Admin) activeBucket(ref bucketRef) error {
	b, err := a.getBucket(ref)
	if err != nil {
		return err
	}
	if b.GetLifecycleState() != loggingpb.LifecycleState_ACTIVE {
		return apierror.FailedPrecondition("bucket %s is %s", ref.name(), b.GetLifecycleState())
	}
	return nil
}

func (a *Admin) getView(b bucketRef, id string) (*loggingpb.LogView, error) {
	raw, err := a.db.Get(viewKey(b, id))
	if errors.Is(err, store.ErrNotFound) {
		if id == viewAllLogs && !a.allLogsDeleted(b) {
			return allLogsView(b), nil
		}
		return nil, apierror.NotFound("view %s does not exist", viewName(b, id))
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the view")
	}
	v := &loggingpb.LogView{}
	if err := protojson.Unmarshal(raw, v); err != nil {
		return nil, apierror.Internal(err, "decoding the view")
	}
	return v, nil
}

// allLogsDeleted is whether a user bucket's _AllLogs view was deleted: a
// marker key, since the view is otherwise synthesized.
func (a *Admin) allLogsDeleted(b bucketRef) bool {
	_, err := a.db.Get(viewPrefix(b) + "deleted-alllogs")
	return err == nil
}

func (a *Admin) putView(b bucketRef, v *loggingpb.LogView, id string) error {
	raw, err := protojson.Marshal(v)
	if err != nil {
		return apierror.Internal(err, "encoding the view")
	}
	if err := a.db.Put(viewKey(b, id), raw); err != nil {
		return apierror.Internal(err, "writing the view")
	}
	return nil
}

func checkView(v *loggingpb.LogView) error {
	if err := checkDescription(v.GetDescription()); err != nil {
		return err
	}
	if len(v.GetFilter()) > maxFilterLen {
		return apierror.InvalidArgument("filter is longer than %d characters", maxFilterLen)
	}
	return nil
}

func (s configServer) ListViews(_ context.Context, req *loggingpb.ListViewsRequest) (*loggingpb.ListViewsResponse, error) {
	ref, err := parseBucket("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if _, err := s.a.getBucket(ref); err != nil {
		return nil, apierror.Wrap(err)
	}
	keys, err := s.a.db.List(viewPrefix(ref))
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "listing views"))
	}
	ids := map[string]bool{}
	if !s.a.allLogsDeleted(ref) {
		ids[viewAllLogs] = true
	}
	for _, k := range keys {
		if raw, err := hex.DecodeString(strings.TrimPrefix(k, viewPrefix(ref))); err == nil {
			ids[string(raw)] = true
		}
	}
	names := make([]string, 0, len(ids))
	for id := range ids {
		names = append(names, id)
	}
	page, next, err := paging.Page("logging-views|"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("page_token: %v", err))
	}
	resp := &loggingpb.ListViewsResponse{NextPageToken: next}
	for _, id := range page {
		if v, err := s.a.getView(ref, id); err == nil {
			resp.Views = append(resp.Views, v)
		}
	}
	return resp, nil
}

func (s configServer) GetView(_ context.Context, req *loggingpb.GetViewRequest) (*loggingpb.LogView, error) {
	ref, id, err := parseView("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if _, err := s.a.getBucket(ref); err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.a.getView(ref, id)
	return v, apierror.Wrap(err)
}

func (s configServer) CreateView(_ context.Context, req *loggingpb.CreateViewRequest) (*loggingpb.LogView, error) {
	ref, err := parseBucket("parent", req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	id := req.GetViewId()
	if strings.HasPrefix(id, "_") {
		return nil, apierror.Wrap(apierror.InvalidArgument("view_id %q: an ID starting with _ is reserved", id))
	}
	if err := checkID("view", id, configIDRe); err != nil {
		return nil, apierror.Wrap(err)
	}
	v := proto.Clone(req.GetView()).(*loggingpb.LogView)
	if v == nil {
		v = &loggingpb.LogView{}
	}
	if err := checkView(v); err != nil {
		return nil, apierror.Wrap(err)
	}
	if ref.bucket == bucketRequired {
		return nil, apierror.Wrap(apierror.FailedPrecondition("bucket %s cannot be modified", ref.name()))
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if err := s.a.activeBucket(ref); err != nil {
		return nil, apierror.Wrap(err)
	}
	if _, err := s.a.getView(ref, id); err == nil {
		return nil, apierror.Wrap(apierror.AlreadyExists("view %s already exists", viewName(ref, id)))
	}
	now := timestamppb.New(s.a.now())
	v.Name, v.CreateTime, v.UpdateTime = viewName(ref, id), now, now
	if err := s.a.putView(ref, v, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return v, nil
}

func (s configServer) UpdateView(_ context.Context, req *loggingpb.UpdateViewRequest) (*loggingpb.LogView, error) {
	ref, id, err := parseView("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetView() == nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("view is required"))
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("update_mask is required: name the fields to change"))
	}
	if id == viewAllLogs || ref.bucket == bucketRequired {
		return nil, apierror.Wrap(apierror.FailedPrecondition("view %s cannot be modified", viewName(ref, id)))
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if err := s.a.activeBucket(ref); err != nil {
		return nil, apierror.Wrap(err)
	}
	old, err := s.a.getView(ref, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v := proto.Clone(old).(*loggingpb.LogView)
	if err := applyMask(v, req.GetView(), req.GetUpdateMask(), viewUpdatable); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := checkView(v); err != nil {
		return nil, apierror.Wrap(err)
	}
	v.UpdateTime = timestamppb.New(s.a.now())
	if err := s.a.putView(ref, v, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return v, nil
}

func (s configServer) DeleteView(_ context.Context, req *loggingpb.DeleteViewRequest) (*emptypb.Empty, error) {
	ref, id, err := parseView("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if builtinBucket(ref.bucket) && ref.location == "global" && id == viewAllLogs {
		return nil, apierror.Wrap(apierror.FailedPrecondition("view %s cannot be deleted", viewName(ref, id)))
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if err := s.a.activeBucket(ref); err != nil {
		return nil, apierror.Wrap(err)
	}
	if _, err := s.a.getView(ref, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	if id == viewAllLogs {
		if err := s.a.db.Put(viewPrefix(ref)+"deleted-alllogs", []byte("1")); err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "deleting the view"))
		}
		_ = s.a.db.Delete(viewKey(ref, id))
		return &emptypb.Empty{}, nil
	}
	if err := s.a.db.Delete(viewKey(ref, id)); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "deleting the view"))
	}
	return &emptypb.Empty{}, nil
}

// Buckets lists a project's buckets in every location, for the console.
func (a *Admin) Buckets(project string) ([]*loggingpb.LogBucket, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buckets(project, "-")
}
