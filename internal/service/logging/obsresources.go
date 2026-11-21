package logging

// The Observability API's settings, operations, buckets, datasets, views and
// links (#2117): observability.googleapis.com v1, as its discovery document
// (revision 20261029) and Google's pages describe them (read 2026-11-21):
//
//   - docs.cloud.google.com/stackdriver/docs/observability/
//     set-defaults-for-observability-buckets: Settings are kept per
//     project, folder and organization and per location; "When the system
//     automatically creates an observability bucket, it uses the default
//     settings to determine the bucket's location and encryption key"; a
//     folder's or organization's settings "apply to descendants of the
//     folder, except those descendants that have default settings
//     configured"; "Cloud KMS keys are regional resources. They can only be
//     used to encrypt or decrypt data that is stored in the same location as
//     the key", and "If you set a default Cloud KMS key for a location, then
//     you must also set that location as the default storage location";
//     the service account is service-PROJECT_NUMBER@gcp-sa-observability
//     (service-org-ORGANIZATION_ID@ for an organization). updateSettings
//     answers a long-running operation.
//   - docs.cloud.google.com/trace/docs/storage-overview: Cloud Trace's data
//     is held by the observability bucket _Trace, in its dataset Spans,
//     read through the view _AllSpans ("That view includes all data in the
//     dataset"). The system creates the bucket "in response to receiving
//     trace data" (EnsureTraceBucket, which Cloud Trace calls), or it is
//     created through the Observability API; its location comes from the
//     default settings, else "the system selects a supported location". You
//     can't create, delete, or modify datasets or views.
//   - docs.cloud.google.com/stackdriver/docs/observability/
//     observability-bucket-locations: the locations a bucket may be in.
//   - docs.cloud.google.com/trace/docs/analytics-query-linked-dataset: a
//     link's ID is the name of the linked BigQuery dataset, "limited to 100
//     characters and can include only letters, digits, and underscores",
//     unique in the project; "Each observability dataset supports one
//     linked BigQuery dataset"; its location must match the bucket's.
//     links.create answers an operation. The link's dataset is made in the
//     instance's BigQuery (as Cloud Logging's links are, analytics.go), with
//     the trace storage schema (docs.cloud.google.com/trace/docs/reference/
//     trace-schema) and an _AllSpans view; AppendSpans fills it.
//   - buckets.reprovision "is allowed only a single time for a bucket and
//     results in data loss. Subsequent calls to this method for the same
//     bucket will result in a FAILED_PRECONDITION (400 Bad Request) error."
//
// Choices Google does not document (UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/trace/docs/storage-overview)): a bucket ID
// is 1-100 letters, digits, - and _, and only _Trace starts with _; a
// project holds one _Trace, in one location (a second is ALREADY_EXISTS);
// only _Trace has a dataset (Spans) and a view (_AllSpans); a bucket is in
// one of the documented bucket locations, not global (INVALID_ARGUMENT); a
// system-created _Trace without default settings is in "us"; a second link
// on a dataset is FAILED_PRECONDITION; a folder's service account is
// service-folder-FOLDER_ID@gcp-sa-observability; an operation is done when
// it is answered, so cancel changes nothing; operations.list takes the
// filter done=true or done=false and nothing else; buckets are never
// soft-deleted (the API has no delete), so showDeleted changes nothing.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The names Cloud Trace's storage has.
const (
	TraceBucketID  = "_Trace"
	SpansDatasetID = "Spans"
	AllSpansViewID = "_AllSpans"
	// DefaultTraceBucketLocation is where a system-created _Trace goes
	// without default settings (UNVERIFIED (docs re-read 2026-11-21:
	// https://docs.cloud.google.com/trace/docs/storage-overview): "the
	// system selects a supported location").
	DefaultTraceBucketLocation = "us"
)

// ObservabilityBucketLocations are the documented observability bucket
// locations (observability-bucket-locations, read 2026-11-21).
var ObservabilityBucketLocations = []string{
	"eu", "us",
	"africa-south1",
	"northamerica-northeast1", "northamerica-northeast2", "northamerica-south1", "southamerica-east1", "southamerica-west1",
	"us-central1", "us-east1", "us-east4", "us-east5", "us-south1", "us-west1", "us-west2", "us-west3", "us-west4",
	"asia-east1", "asia-east2", "asia-northeast1", "asia-northeast2", "asia-northeast3", "asia-south1", "asia-south2",
	"asia-southeast1", "asia-southeast2", "asia-southeast3", "australia-southeast1", "australia-southeast2",
	"europe-central2", "europe-north1", "europe-north2", "europe-southwest1", "europe-west1", "europe-west2", "europe-west3",
	"europe-west4", "europe-west6", "europe-west8", "europe-west9", "europe-west10", "europe-west12",
	"me-central1", "me-central2", "me-west1",
}

func obsBucketLocation(loc string) bool {
	for _, l := range ObservabilityBucketLocations {
		if l == loc {
			return true
		}
	}
	return false
}

var (
	obsBucketIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]{0,99}$`)
	obsLinkIDRe   = regexp.MustCompile(`^[A-Za-z0-9_]{1,100}$`)
)

// obsAgent is the Observability service account of a resource container.
func obsAgent(p parent) string {
	switch p.coll {
	case collOrgs:
		return "service-org-" + p.id + "@gcp-sa-observability.iam.gserviceaccount.com"
	case collFolders:
		return "service-folder-" + p.id + "@gcp-sa-observability.iam.gserviceaccount.com"
	}
	return "service-" + strconv.FormatInt(resource.ProjectNumber(p.id), 10) + "@gcp-sa-observability.iam.gserviceaccount.com"
}

func hexs(s string) string { return hex.EncodeToString([]byte(s)) }

// --- JSON plumbing --------------------------------------------------------

func writeObsProto(w http.ResponseWriter, m proto.Message) error {
	b, err := protojson.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encoding the response")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	return nil
}

func readObsProto(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		return apierror.InvalidArgument("reading the request body: %v", err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, m); err != nil {
		return apierror.InvalidArgument("invalid JSON payload: %v", err)
	}
	return nil
}

func maskPaths(r *http.Request) []string {
	var out []string
	for _, v := range r.URL.Query()["updateMask"] {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}

func pageArgs(r *http.Request) (int, string, error) {
	size := 0
	if v := r.URL.Query().Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, "", apierror.InvalidArgument("pageSize %q is not a non-negative number", v)
		}
		size = n
	}
	return size, r.URL.Query().Get("pageToken"), nil
}

// --- routing ---------------------------------------------------------------

// serveObsResources serves the settings, operations, locations, buckets,
// datasets, views and links paths, and reports false for any other path.
func (a *Admin) serveObsResources(w http.ResponseWriter, r *http.Request) bool {
	rest0, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	if !ok {
		return false
	}
	parts := strings.Split(rest0, "/")
	if len(parts) < 3 || parts[2] != "locations" {
		return false
	}
	switch parts[0] {
	case collProjects, collFolders, collOrgs:
	default:
		return false
	}
	if len(parts) >= 5 && (parts[4] == "scopes" || parts[4] == "traceScopes") {
		return false
	}
	err := a.routeObs(w, r, parts)
	if err != nil {
		rest.WriteError(w, err)
	}
	return true
}

func (a *Admin) routeObs(w http.ResponseWriter, r *http.Request, parts []string) error {
	p, _, err := cutParent("name", parts[0]+"/"+parts[1])
	if err != nil {
		return err
	}
	if err := a.check(p); err != nil {
		return err
	}
	if len(parts) == 3 {
		if r.Method != http.MethodGet {
			return apierror.NotFound("no method answers %s %s", r.Method, r.URL.Path)
		}
		return a.listObsLocations(w, r, p)
	}
	loc := parts[3]
	n := len(parts)
	verb := ""
	if i := strings.LastIndex(parts[n-1], ":"); i >= 0 {
		verb = parts[n-1][i+1:]
		parts[n-1] = parts[n-1][:i]
	}
	if n == 4 && verb == "" && r.Method == http.MethodGet {
		return a.getObsLocation(w, p, loc)
	}
	if n < 5 {
		return apierror.NotFound("no method answers %s %s", r.Method, r.URL.Path)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch parts[4] {
	case "settings":
		if n != 5 || verb != "" {
			break
		}
		switch r.Method {
		case http.MethodGet:
			st, err := a.obsSettings(p, loc)
			if err != nil {
				return err
			}
			return writeObsProto(w, st)
		case http.MethodPatch:
			op, err := a.updateObsSettings(r, p, loc)
			if err != nil {
				return err
			}
			return writeObsProto(w, op)
		}
	case "operations":
		return a.serveObsOperations(w, r, p, loc, parts[5:], verb)
	case "buckets":
		if !p.isProject() {
			break
		}
		return a.serveObsBuckets(w, r, p, loc, parts[5:], verb)
	}
	return apierror.NotFound("no method answers %s %s", r.Method, r.URL.Path)
}

// --- locations -------------------------------------------------------------

func obsLocation(p parent, loc string) *locationpb.Location {
	return &locationpb.Location{Name: p.String() + "/locations/" + loc, LocationId: loc, DisplayName: loc}
}

func (a *Admin) getObsLocation(w http.ResponseWriter, p parent, loc string) error {
	if loc != "global" && !obsBucketLocation(loc) {
		return apierror.NotFound("location %s/locations/%s does not exist", p, loc)
	}
	return writeObsProto(w, obsLocation(p, loc))
}

func (a *Admin) listObsLocations(w http.ResponseWriter, r *http.Request, p parent) error {
	size, token, err := pageArgs(r)
	if err != nil {
		return err
	}
	ids := append([]string{"global"}, ObservabilityBucketLocations...)
	page, next, err := paging.Page("observability-locations|"+p.String(), ids, token, size)
	if err != nil {
		return apierror.InvalidArgument("pageToken: %v", err)
	}
	out := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		out.Locations = append(out.Locations, obsLocation(p, id))
	}
	return writeObsProto(w, out)
}

// --- settings --------------------------------------------------------------

func obsSettingsKey(p parent, loc string) string {
	return "observability/settings/" + hexs(p.String()) + "/" + hexs(loc)
}

func (a *Admin) checkObsLocation(loc string) error {
	if loc != "global" && !obsBucketLocation(loc) {
		return apierror.InvalidArgument("location %q is not global or a supported observability bucket location", loc)
	}
	return nil
}

// storedObsSettings is p's settings in loc as stored, or empty ones.
func (a *Admin) storedObsSettings(p parent, loc string) (*obspb.Settings, error) {
	st := &obspb.Settings{}
	raw, err := a.db.Get(obsSettingsKey(p, loc))
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, apierror.Internal(err, "reading the settings")
	default:
		if err := protojson.Unmarshal(raw, st); err != nil {
			return nil, apierror.Internal(err, "decoding the settings")
		}
	}
	st.Name = p.String() + "/locations/" + loc + "/settings"
	st.ServiceAccountId = obsAgent(p)
	return st, nil
}

func (a *Admin) obsSettings(p parent, loc string) (*obspb.Settings, error) {
	if err := a.checkObsLocation(loc); err != nil {
		return nil, err
	}
	return a.storedObsSettings(p, loc)
}

// effectiveObsSettings is the default storage location and the default key
// in loc a new system-created bucket of project p gets: p's own settings,
// else its nearest ancestor's that has one. The caller holds a.mu.
func (a *Admin) effectiveObsDefaults(p parent) (location string) {
	for _, q := range append([]parent{p}, a.ancestors(p)...) {
		if st, err := a.storedObsSettings(q, "global"); err == nil && st.GetDefaultStorageLocation() != "" {
			return st.GetDefaultStorageLocation()
		}
	}
	return ""
}

func (a *Admin) effectiveObsKey(p parent, loc string) string {
	for _, q := range append([]parent{p}, a.ancestors(p)...) {
		if st, err := a.storedObsSettings(q, loc); err == nil && st.GetKmsKeyName() != "" {
			return st.GetKmsKeyName()
		}
	}
	return ""
}

func (a *Admin) updateObsSettings(r *http.Request, p parent, loc string) (*longrunningpb.Operation, error) {
	if err := a.checkObsLocation(loc); err != nil {
		return nil, err
	}
	var body obspb.Settings
	if err := readObsProto(r, &body); err != nil {
		return nil, err
	}
	name := p.String() + "/locations/" + loc + "/settings"
	if body.GetName() != "" && body.GetName() != name {
		return nil, apierror.InvalidArgument("settings.name %q is not %s", body.GetName(), name)
	}
	cur, err := a.storedObsSettings(p, loc)
	if err != nil {
		return nil, err
	}
	paths := maskPaths(r)
	if len(paths) == 0 {
		if body.GetDefaultStorageLocation() != "" {
			paths = append(paths, "defaultStorageLocation")
		}
		if body.GetKmsKeyName() != "" {
			paths = append(paths, "kmsKeyName")
		}
	}
	next := proto.Clone(cur).(*obspb.Settings)
	setKey := false
	for _, f := range paths {
		switch f {
		case "defaultStorageLocation", "default_storage_location":
			if l := body.GetDefaultStorageLocation(); l != "" && !obsBucketLocation(l) {
				return nil, apierror.InvalidArgument("defaultStorageLocation %q is not a supported observability bucket location", l)
			}
			next.DefaultStorageLocation = body.GetDefaultStorageLocation()
		case "kmsKeyName", "kms_key_name":
			next.KmsKeyName = body.GetKmsKeyName()
			setKey = true
		case "name", "serviceAccountId", "service_account_id":
			return nil, apierror.InvalidArgument("updateMask path %q is not a field that can be updated", f)
		default:
			return nil, apierror.InvalidArgument("updateMask path %q is not a field of Settings", f)
		}
	}
	if setKey && next.GetKmsKeyName() != "" {
		key := next.GetKmsKeyName()
		kl, ok := cmek.Location(key)
		if !ok {
			return nil, apierror.InvalidArgument("kmsKeyName %q must be projects/[PROJECT_ID]/locations/[LOCATION]/keyRings/[KEYRING]/cryptoKeys/[KEY]", key)
		}
		if kl != loc {
			return nil, apierror.InvalidArgument("kmsKeyName %q is in %s: Cloud KMS keys are regional resources, and a default key is set for its own location, not %s", key, kl, loc)
		}
		def := next.GetDefaultStorageLocation()
		if loc != "global" {
			if g, err := a.storedObsSettings(p, "global"); err == nil {
				def = g.GetDefaultStorageLocation()
			}
		}
		if def != loc {
			return nil, apierror.FailedPrecondition("if you set a default Cloud KMS key for a location, then you must also set that location as the default storage location: the default storage location of %s is %q, not %s", p, def, loc)
		}
		agent := obsAgent(p)
		if err := a.cmekKeys().Usable(r.Context(), p.id, agent, key); err != nil {
			return nil, obsKeyError(agent, err)
		}
	}
	raw, err := protojson.Marshal(&obspb.Settings{DefaultStorageLocation: next.GetDefaultStorageLocation(), KmsKeyName: next.GetKmsKeyName()})
	if err != nil {
		return nil, apierror.Internal(err, "encoding the settings")
	}
	if err := a.db.Put(obsSettingsKey(p, loc), raw); err != nil {
		return nil, apierror.Internal(err, "writing the settings")
	}
	return a.obsOp(p, loc, "update", name, next)
}

func obsKeyError(agent string, err error) error {
	e := cmek.As(err)
	if e == nil {
		return apierror.Internal(err, "Cloud KMS")
	}
	switch e.Kind {
	case cmek.Invalid:
		return apierror.InvalidArgument("kms key %s: %s", e.Key, e.Msg)
	case cmek.PermissionDenied:
		key := e.Key
		if k := cmek.KeyOf(key); k != "" {
			key = k
		}
		return apierror.FailedPrecondition("service account %s must have the Cloud KMS CryptoKey Encrypter/Decrypter role (roles/cloudkms.cryptoKeyEncrypterDecrypter) on the key %s", agent, key)
	}
	return keyError(agent, err)
}

// --- operations ------------------------------------------------------------

func obsOpPrefix(p parent, loc string) string {
	return "observability/operations/" + hexs(p.String()+"/locations/"+loc) + "/"
}

// obsOp is a done operation under p's loc, kept for operations.get and
// list; response nil is google.protobuf.Empty. The caller holds a.mu.
func (a *Admin) obsOp(p parent, loc, verb, target string, response proto.Message) (*longrunningpb.Operation, error) {
	now := timestamppb.New(a.now())
	md, err := anypb.New(&obspb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Internal(err, "operation metadata")
	}
	if response == nil {
		response = &emptypb.Empty{}
	}
	resp, err := anypb.New(response)
	if err != nil {
		return nil, apierror.Internal(err, "operation response")
	}
	a.ops++
	id := fmt.Sprintf("operation-%d-%d", a.now().UnixNano(), a.ops)
	op := &longrunningpb.Operation{Name: p.String() + "/locations/" + loc + "/operations/" + id, Metadata: md, Done: true,
		Result: &longrunningpb.Operation_Response{Response: resp}}
	raw, err := proto.Marshal(op)
	if err != nil {
		return nil, apierror.Internal(err, "encoding the operation")
	}
	if err := a.db.Put(obsOpPrefix(p, loc)+id, raw); err != nil {
		return nil, apierror.Internal(err, "writing the operation")
	}
	return op, nil
}

func (a *Admin) obsOperation(p parent, loc, id string) (*longrunningpb.Operation, error) {
	raw, err := a.db.Get(obsOpPrefix(p, loc) + id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("operation %s/locations/%s/operations/%s does not exist", p, loc, id)
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the operation")
	}
	op := &longrunningpb.Operation{}
	if err := proto.Unmarshal(raw, op); err != nil {
		return nil, apierror.Internal(err, "decoding the operation")
	}
	return op, nil
}

func (a *Admin) serveObsOperations(w http.ResponseWriter, r *http.Request, p parent, loc string, rest0 []string, verb string) error {
	if err := a.checkObsLocation(loc); err != nil {
		return err
	}
	switch {
	case len(rest0) == 0 && verb == "" && r.Method == http.MethodGet:
		return a.listObsOperations(w, r, p, loc)
	case len(rest0) == 1 && verb == "" && r.Method == http.MethodGet:
		op, err := a.obsOperation(p, loc, rest0[0])
		if err != nil {
			return err
		}
		return writeObsProto(w, op)
	case len(rest0) == 1 && verb == "" && r.Method == http.MethodDelete:
		if _, err := a.obsOperation(p, loc, rest0[0]); err != nil {
			return err
		}
		if err := a.db.Delete(obsOpPrefix(p, loc) + rest0[0]); err != nil {
			return apierror.Internal(err, "deleting the operation")
		}
		return writeObsProto(w, &emptypb.Empty{})
	case len(rest0) == 1 && verb == "cancel" && r.Method == http.MethodPost:
		// Every operation is done when it is answered: cancelling one
		// changes nothing.
		if _, err := a.obsOperation(p, loc, rest0[0]); err != nil {
			return err
		}
		return writeObsProto(w, &emptypb.Empty{})
	}
	return apierror.NotFound("no operations method answers %s %s", r.Method, r.URL.Path)
}

func (a *Admin) listObsOperations(w http.ResponseWriter, r *http.Request, p parent, loc string) error {
	size, token, err := pageArgs(r)
	if err != nil {
		return err
	}
	want := -1
	switch f := strings.ReplaceAll(r.URL.Query().Get("filter"), " ", ""); f {
	case "":
	case "done=true", "done:true":
		want = 1
	case "done=false", "done:false":
		want = 0
	default:
		return apierror.InvalidArgument("filter %q: only done=true and done=false are supported", f)
	}
	keys, err := a.db.List(obsOpPrefix(p, loc))
	if err != nil {
		return apierror.Internal(err, "listing operations")
	}
	var ids []string
	for _, k := range keys {
		ids = append(ids, strings.TrimPrefix(k, obsOpPrefix(p, loc)))
	}
	sort.Strings(ids)
	if want == 0 {
		ids = nil // every operation is done
	}
	page, next, err := paging.Page("observability-operations|"+p.String()+"|"+loc, ids, token, size)
	if err != nil {
		return apierror.InvalidArgument("pageToken: %v", err)
	}
	out := &longrunningpb.ListOperationsResponse{NextPageToken: next}
	for _, id := range page {
		if op, err := a.obsOperation(p, loc, id); err == nil {
			out.Operations = append(out.Operations, op)
		}
	}
	return writeObsProto(w, out)
}

// --- buckets ---------------------------------------------------------------

func obsBucketPrefix(project string) string { return "observability/buckets/" + hexs(project) + "/" }

func obsBucketKey(project, loc, id string) string {
	return obsBucketPrefix(project) + hexs(loc) + "/" + hexs(id)
}

func obsBucketName(project, loc, id string) string {
	return "projects/" + project + "/locations/" + loc + "/buckets/" + id
}

// obsBucketRecord is a stored bucket and what the API does not show.
type obsBucketRecord struct {
	Bucket        *obspb.Bucket
	Reprovisioned bool
}

func (a *Admin) putObsBucket(project, loc, id string, rec obsBucketRecord) error {
	b, err := protojson.Marshal(rec.Bucket)
	if err != nil {
		return apierror.Internal(err, "encoding the bucket")
	}
	flag := "0"
	if rec.Reprovisioned {
		flag = "1"
	}
	if err := a.db.Put(obsBucketKey(project, loc, id), append([]byte(flag), b...)); err != nil {
		return apierror.Internal(err, "writing the bucket")
	}
	return nil
}

func (a *Admin) obsBucket(project, loc, id string) (obsBucketRecord, error) {
	raw, err := a.db.Get(obsBucketKey(project, loc, id))
	if errors.Is(err, store.ErrNotFound) {
		return obsBucketRecord{}, apierror.NotFound("bucket %s does not exist", obsBucketName(project, loc, id))
	}
	if err != nil || len(raw) == 0 {
		return obsBucketRecord{}, apierror.Internal(err, "reading the bucket")
	}
	b := &obspb.Bucket{}
	if err := protojson.Unmarshal(raw[1:], b); err != nil {
		return obsBucketRecord{}, apierror.Internal(err, "decoding the bucket")
	}
	return obsBucketRecord{Bucket: b, Reprovisioned: raw[0] == '1'}, nil
}

// obsBuckets are the project's buckets as location/id pairs, sorted.
func (a *Admin) obsBuckets(project string) ([][2]string, error) {
	keys, err := a.db.List(obsBucketPrefix(project))
	if err != nil {
		return nil, apierror.Internal(err, "listing buckets")
	}
	var out [][2]string
	for _, k := range keys {
		parts := strings.Split(strings.TrimPrefix(k, obsBucketPrefix(project)), "/")
		if len(parts) != 2 {
			continue
		}
		l, err1 := hex.DecodeString(parts[0])
		i, err2 := hex.DecodeString(parts[1])
		if err1 == nil && err2 == nil {
			out = append(out, [2]string{string(l), string(i)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out, nil
}

// traceBucketLocation is where the project's _Trace is, or "".
func (a *Admin) traceBucketLocation(project string) string {
	all, _ := a.obsBuckets(project)
	for _, b := range all {
		if b[1] == TraceBucketID {
			return b[0]
		}
	}
	return ""
}

// checkObsBucketKey checks a bucket's key and returns the CMEK settings it
// is stored with. The caller holds a.mu.
func (a *Admin) checkObsBucketKey(ctx context.Context, project, loc, key string) (*obspb.CmekSettings, error) {
	kl, ok := cmek.Location(key)
	if !ok {
		return nil, apierror.InvalidArgument("cmekSettings.kmsKey %q must be projects/[PROJECT_ID]/locations/[LOCATION]/keyRings/[KEYRING]/cryptoKeys/[KEY]", key)
	}
	if kl != loc {
		return nil, apierror.InvalidArgument("cmekSettings.kmsKey %q is in %s: a key can only encrypt data stored in its own location, and the bucket is in %s", key, kl, loc)
	}
	p := projectParent(project)
	agent := obsAgent(p)
	probe, err := a.cmekKeys().Seal(ctx, project, agent, key, nil, []byte(obsBucketName(project, loc, "")))
	if err != nil {
		return nil, obsKeyError(agent, err)
	}
	return &obspb.CmekSettings{KmsKey: key, KmsKeyVersion: probe.KeyVersion, ServiceAccountId: agent}, nil
}

// createObsBucket makes a bucket; the caller holds a.mu.
func (a *Admin) createObsBucket(ctx context.Context, project, loc, id string, in *obspb.Bucket) (*obspb.Bucket, error) {
	if !obsBucketLocation(loc) {
		return nil, apierror.InvalidArgument("location %q is not a supported observability bucket location", loc)
	}
	if !obsBucketIDRe.MatchString(id) && id != TraceBucketID {
		return nil, apierror.InvalidArgument("bucketId %q must be 1-100 letters, digits, - and _, not starting with _ (only the system's %s does)", id, TraceBucketID)
	}
	if _, err := a.obsBucket(project, loc, id); err == nil {
		return nil, apierror.AlreadyExists("bucket %s already exists", obsBucketName(project, loc, id))
	}
	if id == TraceBucketID {
		if l := a.traceBucketLocation(project); l != "" {
			return nil, apierror.AlreadyExists("project %s already has its %s bucket, in %s", project, TraceBucketID, l)
		}
	}
	if in == nil {
		in = &obspb.Bucket{}
	}
	now := timestamppb.New(a.now())
	b := &obspb.Bucket{Name: obsBucketName(project, loc, id), DisplayName: in.GetDisplayName(), Description: in.GetDescription(),
		CreateTime: now, UpdateTime: now}
	key := in.GetCmekSettings().GetKmsKey()
	if key == "" {
		key = a.effectiveObsKey(projectParent(project), loc)
	}
	if key != "" {
		cs, err := a.checkObsBucketKey(ctx, project, loc, key)
		if err != nil {
			return nil, err
		}
		b.CmekSettings = cs
	}
	return b, a.putObsBucket(project, loc, id, obsBucketRecord{Bucket: b})
}

// EnsureTraceBucket makes the project's _Trace bucket if it has none, as
// the system does "in response to receiving trace data": in the default
// storage location of the project's (or its nearest ancestor's) settings,
// with the default key of that location, else in
// DefaultTraceBucketLocation. Cloud Trace calls it when it takes spans.
func (a *Admin) EnsureTraceBucket(ctx context.Context, project string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.traceBucketLocation(project) != "" {
		return nil
	}
	loc := a.effectiveObsDefaults(projectParent(project))
	if loc == "" {
		loc = DefaultTraceBucketLocation
	}
	_, err := a.createObsBucket(ctx, project, loc, TraceBucketID, &obspb.Bucket{})
	return err
}

func (a *Admin) serveObsBuckets(w http.ResponseWriter, r *http.Request, p parent, loc string, rest0 []string, verb string) error {
	project := p.id
	if len(rest0) == 0 {
		switch {
		case verb == "" && r.Method == http.MethodPost:
			var in obspb.Bucket
			if err := readObsProto(r, &in); err != nil {
				return err
			}
			id := r.URL.Query().Get("bucketId")
			if id == "" {
				return apierror.InvalidArgument("bucketId is required")
			}
			b, err := a.createObsBucket(r.Context(), project, loc, id, &in)
			if err != nil {
				return err
			}
			op, err := a.obsOp(p, loc, "create", b.GetName(), b)
			if err != nil {
				return err
			}
			return writeObsProto(w, op)
		case verb == "" && r.Method == http.MethodGet:
			return a.listObsBuckets(w, r, project, loc)
		}
		return apierror.NotFound("no buckets method answers %s %s", r.Method, r.URL.Path)
	}
	id := rest0[0]
	if len(rest0) == 1 {
		rec, err := a.obsBucket(project, loc, id)
		if err != nil {
			return err
		}
		switch {
		case verb == "" && r.Method == http.MethodGet:
			return writeObsProto(w, rec.Bucket)
		case verb == "" && r.Method == http.MethodPatch:
			b, err := a.patchObsBucket(r, project, loc, id, rec)
			if err != nil {
				return err
			}
			op, err := a.obsOp(p, loc, "update", b.GetName(), b)
			if err != nil {
				return err
			}
			return writeObsProto(w, op)
		case verb == "reprovision" && r.Method == http.MethodPost:
			if rec.Reprovisioned {
				return apierror.FailedPrecondition("bucket %s has already been reprovisioned: reprovisioning is allowed only a single time for a bucket", rec.Bucket.GetName())
			}
			rec.Reprovisioned = true
			rec.Bucket.UpdateTime = timestamppb.New(a.now())
			if err := a.putObsBucket(project, loc, id, rec); err != nil {
				return err
			}
			if a.reprovisioned != nil && id == TraceBucketID {
				a.reprovisioned(project) // the data loss: Cloud Trace drops the project's spans
			}
			op, err := a.obsOp(p, loc, "reprovision", rec.Bucket.GetName(), rec.Bucket)
			if err != nil {
				return err
			}
			return writeObsProto(w, op)
		}
		return apierror.NotFound("no buckets method answers %s %s", r.Method, r.URL.Path)
	}
	if _, err := a.obsBucket(project, loc, id); err != nil {
		return err
	}
	if rest0[1] != "datasets" {
		return apierror.NotFound("no method answers %s %s", r.Method, r.URL.Path)
	}
	return a.serveObsDatasets(w, r, p, loc, id, rest0[2:], verb)
}

func (a *Admin) listObsBuckets(w http.ResponseWriter, r *http.Request, project, loc string) error {
	if loc != "-" && !obsBucketLocation(loc) {
		return apierror.InvalidArgument("location %q is not a supported observability bucket location", loc)
	}
	size, token, err := pageArgs(r)
	if err != nil {
		return err
	}
	all, err := a.obsBuckets(project)
	if err != nil {
		return err
	}
	var keys []string
	for _, b := range all {
		if loc == "-" || b[0] == loc {
			keys = append(keys, b[0]+"/"+b[1])
		}
	}
	page, next, err := paging.Page("observability-buckets|"+project+"|"+loc, keys, token, size)
	if err != nil {
		return apierror.InvalidArgument("pageToken: %v", err)
	}
	out := &obspb.ListBucketsResponse{NextPageToken: next}
	for _, k := range page {
		l, id, _ := strings.Cut(k, "/")
		if rec, err := a.obsBucket(project, l, id); err == nil {
			out.Buckets = append(out.Buckets, rec.Bucket)
		}
	}
	return writeObsProto(w, out)
}

func (a *Admin) patchObsBucket(r *http.Request, project, loc, id string, rec obsBucketRecord) (*obspb.Bucket, error) {
	var in obspb.Bucket
	if err := readObsProto(r, &in); err != nil {
		return nil, err
	}
	paths := maskPaths(r)
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("updateMask is required")
	}
	b := rec.Bucket
	for _, f := range paths {
		switch f {
		case "displayName", "display_name":
			b.DisplayName = in.GetDisplayName()
		case "description":
			b.Description = in.GetDescription()
		case "cmekSettings", "cmek_settings", "cmekSettings.kmsKey", "cmek_settings.kms_key":
			if key := in.GetCmekSettings().GetKmsKey(); key == "" {
				b.CmekSettings = nil
			} else {
				cs, err := a.checkObsBucketKey(r.Context(), project, loc, key)
				if err != nil {
					return nil, err
				}
				b.CmekSettings = cs
			}
		default:
			return nil, apierror.InvalidArgument("updateMask path %q is not a field of Bucket that can be updated", f)
		}
	}
	b.UpdateTime = timestamppb.New(a.now())
	return b, a.putObsBucket(project, loc, id, rec)
}

// --- datasets and views ----------------------------------------------------

// obsDatasets are a bucket's datasets: _Trace's Spans, none for another.
func obsDatasets(bucket *obspb.Bucket) []*obspb.Dataset {
	if !strings.HasSuffix(bucket.GetName(), "/buckets/"+TraceBucketID) {
		return nil
	}
	return []*obspb.Dataset{{Name: bucket.GetName() + "/datasets/" + SpansDatasetID, DisplayName: SpansDatasetID,
		Description: "Cloud Trace's spans", CreateTime: bucket.GetCreateTime()}}
}

func obsViews(ds *obspb.Dataset) []*obspb.View {
	return []*obspb.View{{Name: ds.GetName() + "/views/" + AllSpansViewID, DisplayName: AllSpansViewID,
		Description: "All the spans of the dataset", CreateTime: ds.GetCreateTime(), UpdateTime: ds.GetCreateTime()}}
}

func (a *Admin) serveObsDatasets(w http.ResponseWriter, r *http.Request, p parent, loc, bucket string, rest0 []string, verb string) error {
	rec, err := a.obsBucket(p.id, loc, bucket)
	if err != nil {
		return err
	}
	dss := obsDatasets(rec.Bucket)
	if len(rest0) == 0 {
		if r.Method != http.MethodGet || verb != "" {
			return apierror.NotFound("no datasets method answers %s %s", r.Method, r.URL.Path)
		}
		size, token, err := pageArgs(r)
		if err != nil {
			return err
		}
		var ids []string
		for _, d := range dss {
			ids = append(ids, d.GetName())
		}
		page, next, err := paging.Page("observability-datasets|"+rec.Bucket.GetName(), ids, token, size)
		if err != nil {
			return apierror.InvalidArgument("pageToken: %v", err)
		}
		out := &obspb.ListDatasetsResponse{NextPageToken: next}
		for _, d := range dss {
			for _, n := range page {
				if d.GetName() == n {
					out.Datasets = append(out.Datasets, d)
				}
			}
		}
		return writeObsProto(w, out)
	}
	var ds *obspb.Dataset
	for _, d := range dss {
		if d.GetName() == rec.Bucket.GetName()+"/datasets/"+rest0[0] {
			ds = d
		}
	}
	if ds == nil {
		return apierror.NotFound("dataset %s/datasets/%s does not exist", rec.Bucket.GetName(), rest0[0])
	}
	if len(rest0) == 1 {
		if r.Method != http.MethodGet || verb != "" {
			return apierror.NotFound("no datasets method answers %s %s: datasets can't be created, deleted or modified", r.Method, r.URL.Path)
		}
		return writeObsProto(w, ds)
	}
	switch rest0[1] {
	case "views":
		if r.Method != http.MethodGet || verb != "" || len(rest0) > 3 {
			break
		}
		views := obsViews(ds)
		if len(rest0) == 3 {
			for _, v := range views {
				if v.GetName() == ds.GetName()+"/views/"+rest0[2] {
					return writeObsProto(w, v)
				}
			}
			return apierror.NotFound("view %s/views/%s does not exist", ds.GetName(), rest0[2])
		}
		if _, _, err := pageArgs(r); err != nil {
			return err
		}
		return writeObsProto(w, &obspb.ListViewsResponse{Views: views})
	case "links":
		return a.serveObsLinks(w, r, p, loc, ds, rest0[2:], verb)
	}
	return apierror.NotFound("no method answers %s %s", r.Method, r.URL.Path)
}

// --- links -----------------------------------------------------------------

func obsLinkPrefix(dataset string) string { return "observability/links/" + hexs(dataset) + "/" }

func (a *Admin) obsLink(dataset, id string) (*obspb.Link, error) {
	raw, err := a.db.Get(obsLinkPrefix(dataset) + hexs(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierror.NotFound("link %s/links/%s does not exist", dataset, id)
	}
	if err != nil {
		return nil, apierror.Internal(err, "reading the link")
	}
	l := &obspb.Link{}
	if err := protojson.Unmarshal(raw, l); err != nil {
		return nil, apierror.Internal(err, "decoding the link")
	}
	return l, nil
}

func (a *Admin) putObsLink(dataset, id string, l *obspb.Link) error {
	raw, err := protojson.Marshal(l)
	if err != nil {
		return apierror.Internal(err, "encoding the link")
	}
	if err := a.db.Put(obsLinkPrefix(dataset)+hexs(id), raw); err != nil {
		return apierror.Internal(err, "writing the link")
	}
	return nil
}

func (a *Admin) obsLinks(dataset string) ([]*obspb.Link, error) {
	keys, err := a.db.List(obsLinkPrefix(dataset))
	if err != nil {
		return nil, apierror.Internal(err, "listing links")
	}
	sort.Strings(keys)
	var out []*obspb.Link
	for _, k := range keys {
		raw, err := hex.DecodeString(strings.TrimPrefix(k, obsLinkPrefix(dataset)))
		if err != nil {
			continue
		}
		if l, err := a.obsLink(dataset, string(raw)); err == nil {
			out = append(out, l)
		}
	}
	return out, nil
}

// SpanFields is the trace storage schema (docs.cloud.google.com/trace/docs/
// reference/trace-schema), the columns of a linked dataset's spans.
func SpanFields() []Field {
	s := func(n string) Field { return Field{Name: n, Type: "STRING"} }
	i := func(n string) Field { return Field{Name: n, Type: "INTEGER"} }
	t := func(n string) Field { return Field{Name: n, Type: "TIMESTAMP"} }
	j := func(n string) Field { return Field{Name: n, Type: "JSON"} }
	r := func(n string, fs ...Field) Field { return Field{Name: n, Type: "RECORD", Fields: fs} }
	return []Field{
		s("trace_id"), s("span_id"), s("trace_state"), s("parent_span_id"), s("name"), i("kind"),
		t("start_time"), i("start_time_unix_nano"), t("end_time"), i("end_time_unix_nano"),
		t("receive_time"), i("receive_time_unix_nano"), i("duration_unix_nano"),
		j("attributes"), i("dropped_attributes_count"),
		{Name: "events", Type: "RECORD", Repeated: true, Fields: []Field{t("time"), i("time_unix_nano"), s("name"), j("attributes"), i("dropped_attributes_count")}},
		i("dropped_events_count"),
		{Name: "links", Type: "RECORD", Repeated: true, Fields: []Field{s("trace_id"), s("span_id"), s("trace_state"), j("attributes"), i("dropped_attributes_count")}},
		i("dropped_links_count"),
		r("status", i("code"), s("message")),
		r("resource", j("attributes"), i("dropped_attributes_count")),
		r("instrumentation_scope", s("name"), s("version"), j("attributes"), i("dropped_attributes_count")),
		s("resource_schema_link"), s("scope_schema_link"),
		r("apphub", r("application", s("container"), s("location"), s("id")),
			r("service", s("id"), s("environment_type"), s("criticality_type")),
			r("workload", s("id"), s("environment_type"), s("criticality_type"))),
		// Every column the views select is the schema's; the table also
		// lists the log views column the linked-dataset writer expects.
		{Name: LinkedViewsColumn, Type: "STRING", Repeated: true},
	}
}

func spanViewQuery(project, dataset string) string {
	var cols []string
	for _, f := range SpanFields() {
		if f.Name != LinkedViewsColumn {
			cols = append(cols, "`"+f.Name+"`")
		}
	}
	return fmt.Sprintf("SELECT %s FROM `%s.%s.%s`", strings.Join(cols, ", "), project, dataset, LinkedEntriesTable)
}

func (a *Admin) serveObsLinks(w http.ResponseWriter, r *http.Request, p parent, loc string, ds *obspb.Dataset, rest0 []string, verb string) error {
	project := p.id
	dataset := ds.GetName()
	if len(rest0) == 0 {
		switch {
		case verb == "" && r.Method == http.MethodGet:
			size, token, err := pageArgs(r)
			if err != nil {
				return err
			}
			ls, err := a.obsLinks(dataset)
			if err != nil {
				return err
			}
			var names []string
			for _, l := range ls {
				names = append(names, l.GetName())
			}
			page, next, err := paging.Page("observability-links|"+dataset, names, token, size)
			if err != nil {
				return apierror.InvalidArgument("pageToken: %v", err)
			}
			out := &obspb.ListLinksResponse{NextPageToken: next}
			for _, l := range ls {
				for _, n := range page {
					if n == l.GetName() {
						out.Links = append(out.Links, l)
					}
				}
			}
			return writeObsProto(w, out)
		case verb == "" && r.Method == http.MethodPost:
			id := r.URL.Query().Get("linkId")
			if !obsLinkIDRe.MatchString(id) {
				return apierror.InvalidArgument("linkId %q is the name of the linked BigQuery dataset: it must be limited to 100 characters and can include only letters, digits, and underscores", id)
			}
			var in obspb.Link
			if err := readObsProto(r, &in); err != nil {
				return err
			}
			if _, err := a.obsLink(dataset, id); err == nil {
				return apierror.AlreadyExists("link %s/links/%s already exists", dataset, id)
			}
			if ls, err := a.obsLinks(dataset); err != nil {
				return err
			} else if len(ls) > 0 {
				return apierror.FailedPrecondition("dataset %s already has the link %s: each observability dataset supports one linked BigQuery dataset", dataset, ls[0].GetName())
			}
			if a.linked == nil {
				return apierror.FailedPrecondition("a link makes a BigQuery dataset, and BigQuery is not served on this instance: start it with bigquery among --services")
			}
			ctx := context.WithoutCancel(r.Context())
			if err := a.linked.Create(ctx, project, id, "Linked dataset of "+dataset, SpanFields(),
				[]LinkedView{{ID: AllSpansViewID, Query: spanViewQuery(project, id)}}); err != nil {
				return linkError(project, id, err)
			}
			l := &obspb.Link{Name: dataset + "/links/" + id, DisplayName: in.GetDisplayName(), Description: in.GetDescription(),
				CreateTime: timestamppb.New(a.now())}
			if err := a.putObsLink(dataset, id, l); err != nil {
				return err
			}
			op, err := a.obsOp(p, loc, "create", l.GetName(), l)
			if err != nil {
				return err
			}
			return writeObsProto(w, op)
		}
		return apierror.NotFound("no links method answers %s %s", r.Method, r.URL.Path)
	}
	if len(rest0) != 1 || verb != "" {
		return apierror.NotFound("no links method answers %s %s", r.Method, r.URL.Path)
	}
	id := rest0[0]
	l, err := a.obsLink(dataset, id)
	if err != nil {
		return err
	}
	switch r.Method {
	case http.MethodGet:
		return writeObsProto(w, l)
	case http.MethodPatch:
		var in obspb.Link
		if err := readObsProto(r, &in); err != nil {
			return err
		}
		paths := maskPaths(r)
		if len(paths) == 0 {
			return apierror.InvalidArgument("updateMask is required")
		}
		for _, f := range paths {
			switch f {
			case "displayName", "display_name":
				l.DisplayName = in.GetDisplayName()
			case "description":
				l.Description = in.GetDescription()
			default:
				return apierror.InvalidArgument("updateMask path %q is not a field of Link that can be updated", f)
			}
		}
		if err := a.putObsLink(dataset, id, l); err != nil {
			return err
		}
		op, err := a.obsOp(p, loc, "update", l.GetName(), l)
		if err != nil {
			return err
		}
		return writeObsProto(w, op)
	case http.MethodDelete:
		if a.linked != nil {
			if err := a.linked.Delete(context.WithoutCancel(r.Context()), project, id); err != nil {
				return linkError(project, id, err)
			}
		}
		if err := a.db.Delete(obsLinkPrefix(dataset) + hexs(id)); err != nil {
			return apierror.Internal(err, "deleting the link")
		}
		op, err := a.obsOp(p, loc, "delete", l.GetName(), nil)
		if err != nil {
			return err
		}
		return writeObsProto(w, op)
	}
	return apierror.NotFound("no links method answers %s %s", r.Method, r.URL.Path)
}

// AppendSpans streams rows of the trace storage schema (SpanFields) into
// the linked BigQuery dataset of the project's _Trace Spans dataset, if it
// has one; insertIDs dedupe them. Cloud Trace calls it as it takes spans.
func (a *Admin) AppendSpans(ctx context.Context, project string, rows []map[string]any, insertIDs []string) error {
	a.mu.Lock()
	loc := a.traceBucketLocation(project)
	var ls []*obspb.Link
	if loc != "" {
		ls, _ = a.obsLinks(obsBucketName(project, loc, TraceBucketID) + "/datasets/" + SpansDatasetID)
	}
	linked := a.linked
	a.mu.Unlock()
	if linked == nil {
		return nil
	}
	for _, l := range ls {
		id := l.GetName()[strings.LastIndex(l.GetName(), "/")+1:]
		if err := appendRows(ctx, linked, project, id, rows, insertIDs); err != nil {
			return err
		}
	}
	return nil
}

// OnTraceBucketReprovision sets what reprovisioning a project's _Trace
// bucket does to the data it holds: Cloud Trace drops the project's spans.
func (a *Admin) OnTraceBucketReprovision(f func(project string)) {
	a.mu.Lock()
	a.reprovisioned = f
	a.mu.Unlock()
}

// ObservabilityMethodStatuses is every method of the Observability API v1
// discovery document, by discovery ID, and whether it is served: all of
// them are, on the Logging port (scopes #2020, trace scopes #2108, the rest
// #2117).
func ObservabilityMethodStatuses() map[string]bool {
	out := map[string]bool{}
	for _, coll := range []string{"projects", "folders", "organizations"} {
		p := "observability." + coll + ".locations."
		for _, m := range []string{"get", "list", "getSettings", "updateSettings", "operations.cancel", "operations.delete", "operations.get", "operations.list"} {
			out[p+m] = true
		}
	}
	p := "observability.projects.locations."
	for _, m := range []string{"buckets.create", "buckets.get", "buckets.list", "buckets.patch", "buckets.reprovision",
		"buckets.datasets.get", "buckets.datasets.list",
		"buckets.datasets.links.create", "buckets.datasets.links.delete", "buckets.datasets.links.get", "buckets.datasets.links.list", "buckets.datasets.links.patch",
		"buckets.datasets.views.get", "buckets.datasets.views.list",
		"scopes.get", "scopes.patch",
		"traceScopes.create", "traceScopes.delete", "traceScopes.get", "traceScopes.list", "traceScopes.patch"} {
		out[p+m] = true
	}
	return out
}
