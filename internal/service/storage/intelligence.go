package storage

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Storage Intelligence configurations (#1519): google.storage.control.v2's
// IntelligenceConfig, the singleton of a project, a folder and an
// organization (docs.cloud.google.com/storage/docs/storage-intelligence/
// overview and .../configure-manage-storage-intelligence), on the
// StorageControl service's gRPC (grpccontrol.go) and its REST mapping:
//
//	GET   /v2/{projects|folders|organizations}/{id}/locations/global/intelligenceConfig
//	PATCH /v2/{...}/intelligenceConfig?updateMask=&requestId=   body: IntelligenceConfig
//
// gcloud storage intelligence-configs calls the same paths, under the
// storage endpoint override gcloud-setup writes, so they are served under
// /storage/v1/v2/ as well.
//
// Stored, nothing acts on it: Storage Intelligence's features (Storage
// Insights datasets, storage batch operations, Gemini Cloud Assist) are
// other APIs CloudBurrow does not serve, and nothing on a bucket or object
// is observably different with it enabled. What is kept is what Google
// documents of the resource:
//
//   - a resource with no configuration reads as edition INHERIT, "the
//     default edition when there is no IntelligenceConfig setup";
//   - updates set edition_config and filter by update mask (edition_config,
//     filter, or a filter.* sub-field as gcloud sends); an omitted mask
//     sets both. DISABLED and INHERIT take no filter ("Filters are not
//     applicable"), and one sent with them is refused;
//   - TRIAL sets trial_config.expire_time 30 days on; once it passes the
//     configuration is STANDARD ("At the end of the trial period, the
//     IntelligenceConfig resource is upgraded to STANDARD edition");
//   - effective_intelligence_config names the configuration that applies:
//     the resource's own when its edition is set, its parent's when it
//     inherits, and effective edition NONE when nothing above sets one.
//
// A project's name is returned with its number, the documented form
// (projects/{project_number}/...); a project ID is accepted as the
// provider and gcloud send it. UNVERIFIED: the statuses and messages of the
// refusals, that a second trial is refused, the effective edition of
// ESSENTIALS (STANDARD here), and the parent: the storage server does not
// see the resource hierarchy, so a project or a folder that inherits
// inherits the local organization's configuration (organizations/
// 100000000001, resourcemanager.OrganizationName), never a folder's.

const intelligencePrefix = "intelligence/"

// localOrganization is the instance's one organization, the parent this
// resolves inheritance to.
const localOrganization = "100000000001"

// trialPeriod is how long a TRIAL edition lasts.
const trialPeriod = 30 * 24 * time.Hour

// intelligenceRecord is a stored configuration.
type intelligenceRecord struct {
	Edition     string    `json:"edition"`
	Filter      []byte    `json:"filter,omitempty"` // protojson of IntelligenceConfig_Filter
	Updated     time.Time `json:"updated"`
	TrialExpire time.Time `json:"trialExpire,omitempty"`
}

// intelligenceTarget is a parsed name: kind is projects, folders or
// organizations, and id is a project number, a folder or an organization ID.
type intelligenceTarget struct{ kind, id string }

func (t intelligenceTarget) name() string {
	return t.kind + "/" + t.id + "/locations/global/intelligenceConfig"
}

func (t intelligenceTarget) key() string { return intelligencePrefix + t.kind + "/" + t.id }

var digits = regexp.MustCompile(`^[0-9]+$`)

// parseIntelligenceName parses {kind}/{id}/locations/{location}/intelligenceConfig.
// want, when set, is the kind the method serves.
func parseIntelligenceName(name, want string) (intelligenceTarget, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 5 || parts[2] != "locations" || parts[4] != "intelligenceConfig" || parts[1] == "" {
		return intelligenceTarget{}, badRequest("Invalid argument: name %q is not {projects|folders|organizations}/{id}/locations/global/intelligenceConfig", name)
	}
	t := intelligenceTarget{kind: parts[0], id: parts[1]}
	if want != "" && t.kind != want {
		return t, badRequest("Invalid argument: name %q is not %s/{id}/locations/global/intelligenceConfig", name, want)
	}
	if parts[3] != "global" {
		return t, badRequest("Invalid argument: the location of an IntelligenceConfig is global, not %q", parts[3])
	}
	switch t.kind {
	case "projects":
		if !digits.MatchString(t.id) {
			if !resource.ValidProjectID(t.id) {
				return t, badRequest("Invalid argument: %q is not a project ID or number", t.id)
			}
			t.id = strconv.FormatInt(resource.ProjectNumber(t.id), 10)
		}
	case "folders", "organizations":
		if !digits.MatchString(t.id) {
			return t, badRequest("Invalid argument: %q is not a %s ID", t.id, strings.TrimSuffix(t.kind, "s"))
		}
	default:
		return t, badRequest("Invalid argument: name %q is not {projects|folders|organizations}/{id}/locations/global/intelligenceConfig", name)
	}
	return t, nil
}

func getIntelligence(tx Tx, t intelligenceTarget) (intelligenceRecord, bool, error) {
	var rec intelligenceRecord
	raw, ok := tx.Get(t.key())
	if !ok {
		return rec, false, nil
	}
	return rec, true, json.Unmarshal(raw, &rec)
}

// edition is the record's edition at now: a trial that has ended is
// STANDARD.
func (rec intelligenceRecord) edition(now time.Time) controlpb.IntelligenceConfig_EditionConfig {
	e := controlpb.IntelligenceConfig_EditionConfig(controlpb.IntelligenceConfig_EditionConfig_value[rec.Edition])
	if e == controlpb.IntelligenceConfig_EDITION_CONFIG_UNSPECIFIED {
		return controlpb.IntelligenceConfig_INHERIT
	}
	if e == controlpb.IntelligenceConfig_TRIAL && !rec.TrialExpire.IsZero() && !now.Before(rec.TrialExpire) {
		return controlpb.IntelligenceConfig_STANDARD
	}
	return e
}

// effective is the configuration that applies to t.
func effectiveIntelligence(tx Tx, t intelligenceTarget, now time.Time) (*controlpb.IntelligenceConfig_EffectiveIntelligenceConfig, error) {
	for {
		rec, _, err := getIntelligence(tx, t)
		if err != nil {
			return nil, err
		}
		switch rec.edition(now) {
		case controlpb.IntelligenceConfig_DISABLED:
			return &controlpb.IntelligenceConfig_EffectiveIntelligenceConfig{
				EffectiveEdition: controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_NONE, IntelligenceConfig: t.name()}, nil
		case controlpb.IntelligenceConfig_INHERIT:
			if t.kind == "organizations" {
				return &controlpb.IntelligenceConfig_EffectiveIntelligenceConfig{
					EffectiveEdition: controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_NONE}, nil
			}
			t = intelligenceTarget{kind: "organizations", id: localOrganization}
		default:
			return &controlpb.IntelligenceConfig_EffectiveIntelligenceConfig{
				EffectiveEdition: controlpb.IntelligenceConfig_EffectiveIntelligenceConfig_STANDARD, IntelligenceConfig: t.name()}, nil
		}
	}
}

// intelligenceProto is t's configuration as Google returns it.
func intelligenceProto(tx Tx, t intelligenceTarget, now time.Time) (*controlpb.IntelligenceConfig, error) {
	rec, found, err := getIntelligence(tx, t)
	if err != nil {
		return nil, err
	}
	out := &controlpb.IntelligenceConfig{Name: t.name(), EditionConfig: rec.edition(now)}
	if found {
		out.UpdateTime = timestamppb.New(rec.Updated)
		if len(rec.Filter) > 0 {
			f := &controlpb.IntelligenceConfig_Filter{}
			if err := protojson.Unmarshal(rec.Filter, f); err != nil {
				return nil, err
			}
			out.Filter = f
		}
		if out.EditionConfig == controlpb.IntelligenceConfig_TRIAL {
			out.TrialConfig = &controlpb.IntelligenceConfig_TrialConfig{ExpireTime: timestamppb.New(rec.TrialExpire)}
		}
	}
	if out.EffectiveIntelligenceConfig, err = effectiveIntelligence(tx, t, now); err != nil {
		return nil, err
	}
	return out, nil
}

// GetIntelligenceConfig returns the configuration name names; kind, when
// set, is the kind the caller's method serves.
func (s *Server) GetIntelligenceConfig(name, kind string) (*controlpb.IntelligenceConfig, error) {
	t, err := parseIntelligenceName(name, kind)
	if err != nil {
		return nil, err
	}
	var out *controlpb.IntelligenceConfig
	err = s.meta.View(func(tx Tx) error {
		out, err = intelligenceProto(tx, t, s.now())
		return err
	})
	return out, err
}

func filterEmpty(f *controlpb.IntelligenceConfig_Filter) bool {
	return f == nil || (f.CloudStorageLocations == nil && f.CloudStorageBuckets == nil)
}

func checkFilter(f *controlpb.IntelligenceConfig_Filter) error {
	if f == nil {
		return nil
	}
	for _, l := range append(f.GetIncludedCloudStorageLocations().GetLocations(), f.GetExcludedCloudStorageLocations().GetLocations()...) {
		if strings.TrimSpace(l) == "" {
			return badRequest("Invalid argument: a filter location is empty")
		}
	}
	for _, re := range append(f.GetIncludedCloudStorageBuckets().GetBucketIdRegexes(), f.GetExcludedCloudStorageBuckets().GetBucketIdRegexes()...) {
		if _, err := regexp.Compile(re); err != nil {
			return badRequest("Invalid argument: bucket ID regex %q: %v", re, err)
		}
	}
	return nil
}

// UpdateIntelligenceConfig sets the fields paths name of in's configuration.
func (s *Server) UpdateIntelligenceConfig(in *controlpb.IntelligenceConfig, paths []string, kind string) (*controlpb.IntelligenceConfig, error) {
	t, err := parseIntelligenceName(in.GetName(), kind)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		paths = []string{"edition_config", "filter"}
	}
	now := s.now()
	var out *controlpb.IntelligenceConfig
	err = s.meta.Update(func(tx Tx) error {
		rec, _, err := getIntelligence(tx, t)
		if err != nil {
			return err
		}
		edition := rec.edition(now)
		filter := &controlpb.IntelligenceConfig_Filter{}
		if len(rec.Filter) > 0 {
			if err := protojson.Unmarshal(rec.Filter, filter); err != nil {
				return err
			}
		}
		inF := in.GetFilter()
		for _, p := range paths {
			switch snake(strings.TrimSpace(p)) {
			case "*":
				edition = in.GetEditionConfig()
				filter = cloneFilter(inF)
			case "edition_config":
				edition = in.GetEditionConfig()
			case "filter":
				filter = cloneFilter(inF)
			case "filter.included_cloud_storage_locations", "filter.excluded_cloud_storage_locations":
				filter.CloudStorageLocations = inF.GetCloudStorageLocations()
			case "filter.included_cloud_storage_buckets", "filter.excluded_cloud_storage_buckets":
				filter.CloudStorageBuckets = inF.GetCloudStorageBuckets()
			default:
				return badRequest("Invalid argument: update_mask path %q is not edition_config or filter", p)
			}
		}
		switch edition {
		case controlpb.IntelligenceConfig_EDITION_CONFIG_UNSPECIFIED:
			return badRequest("Invalid argument: edition_config is required: INHERIT, DISABLED, STANDARD, TRIAL or ESSENTIALS")
		case controlpb.IntelligenceConfig_INHERIT, controlpb.IntelligenceConfig_DISABLED:
			if !filterEmpty(filter) {
				return badRequest("Invalid argument: a filter does not apply to edition %s", edition)
			}
		case controlpb.IntelligenceConfig_TRIAL:
			if rec.TrialExpire.IsZero() {
				rec.TrialExpire = now.Add(trialPeriod).UTC()
			} else if !now.Before(rec.TrialExpire) {
				return errorf(http.StatusBadRequest, "failedPrecondition", "The Storage Intelligence trial of %s ended at %s", t.name(), rfc3339(rec.TrialExpire))
			}
		}
		if err := checkFilter(filter); err != nil {
			return err
		}
		rec.Edition = edition.String()
		rec.Filter = nil
		if !filterEmpty(filter) {
			if rec.Filter, err = protojson.Marshal(filter); err != nil {
				return err
			}
		}
		rec.Updated = now.UTC()
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		tx.Put(t.key(), raw)
		out, err = intelligenceProto(tx, t, now)
		return err
	})
	return out, err
}

func cloneFilter(f *controlpb.IntelligenceConfig_Filter) *controlpb.IntelligenceConfig_Filter {
	if f == nil {
		return &controlpb.IntelligenceConfig_Filter{}
	}
	return proto.Clone(f).(*controlpb.IntelligenceConfig_Filter)
}

// snake is a camelCase mask path in snake_case, as REST callers may send.
func snake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isIntelligencePath says whether the /v2/ path rest is an
// IntelligenceConfig's.
func isIntelligencePath(rest string) bool {
	return strings.HasSuffix(rest, "/intelligenceConfig") &&
		(strings.HasPrefix(rest, "projects/") || strings.HasPrefix(rest, "folders/") || strings.HasPrefix(rest, "organizations/"))
}

// serveIntelligence serves the REST mapping of the IntelligenceConfig
// methods; name is the path after /v2/.
func (s *Server) serveIntelligence(w http.ResponseWriter, r *http.Request, name string) {
	mo := protojson.MarshalOptions{}
	switch r.Method {
	case http.MethodGet:
		out, err := s.GetIntelligenceConfig(name, "")
		if err != nil {
			writeControlError(w, err)
			return
		}
		writeControlProto(w, mo, out)
	case http.MethodPatch:
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeControlError(w, badRequest("Invalid argument: %v", err))
			return
		}
		in := &controlpb.IntelligenceConfig{}
		if len(raw) > 0 {
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, in); err != nil {
				writeControlError(w, badRequest("Invalid argument: %v", err))
				return
			}
		}
		in.Name = name
		var paths []string
		if m := r.URL.Query().Get("updateMask"); m != "" {
			paths = strings.Split(m, ",")
		}
		out, err := s.UpdateIntelligenceConfig(in, paths, "")
		if err != nil {
			writeControlError(w, err)
			return
		}
		writeControlProto(w, mo, out)
	default:
		writeControlError(w, errorf(http.StatusNotImplemented, "notImplemented", "%s %s is not implemented", r.Method, r.URL.Path))
	}
}

func writeControlProto(w http.ResponseWriter, mo protojson.MarshalOptions, m proto.Message) {
	b, err := mo.Marshal(m)
	if err != nil {
		writeControlError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ControlGRPCMethods are the google.storage.control.v2.StorageControl
// methods tools/coverage lists, the IntelligenceConfig ones (the
// StorageControl service's other methods are named on the JSON API's rows).
func ControlGRPCMethods() []string {
	var out []string
	for _, kind := range []string{"Project", "Folder", "Organization"} {
		for _, verb := range []string{"Get", "Update"} {
			out = append(out, "google.storage.control.v2.StorageControl/"+verb+kind+"IntelligenceConfig")
		}
	}
	sort.Strings(out)
	return out
}
