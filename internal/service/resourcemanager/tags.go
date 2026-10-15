package resourcemanager

// Liens, TagKeys, TagValues, TagBindings, TagHolds and EffectiveTags (#1295).
//
// Everything here is stored, not enforced, with one exception: a lien whose
// restrictions include resourcemanager.projects.delete makes DeleteProject
// FAILED_PRECONDITION, as on Google. That is a data rule, like a folder that
// must be empty before it is deleted, not an authorization decision: it
// depends on no principal and grants nothing, so it is consistent with
// ADR-0006, which keeps policies stored and never enforced. Tags are
// referenced by no IAM condition or organization policy here, so a binding
// changes nothing but what ListTagBindings and ListEffectiveTags return.
//
// The referential rules Google applies are kept: a tag key with values, or a
// value with bindings or holds, is not deleted; short names are unique under
// their parent; a resource carries at most one value of each key.
//
// Tag records are the API's own messages, stored as protojson.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	lienPrefix       = "resourcemanager/liens/"
	tagKeyPrefix     = "resourcemanager/tagkeys/"
	tagValuePrefix   = "resourcemanager/tagvalues/"
	tagBindingPrefix = "resourcemanager/tagbindings/"
	tagHoldPrefix    = "resourcemanager/tagholds/"

	// ProjectDeleteRestriction is the lien restriction DeleteProject honours.
	ProjectDeleteRestriction = "resourcemanager.projects.delete"

	// CRMPrefix starts a Resource Manager resource's full resource name, as
	// a tag binding's parent names it.
	CRMPrefix = "//cloudresourcemanager.googleapis.com/"
)

var (
	tagShortNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,254}[A-Za-z0-9])?$`)
	lienIDPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

func pmarshal(m proto.Message) ([]byte, error) { return protojson.Marshal(m) }

func (r *Registry) putProto(key string, m proto.Message) error {
	b, err := pmarshal(m)
	if err != nil {
		return apierror.Internal(err, "encode %s", key)
	}
	if err := r.st.Put(key, b); err != nil {
		return apierror.Internal(err, "store %s", key)
	}
	return nil
}

func (r *Registry) getProto(key string, m proto.Message) error {
	b, err := r.st.Get(key)
	if err != nil {
		return err
	}
	if err := protojson.Unmarshal(b, m); err != nil {
		return apierror.Internal(err, "decode %s", key)
	}
	return nil
}

// listProtos decodes every record under prefix with mk.
func listProtos[T proto.Message](r *Registry, prefix string, mk func() T) ([]T, error) {
	keys, err := r.st.List(prefix)
	if err != nil {
		return nil, apierror.Internal(err, "list %s", prefix)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(keys))
	for _, k := range keys {
		m := mk()
		if err := r.getProto(k, m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func protoEtag(m proto.Message) string {
	c := proto.Clone(m)
	// The etag is a digest of everything but itself.
	if f := c.ProtoReflect().Descriptor().Fields().ByName("etag"); f != nil {
		c.ProtoReflect().Clear(f)
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return policyEtag(b)
}

func (r *Registry) nowTS() *timestamppb.Timestamp {
	return timestamppb.New(r.now().UTC().Truncate(time.Second))
}

// resolveProjectLocked finds the project "projects/{id}" or
// "projects/{number}" names. The caller holds r.mu.
func (r *Registry) resolveProjectLocked(name string) (Project, error) {
	id, ok := strings.CutPrefix(name, "projects/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return Project{}, apierror.InvalidArgument("%q must be projects/{project_number} or projects/{project_id}", name)
	}
	if n, err := strconv.ParseInt(id, 10, 64); err == nil {
		keys, err := r.st.List(keyPrefix)
		if err != nil {
			return Project{}, apierror.Internal(err, "list projects")
		}
		for _, k := range keys {
			pid := strings.TrimPrefix(k, keyPrefix)
			if projectNumber(pid) == n {
				return r.load(pid)
			}
		}
		return Project{}, apierror.NotFound("project %s not found", name)
	}
	return r.load(id)
}

// namesProject reports whether "projects/X" names the project id.
func namesProject(parent, id string) bool {
	x, ok := strings.CutPrefix(parent, "projects/")
	return ok && (x == id || x == strconv.FormatInt(projectNumber(id), 10))
}

// --- Liens ---------------------------------------------------------------

// Lien is one lien, in the JSON shape v1 and v3 share. Liens have no gRPC
// surface: Google publishes them over JSON only.
type Lien struct {
	Name         string   `json:"name,omitempty"`
	Parent       string   `json:"parent,omitempty"`
	Restrictions []string `json:"restrictions,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	Origin       string   `json:"origin,omitempty"`
	CreateTime   string   `json:"createTime,omitempty"`
}

func (r *Registry) putLien(id string, l Lien) error {
	b, err := json.Marshal(l)
	if err != nil {
		return apierror.Internal(err, "encode lien")
	}
	if err := r.st.Put(lienPrefix+id, b); err != nil {
		return apierror.Internal(err, "store lien")
	}
	return nil
}

func (r *Registry) getLien(id string) (Lien, error) {
	var l Lien
	b, err := r.st.Get(lienPrefix + id)
	if err != nil {
		return l, apierror.NotFound("lien liens/%s not found", id)
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, apierror.Internal(err, "decode lien")
	}
	return l, nil
}

// CreateLien places a lien on a project.
func (r *Registry) CreateLien(in Lien) (Lien, error) {
	if len(in.Restrictions) == 0 {
		return Lien{}, apierror.InvalidArgument("lien restrictions are required, such as %q", ProjectDeleteRestriction)
	}
	for _, s := range in.Restrictions {
		if !strings.Contains(s, ".") {
			return Lien{}, apierror.InvalidArgument("lien restriction %q must be a permission, such as %q", s, ProjectDeleteRestriction)
		}
	}
	if in.Reason == "" || len(in.Reason) > 200 {
		return Lien{}, apierror.InvalidArgument("lien reason is required, at most 200 characters")
	}
	if in.Origin == "" || len(in.Origin) > 200 {
		return Lien{}, apierror.InvalidArgument("lien origin is required, at most 200 characters")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.resolveProjectLocked(in.Parent)
	if err != nil {
		return Lien{}, err
	}
	if p.State != StateActive {
		return Lien{}, apierror.FailedPrecondition("project %s is pending deletion", p.ProjectID)
	}
	// Google's lien IDs look like p{number}-l{number}.
	id := "p" + strconv.FormatInt(projectNumber(p.ProjectID), 10) + "-l" + randomFolderNumber()
	for {
		if _, err := r.st.Get(lienPrefix + id); err != nil {
			break
		}
		id = "p" + strconv.FormatInt(projectNumber(p.ProjectID), 10) + "-l" + randomFolderNumber()
	}
	l := in
	l.Restrictions = append([]string(nil), in.Restrictions...)
	l.Name, l.CreateTime = "liens/"+id, r.now().UTC().Format(time.RFC3339)
	return l, r.putLien(id, l)
}

func lienID(name string) (string, error) {
	id := strings.TrimPrefix(name, "liens/")
	if !lienIDPattern.MatchString(id) {
		return "", apierror.InvalidArgument("lien name %q must be liens/{id}", name)
	}
	return id, nil
}

// GetLien returns one lien.
func (r *Registry) GetLien(name string) (Lien, error) {
	id, err := lienID(name)
	if err != nil {
		return Lien{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getLien(id)
}

// Liens lists the liens on a project, oldest first.
func (r *Registry) Liens(parent string) ([]Lien, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.resolveProjectLocked(parent)
	if err != nil {
		return nil, err
	}
	return r.liensOfLocked(p.ProjectID)
}

func (r *Registry) liensOfLocked(projectID string) ([]Lien, error) {
	keys, err := r.st.List(lienPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list liens")
	}
	var out []Lien
	for _, k := range keys {
		l, err := r.getLien(strings.TrimPrefix(k, lienPrefix))
		if err == nil && namesProject(l.Parent, projectID) {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreateTime != out[j].CreateTime {
			return out[i].CreateTime < out[j].CreateTime
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// DeleteLien removes a lien.
func (r *Registry) DeleteLien(name string) error {
	id, err := lienID(name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.getLien(id); err != nil {
		return err
	}
	if err := r.st.Delete(lienPrefix + id); err != nil {
		return apierror.Internal(err, "delete lien")
	}
	return nil
}

// checkDeleteLiensLocked refuses a project delete a lien restricts. The
// caller holds r.mu.
func (r *Registry) checkDeleteLiensLocked(projectID string) error {
	liens, err := r.liensOfLocked(projectID)
	if err != nil {
		return err
	}
	for _, l := range liens {
		for _, s := range l.Restrictions {
			if s == ProjectDeleteRestriction {
				return apierror.FailedPrecondition("a lien to prevent deletion was placed on project %s by %s (%s, reason: %s); "+
					"remove the lien to allow deletion", projectID, l.Origin, l.Name, l.Reason)
			}
		}
	}
	return nil
}

// --- Tag keys and values ----------------------------------------------------

func validateShortName(kind, s string) error {
	if !tagShortNamePattern.MatchString(s) {
		return apierror.InvalidArgument("%s short_name %q must be 1 to 256 characters, starting and ending with a letter or digit, "+
			"with letters, digits, dots, hyphens and underscores between", kind, s)
	}
	return nil
}

func validateDescription(s string) error {
	if len(s) > 256 {
		return apierror.InvalidArgument("description must be at most 256 characters")
	}
	return nil
}

func numericID(prefix, name string) (string, error) {
	id, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", apierror.InvalidArgument("%q must be %s{id}", name, prefix)
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", apierror.InvalidArgument("%q must be %s{id}", name, prefix)
	}
	return id, nil
}

// tagNamespaceLocked is the namespace a tag key's parent gives it: the
// organization's number, or the project's ID. The caller holds r.mu.
func (r *Registry) tagNamespaceLocked(parent string) (string, error) {
	switch {
	case parent == "":
		return "", apierror.InvalidArgument("tag key parent is required: %s or projects/{project_number}", OrganizationName)
	case strings.HasPrefix(parent, "organizations/"):
		if err := checkOrg(parent); err != nil {
			return "", err
		}
		return OrganizationNumber, nil
	case strings.HasPrefix(parent, "projects/"):
		p, err := r.resolveProjectLocked(parent)
		if err != nil {
			return "", err
		}
		if p.State != StateActive {
			return "", apierror.FailedPrecondition("project %s is pending deletion", p.ProjectID)
		}
		return p.ProjectID, nil
	}
	return "", apierror.InvalidArgument("tag key parent %q must be %s or projects/{project_number}", parent, OrganizationName)
}

func (r *Registry) newID(prefix string) string {
	for {
		id := randomFolderNumber()
		if _, err := r.st.Get(prefix + id); err != nil {
			return id
		}
	}
}

// CreateTagKey adds a tag key under the organization or a project. With
// validateOnly it checks and stores nothing.
func (r *Registry) CreateTagKey(in *rmpb.TagKey, validateOnly bool) (*rmpb.TagKey, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("tag_key is required")
	}
	if err := validateShortName("tag key", in.GetShortName()); err != nil {
		return nil, err
	}
	if err := validateDescription(in.GetDescription()); err != nil {
		return nil, err
	}
	if in.GetPurpose() == rmpb.Purpose_GCE_FIREWALL && in.GetPurposeData()["network"] == "" {
		return nil, apierror.InvalidArgument("purpose GCE_FIREWALL requires purpose_data network")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, err := r.tagNamespaceLocked(in.GetParent())
	if err != nil {
		return nil, err
	}
	namespaced := ns + "/" + in.GetShortName()
	keys, err := listProtos(r, tagKeyPrefix, func() *rmpb.TagKey { return &rmpb.TagKey{} })
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.GetNamespacedName() == namespaced {
			return nil, apierror.AlreadyExists("tag key %s already exists (%s)", namespaced, k.GetName())
		}
	}
	id := r.newID(tagKeyPrefix)
	now := r.nowTS()
	k := &rmpb.TagKey{Name: "tagKeys/" + id, Parent: in.GetParent(), ShortName: in.GetShortName(), NamespacedName: namespaced,
		Description: in.GetDescription(), CreateTime: now, UpdateTime: now, Purpose: in.GetPurpose(), PurposeData: in.GetPurposeData()}
	k.Etag = protoEtag(k)
	if validateOnly {
		return k, nil
	}
	return k, r.putProto(tagKeyPrefix+id, k)
}

func (r *Registry) tagKeyLocked(name string) (*rmpb.TagKey, string, error) {
	id, err := numericID("tagKeys/", name)
	if err != nil {
		return nil, "", err
	}
	k := &rmpb.TagKey{}
	if err := r.getProto(tagKeyPrefix+id, k); err != nil {
		return nil, "", apierror.NotFound("tag key %s not found", name)
	}
	return k, id, nil
}

// TagKey returns one tag key.
func (r *Registry) TagKey(name string) (*rmpb.TagKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, _, err := r.tagKeyLocked(name)
	return k, err
}

// AllTagKeys returns every tag key.
func (r *Registry) AllTagKeys() ([]*rmpb.TagKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return listProtos(r, tagKeyPrefix, func() *rmpb.TagKey { return &rmpb.TagKey{} })
}

// TagKeys lists the tag keys under parent.
func (r *Registry) TagKeys(parent string) ([]*rmpb.TagKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ns, err := r.tagNamespaceLocked(parent)
	if err != nil {
		return nil, err
	}
	all, err := listProtos(r, tagKeyPrefix, func() *rmpb.TagKey { return &rmpb.TagKey{} })
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, k := range all {
		if strings.HasPrefix(k.GetNamespacedName(), ns+"/") {
			out = append(out, k)
		}
	}
	return out, nil
}

// NamespacedTagKey finds a tag key by "{org_id|project_id}/{short_name}".
func (r *Registry) NamespacedTagKey(namespaced string) (*rmpb.TagKey, error) {
	all, err := r.AllTagKeys()
	if err != nil {
		return nil, err
	}
	for _, k := range all {
		if k.GetNamespacedName() == namespaced {
			return k, nil
		}
	}
	return nil, apierror.NotFound("tag key %s not found", namespaced)
}

func checkEtag(given, cur, name string) error {
	if given != "" && given != cur {
		return apierror.Aborted("etag %q for %s is stale; read it again and retry", given, name)
	}
	return nil
}

// UpdateTagKey changes a tag key's description: the one field that changes.
func (r *Registry) UpdateTagKey(in *rmpb.TagKey, paths []string, validateOnly bool) (*rmpb.TagKey, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("tag_key is required")
	}
	for _, p := range paths {
		if p != "description" {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated; only description", p)
		}
	}
	if err := validateDescription(in.GetDescription()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k, id, err := r.tagKeyLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkEtag(in.GetEtag(), k.GetEtag(), k.GetName()); err != nil {
		return nil, err
	}
	k.Description, k.UpdateTime = in.GetDescription(), r.nowTS()
	k.Etag = protoEtag(k)
	if validateOnly {
		return k, nil
	}
	return k, r.putProto(tagKeyPrefix+id, k)
}

// DeleteTagKey removes a tag key that has no values.
func (r *Registry) DeleteTagKey(name, etag string, validateOnly bool) (*rmpb.TagKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, id, err := r.tagKeyLocked(name)
	if err != nil {
		return nil, err
	}
	if err := checkEtag(etag, k.GetEtag(), k.GetName()); err != nil {
		return nil, err
	}
	values, err := listProtos(r, tagValuePrefix, func() *rmpb.TagValue { return &rmpb.TagValue{} })
	if err != nil {
		return nil, err
	}
	for _, v := range values {
		if v.GetParent() == k.GetName() {
			return nil, apierror.FailedPrecondition("tag key %s has tag value %s; delete its values first", k.GetName(), v.GetName())
		}
	}
	if validateOnly {
		return k, nil
	}
	if err := r.st.Delete(tagKeyPrefix + id); err != nil {
		return nil, apierror.Internal(err, "delete tag key")
	}
	return k, nil
}

// CreateTagValue adds a value under a tag key.
func (r *Registry) CreateTagValue(in *rmpb.TagValue, validateOnly bool) (*rmpb.TagValue, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("tag_value is required")
	}
	if err := validateShortName("tag value", in.GetShortName()); err != nil {
		return nil, err
	}
	if err := validateDescription(in.GetDescription()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k, _, err := r.tagKeyLocked(in.GetParent())
	if err != nil {
		return nil, err
	}
	namespaced := k.GetNamespacedName() + "/" + in.GetShortName()
	values, err := listProtos(r, tagValuePrefix, func() *rmpb.TagValue { return &rmpb.TagValue{} })
	if err != nil {
		return nil, err
	}
	for _, v := range values {
		if v.GetNamespacedName() == namespaced {
			return nil, apierror.AlreadyExists("tag value %s already exists (%s)", namespaced, v.GetName())
		}
	}
	id := r.newID(tagValuePrefix)
	now := r.nowTS()
	v := &rmpb.TagValue{Name: "tagValues/" + id, Parent: k.GetName(), ShortName: in.GetShortName(), NamespacedName: namespaced,
		Description: in.GetDescription(), CreateTime: now, UpdateTime: now}
	v.Etag = protoEtag(v)
	if validateOnly {
		return v, nil
	}
	return v, r.putProto(tagValuePrefix+id, v)
}

func (r *Registry) tagValueLocked(name string) (*rmpb.TagValue, string, error) {
	id, err := numericID("tagValues/", name)
	if err != nil {
		return nil, "", err
	}
	v := &rmpb.TagValue{}
	if err := r.getProto(tagValuePrefix+id, v); err != nil {
		return nil, "", apierror.NotFound("tag value %s not found", name)
	}
	return v, id, nil
}

// TagValue returns one tag value.
func (r *Registry) TagValue(name string) (*rmpb.TagValue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, _, err := r.tagValueLocked(name)
	return v, err
}

// TagValues lists a tag key's values.
func (r *Registry) TagValues(parent string) ([]*rmpb.TagValue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, err := r.tagKeyLocked(parent); err != nil {
		return nil, err
	}
	all, err := listProtos(r, tagValuePrefix, func() *rmpb.TagValue { return &rmpb.TagValue{} })
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, v := range all {
		if v.GetParent() == parent {
			out = append(out, v)
		}
	}
	return out, nil
}

// NamespacedTagValue finds a value by
// "{org_id|project_id}/{key_short_name}/{short_name}".
func (r *Registry) NamespacedTagValue(namespaced string) (*rmpb.TagValue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.namespacedTagValueLocked(namespaced)
}

func (r *Registry) namespacedTagValueLocked(namespaced string) (*rmpb.TagValue, error) {
	all, err := listProtos(r, tagValuePrefix, func() *rmpb.TagValue { return &rmpb.TagValue{} })
	if err != nil {
		return nil, err
	}
	for _, v := range all {
		if v.GetNamespacedName() == namespaced {
			return v, nil
		}
	}
	return nil, apierror.NotFound("tag value %s not found", namespaced)
}

// UpdateTagValue changes a value's description.
func (r *Registry) UpdateTagValue(in *rmpb.TagValue, paths []string, validateOnly bool) (*rmpb.TagValue, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("tag_value is required")
	}
	for _, p := range paths {
		if p != "description" {
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated; only description", p)
		}
	}
	if err := validateDescription(in.GetDescription()); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, id, err := r.tagValueLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := checkEtag(in.GetEtag(), v.GetEtag(), v.GetName()); err != nil {
		return nil, err
	}
	v.Description, v.UpdateTime = in.GetDescription(), r.nowTS()
	v.Etag = protoEtag(v)
	if validateOnly {
		return v, nil
	}
	return v, r.putProto(tagValuePrefix+id, v)
}

// DeleteTagValue removes a value that is bound to nothing and held by
// nothing.
func (r *Registry) DeleteTagValue(name, etag string, validateOnly bool) (*rmpb.TagValue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, id, err := r.tagValueLocked(name)
	if err != nil {
		return nil, err
	}
	if err := checkEtag(etag, v.GetEtag(), v.GetName()); err != nil {
		return nil, err
	}
	bindings, err := listProtos(r, tagBindingPrefix, func() *rmpb.TagBinding { return &rmpb.TagBinding{} })
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if b.GetTagValue() == v.GetName() {
			return nil, apierror.FailedPrecondition("tag value %s is bound to %s; delete its bindings first", v.GetName(), b.GetParent())
		}
	}
	holds, err := r.st.List(tagHoldPrefix + id + "/")
	if err != nil {
		return nil, apierror.Internal(err, "list tag holds")
	}
	if len(holds) > 0 {
		return nil, apierror.FailedPrecondition("tag value %s has %d tag hold(s); delete them first", v.GetName(), len(holds))
	}
	if validateOnly {
		return v, nil
	}
	if err := r.st.Delete(tagValuePrefix + id); err != nil {
		return nil, apierror.Internal(err, "delete tag value")
	}
	return v, nil
}

// --- Tag bindings ----------------------------------------------------------

// canonicalTarget is the form a binding's parent is compared in: a project
// named by ID or number is named by number. Other names are as given.
func (r *Registry) canonicalTargetLocked(parent string) (string, error) {
	if !strings.HasPrefix(parent, "//") || !strings.Contains(strings.TrimPrefix(parent, "//"), "/") {
		return "", apierror.InvalidArgument("tag binding parent %q must be a full resource name, such as %sprojects/{project_number}", parent, CRMPrefix)
	}
	rest, ok := strings.CutPrefix(parent, CRMPrefix)
	if !ok {
		// Another service's resource: stored as named, not looked up,
		// as those resources live in the services that own them.
		return parent, nil
	}
	switch {
	case strings.HasPrefix(rest, "projects/"):
		p, err := r.resolveProjectLocked(rest)
		if err != nil {
			return "", err
		}
		return CRMPrefix + V3Name(p.ProjectID), nil
	case strings.HasPrefix(rest, "folders/"):
		f, err := r.loadFolder(rest)
		if err != nil {
			return "", err
		}
		return CRMPrefix + f.Name, nil
	case strings.HasPrefix(rest, "organizations/"):
		if err := checkOrg(rest); err != nil {
			return "", err
		}
		return CRMPrefix + rest, nil
	}
	return "", apierror.InvalidArgument("tag binding parent %q: Resource Manager resources are projects, folders and organizations", parent)
}

// TagBindingName is a binding's resource name, as Google forms it.
func TagBindingName(parent, value string) string {
	return "tagBindings/" + url.PathEscape(parent) + "/" + value
}

func bindingKey(canonicalParent, value string) string {
	h := sha256.Sum256([]byte(canonicalParent + "\x00" + value))
	return tagBindingPrefix + hex.EncodeToString(h[:16])
}

// ParseTagBindingName splits "tagBindings/{escaped parent}/tagValues/{id}",
// however many times the parent was escaped on its way here.
func ParseTagBindingName(name string) (parent, value string, err error) {
	rest, ok := strings.CutPrefix(name, "tagBindings/")
	i := strings.LastIndex(rest, "/tagValues/")
	if !ok || i <= 0 {
		return "", "", apierror.InvalidArgument("tag binding name %q must be tagBindings/{parent}/tagValues/{id}", name)
	}
	parent, value = rest[:i], rest[i+1:]
	for n := 0; n < 3 && strings.Contains(parent, "%"); n++ {
		u, err := url.PathUnescape(parent)
		if err != nil {
			break
		}
		parent = u
	}
	return parent, value, nil
}

// CreateTagBinding attaches a tag value to a resource. The value may be named
// by tagValues/{id} or by its namespaced name.
func (r *Registry) CreateTagBinding(in *rmpb.TagBinding, validateOnly bool) (*rmpb.TagBinding, error) {
	if in == nil {
		return nil, apierror.InvalidArgument("tag_binding is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	target, err := r.canonicalTargetLocked(in.GetParent())
	if err != nil {
		return nil, err
	}
	var v *rmpb.TagValue
	switch {
	case in.GetTagValue() != "":
		if v, _, err = r.tagValueLocked(in.GetTagValue()); err != nil {
			return nil, err
		}
	case in.GetTagValueNamespacedName() != "":
		if v, err = r.namespacedTagValueLocked(in.GetTagValueNamespacedName()); err != nil {
			return nil, err
		}
	default:
		return nil, apierror.InvalidArgument("tag_value or tag_value_namespaced_name is required")
	}
	all, err := listProtos(r, tagBindingPrefix, func() *rmpb.TagBinding { return &rmpb.TagBinding{} })
	if err != nil {
		return nil, err
	}
	for _, b := range all {
		bt, err := r.canonicalTargetLocked(b.GetParent())
		if err != nil || bt != target {
			continue
		}
		if b.GetTagValue() == v.GetName() {
			return nil, apierror.AlreadyExists("tag value %s is already bound to %s", v.GetName(), in.GetParent())
		}
		if bv, _, err := r.tagValueLocked(b.GetTagValue()); err == nil && bv.GetParent() == v.GetParent() {
			return nil, apierror.FailedPrecondition("%s already has a value of tag key %s (%s); a resource carries one value per key",
				in.GetParent(), v.GetParent(), bv.GetNamespacedName())
		}
	}
	b := &rmpb.TagBinding{Name: TagBindingName(in.GetParent(), v.GetName()), Parent: in.GetParent(), TagValue: v.GetName(),
		TagValueNamespacedName: v.GetNamespacedName()}
	if validateOnly {
		return b, nil
	}
	return b, r.putProto(bindingKey(target, v.GetName()), b)
}

// TagBindings lists the bindings on one resource.
func (r *Registry) TagBindings(parent string) ([]*rmpb.TagBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	target, err := r.canonicalTargetLocked(parent)
	if err != nil {
		return nil, err
	}
	return r.bindingsOnLocked(target)
}

func (r *Registry) bindingsOnLocked(target string) ([]*rmpb.TagBinding, error) {
	all, err := listProtos(r, tagBindingPrefix, func() *rmpb.TagBinding { return &rmpb.TagBinding{} })
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, b := range all {
		if bt, err := r.canonicalTargetLocked(b.GetParent()); err == nil && bt == target {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetTagValueNamespacedName() < out[j].GetTagValueNamespacedName() })
	return out, nil
}

// DeleteTagBinding detaches a value.
func (r *Registry) DeleteTagBinding(name string) error {
	parent, value, err := ParseTagBindingName(name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	target, err := r.canonicalTargetLocked(parent)
	if err != nil {
		return apierror.NotFound("tag binding %s not found: %v", name, err)
	}
	k := bindingKey(target, value)
	if _, err := r.st.Get(k); err != nil {
		return apierror.NotFound("tag binding %s not found", name)
	}
	if err := r.st.Delete(k); err != nil {
		return apierror.Internal(err, "delete tag binding")
	}
	return nil
}

// EffectiveTags is what a resource carries: its own bindings and those its
// ancestors give it, a nearer value of a key replacing a farther one.
func (r *Registry) EffectiveTags(parent string) ([]*rmpb.EffectiveTag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	target, err := r.canonicalTargetLocked(parent)
	if err != nil {
		return nil, err
	}
	chain := []string{target}
	next := ""
	if rest, ok := strings.CutPrefix(target, CRMPrefix); ok {
		switch {
		case strings.HasPrefix(rest, "projects/"):
			p, err := r.resolveProjectLocked(rest)
			if err != nil {
				return nil, err
			}
			next = p.Parent
		case strings.HasPrefix(rest, "folders/"):
			f, err := r.loadFolder(rest)
			if err != nil {
				return nil, err
			}
			next = f.Parent
		}
	}
	for i := 0; next != "" && i <= MaxFolderDepth+1; i++ {
		chain = append(chain, CRMPrefix+next)
		if !strings.HasPrefix(next, "folders/") {
			break
		}
		f, err := r.loadFolder(next)
		if err != nil {
			return nil, err
		}
		next = f.Parent
	}
	seen := map[string]bool{}
	var out []*rmpb.EffectiveTag
	for i, res := range chain {
		bindings, err := r.bindingsOnLocked(res)
		if err != nil {
			return nil, err
		}
		for _, b := range bindings {
			v, _, err := r.tagValueLocked(b.GetTagValue())
			if err != nil {
				continue
			}
			if seen[v.GetParent()] {
				continue
			}
			seen[v.GetParent()] = true
			k, _, err := r.tagKeyLocked(v.GetParent())
			if err != nil {
				continue
			}
			out = append(out, &rmpb.EffectiveTag{TagValue: v.GetName(), NamespacedTagValue: v.GetNamespacedName(),
				TagKey: k.GetName(), NamespacedTagKey: k.GetNamespacedName(), TagKeyParentName: k.GetParent(), Inherited: i > 0})
		}
	}
	return out, nil
}

// --- Tag holds -------------------------------------------------------------

// CreateTagHold records that something depends on a tag value.
func (r *Registry) CreateTagHold(parent string, in *rmpb.TagHold, validateOnly bool) (*rmpb.TagHold, error) {
	if in == nil || in.GetHolder() == "" {
		return nil, apierror.InvalidArgument("tag_hold.holder is required")
	}
	if len(in.GetHolder()) > 200 || len(in.GetOrigin()) > 200 {
		return nil, apierror.InvalidArgument("tag_hold holder and origin are at most 200 characters")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, vid, err := r.tagValueLocked(parent)
	if err != nil {
		return nil, err
	}
	hid := r.newID(tagHoldPrefix + vid + "/")
	h := &rmpb.TagHold{Name: parent + "/tagHolds/" + hid, Holder: in.GetHolder(), Origin: in.GetOrigin(),
		HelpLink: in.GetHelpLink(), CreateTime: r.nowTS()}
	if validateOnly {
		return h, nil
	}
	return h, r.putProto(tagHoldPrefix+vid+"/"+hid, h)
}

// TagHolds lists a value's holds. filter may be empty, or holder="…" or
// origin="…" terms joined by spaces or AND.
func (r *Registry) TagHolds(parent, filter string) ([]*rmpb.TagHold, error) {
	var preds []func(*rmpb.TagHold) bool
	for _, term := range queryTerms(filter) {
		if term == "AND" {
			continue
		}
		key, val, ok := strings.Cut(term, "=")
		if !ok {
			return nil, apierror.InvalidArgument("filter term %q must be holder=\"...\" or origin=\"...\"", term)
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(key) {
		case "holder":
			preds = append(preds, func(h *rmpb.TagHold) bool { return h.GetHolder() == val })
		case "origin":
			preds = append(preds, func(h *rmpb.TagHold) bool { return h.GetOrigin() == val })
		default:
			return nil, apierror.InvalidArgument("filter key %q is not supported: holder and origin are", key)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, vid, err := r.tagValueLocked(parent)
	if err != nil {
		return nil, err
	}
	all, err := listProtos(r, tagHoldPrefix+vid+"/", func() *rmpb.TagHold { return &rmpb.TagHold{} })
	if err != nil {
		return nil, err
	}
	out := all[:0]
next:
	for _, h := range all {
		for _, p := range preds {
			if !p(h) {
				continue next
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// DeleteTagHold removes a hold.
func (r *Registry) DeleteTagHold(name string) error {
	parent, hid, ok := strings.Cut(name, "/tagHolds/")
	if !ok {
		return apierror.InvalidArgument("tag hold name %q must be tagValues/{id}/tagHolds/{id}", name)
	}
	vid, err := numericID("tagValues/", parent)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseUint(hid, 10, 64); err != nil {
		return apierror.InvalidArgument("tag hold name %q must be tagValues/{id}/tagHolds/{id}", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := tagHoldPrefix + vid + "/" + hid
	if _, err := r.st.Get(k); err != nil {
		return apierror.NotFound("tag hold %s not found", name)
	}
	if err := r.st.Delete(k); err != nil {
		return apierror.Internal(err, "delete tag hold")
	}
	return nil
}
