// Package cloudquotas serves the Cloud Quotas API v1
// (cloudquotas.googleapis.com, #2030): each served API's quotas
// (QuotaInfo), with the defaults Google's quotas pages document, and each
// project's quota preferences (QuotaPreference), which are granted at once
// and change the value CloudBurrow enforces through internal/quotalimit.
//
// Read from Google's public docs on 2026-11-20:
// https://docs.cloud.google.com/docs/quotas/reference/rest and
// https://docs.cloud.google.com/docs/quotas/implement-common-use-cases.
//
// Folders and organizations are containers too (#2102), as Google's
// reference names them: folders/{n}/locations/global and
// organizations/{n}/locations/global. Under each, ListQuotaInfos lists the
// quotas whose containerType is that level (IAM's organization quotas; no
// served API documents a folder quota, so a folder lists none), and
// ListQuotaPreferences lists the container's own preferences, not those of
// the projects under it, as the reference says. A folder's or
// organization's quota counts the usage of everything under it (Google's
// Cloud Quotas overview); a preference there does not change a project's
// quotas, which are separate quotas. The quota adjuster's enablement is
// inherited: a container with none set reads its nearest ancestor's, with
// inherited and inheritedFrom set, or "default".
package cloudquotas

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/quotalimit"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Projects resolves a project ID or number to the project ID.
type Projects interface {
	Resolve(idOrNumber string) (string, error)
}

// Hierarchy is the resource hierarchy: Parent is the parent of
// projects/{id}, folders/{n} or organizations/{n} (folders/{n} or
// organizations/{n}, "" for none), and an error when the container does
// not exist.
type Hierarchy interface {
	Parent(container string) (string, error)
}

// Served says whether the instance serves an API, so its quotas are listed.
type Served func(service string) bool

// Server is the Cloud Quotas API.
type Server struct {
	pb.UnimplementedCloudQuotasServer
	st       store.Store
	projects Projects
	served   Served
	now      func() time.Time
	// Hierarchy checks folders and organizations and walks up from a
	// container for the quota adjuster's inheritance; nil takes every
	// folder and organization, each with no parent.
	Hierarchy Hierarchy

	mu sync.Mutex

	providersMu sync.RWMutex
	providers   []Provider
}

// Provider supplies quotas registered while the instance runs (#2090):
// the per-minute request quotas internal/ratequota enforces, each with its
// value in project before any preference (Default).
type Provider func(project string) []Quota

// AddQuotas adds the quotas p supplies to the Catalog's.
func (s *Server) AddQuotas(p Provider) {
	s.providersMu.Lock()
	defer s.providersMu.Unlock()
	s.providers = append(s.providers, p)
}

// Quotas are every quota the server describes in project: the Catalog's
// and the providers'.
func (s *Server) Quotas(project string) []Quota {
	out := append([]Quota(nil), Catalog...)
	s.providersMu.RLock()
	defer s.providersMu.RUnlock()
	for _, p := range s.providers {
		out = append(out, p(project)...)
	}
	return out
}

// NewServer keeps preferences in st. served may be nil (every catalogued
// API is listed).
func NewServer(st store.Store, projects Projects, served Served) *Server {
	return &Server{st: st, projects: projects, served: served, now: time.Now}
}

// Adjuster is the QuotaAdjusterSettingsManager of the same API.
func (s *Server) Adjuster() pb.QuotaAdjusterSettingsManagerServer { return adjuster{s: s} }

const keyPrefix = "cloudquotas/"

// state is one project's preferences and quota adjuster settings.
type state struct {
	Preferences map[string]json.RawMessage `json:"preferences,omitempty"`
	Adjuster    json.RawMessage            `json:"adjuster,omitempty"`
}

func (s *Server) load(project string) (state, error) {
	out := state{Preferences: map[string]json.RawMessage{}}
	if s.st == nil {
		return out, nil
	}
	b, err := s.st.Get(keyPrefix + project)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("cloudquotas: read %s: %w", project, err)
	}
	if out.Preferences == nil {
		out.Preferences = map[string]json.RawMessage{}
	}
	return out, nil
}

func (s *Server) save(project string, st state) error {
	if s.st == nil {
		return nil
	}
	if !store.SafeKey(keyPrefix + project) {
		return status.Errorf(codes.InvalidArgument, "project %q cannot be stored", project)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.st.Put(keyPrefix+project, b)
}

func (st state) preferences() ([]*pb.QuotaPreference, error) {
	ids := make([]string, 0, len(st.Preferences))
	for id := range st.Preferences {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.QuotaPreference, 0, len(ids))
	for _, id := range ids {
		p := &pb.QuotaPreference{}
		if err := protojson.Unmarshal(st.Preferences[id], p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (st *state) put(id string, p *pb.QuotaPreference) error {
	b, err := protojson.Marshal(p)
	if err != nil {
		return err
	}
	st.Preferences[id] = b
	return nil
}

// ResetProject forgets a project's preferences, or every project's for "".
func (s *Server) ResetProject(project string) error {
	if s.st == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if project != "" {
		err := s.st.Delete(keyPrefix + project)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	keys, err := s.st.List(keyPrefix)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.st.Delete(k); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// resolve is the project ID of a project ID or number: the registry's, or
// for a number the registry does not know, the project whose preferences
// are stored, so a preference made in a project that was never created
// (every CloudBurrow service accepts any project) can be read and updated
// by the name it was answered with, which names the project by number.
func (s *Server) resolve(project string) (string, error) {
	id, err := project, error(nil)
	if s.projects != nil {
		if id, err = s.projects.Resolve(project); err == nil {
			return id, nil
		}
	}
	if n, perr := strconv.ParseInt(project, 10, 64); perr == nil && s.st != nil {
		if keys, lerr := s.st.List(keyPrefix); lerr == nil {
			for _, k := range keys {
				if pid := strings.TrimPrefix(k, keyPrefix); resource.ProjectNumber(pid) == n {
					return pid, nil
				}
			}
		}
	}
	return id, err
}

// container parses "{projects|folders|organizations}/{id}/locations/{l}"
// and returns the store key (a project's ID, or folders/{n} and
// organizations/{n}), the canonical parent (a project named by number, as
// Google names it) and the container's level.
func (s *Server) container(parent string) (key, canonical string, level Container, err error) {
	p := strings.Split(parent, "/")
	if len(p) != 4 || p[2] != "locations" || p[1] == "" || p[3] == "" {
		return "", "", 0, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/locations/global", parent)
	}
	switch p[0] {
	case "projects":
		level = ContainerProject
	case "folders":
		level = ContainerFolder
	case "organizations":
		level = ContainerOrganization
	default:
		return "", "", 0, status.Errorf(codes.InvalidArgument, "invalid parent %q", parent)
	}
	if p[3] != "global" {
		return "", "", 0, status.Errorf(codes.InvalidArgument, "location %q is not supported; Cloud Quotas resources are in location global", p[3])
	}
	if level != ContainerProject {
		if _, err := strconv.ParseUint(p[1], 10, 64); err != nil {
			return "", "", 0, status.Errorf(codes.InvalidArgument, "invalid parent %q: a %s is named by its number", parent, strings.TrimSuffix(p[0], "s"))
		}
		name := p[0] + "/" + p[1]
		if s.Hierarchy != nil {
			if _, err := s.Hierarchy.Parent(name); err != nil {
				return "", "", 0, status.Errorf(codes.PermissionDenied, "Permission denied on resource %s %s (or it may not exist).", strings.TrimSuffix(p[0], "s"), p[1])
			}
		}
		return name, name + "/locations/global", level, nil
	}
	id, err := s.resolve(p[1])
	if err != nil {
		return "", "", 0, status.Errorf(codes.PermissionDenied, "Permission denied on resource project %s (or it may not exist).", p[1])
	}
	return id, "projects/" + strconv.FormatInt(resource.ProjectNumber(id), 10) + "/locations/global", level, nil
}

func (s *Server) listed(service string) bool { return s.served == nil || s.served(service) }

func (s *Server) lookup(project, service, quotaID string) (Quota, bool) {
	for _, q := range s.Quotas(project) {
		if q.Service == service && q.QuotaID == quotaID {
			return q, true
		}
	}
	return Quota{}, false
}

// info is q's QuotaInfo in a project, its dimensions' values changed by
// the project's granted preferences.
func info(canonical string, q Quota, prefs []*pb.QuotaPreference) *pb.QuotaInfo {
	dims := []*pb.DimensionsInfo{{Dimensions: map[string]string{}, Details: &pb.QuotaDetails{Value: q.Default},
		ApplicableLocations: []string{"global"}}}
	if len(q.Dimensions) > 0 {
		dims[0].ApplicableLocations = nil
	}
	for _, p := range prefs {
		if p.GetService() != q.Service || p.GetQuotaId() != q.QuotaID || p.GetQuotaConfig().GetGrantedValue() == nil {
			continue
		}
		v := p.GetQuotaConfig().GetGrantedValue().GetValue()
		if len(p.GetDimensions()) == 0 {
			dims[0].Details.Value = v
			continue
		}
		di := &pb.DimensionsInfo{Dimensions: maps.Clone(p.GetDimensions()), Details: &pb.QuotaDetails{Value: v}}
		if r := p.GetDimensions()["region"]; r != "" {
			di.ApplicableLocations = []string{r}
		}
		dims = append(dims, di)
	}
	return &pb.QuotaInfo{
		Name:              canonical + "/services/" + q.Service + "/quotaInfos/" + q.QuotaID,
		QuotaId:           q.QuotaID,
		Metric:            q.Metric,
		Service:           q.Service,
		IsPrecise:         true,
		RefreshInterval:   q.RefreshInterval,
		ContainerType:     q.Container.containerType(),
		Dimensions:        append([]string(nil), q.Dimensions...),
		MetricDisplayName: q.MetricDisplayName,
		QuotaDisplayName:  q.QuotaDisplayName,
		MetricUnit:        q.MetricUnit,
		QuotaIncreaseEligibility: func() *pb.QuotaIncreaseEligibility {
			if q.Fixed {
				return &pb.QuotaIncreaseEligibility{IneligibilityReason: pb.QuotaIncreaseEligibility_NOT_SUPPORTED}
			}
			return &pb.QuotaIncreaseEligibility{IsEligible: true}
		}(),
		IsFixed:         q.Fixed,
		DimensionsInfos: dims,
	}
}

func (c Container) containerType() pb.QuotaInfo_ContainerType {
	switch c {
	case ContainerFolder:
		return pb.QuotaInfo_FOLDER
	case ContainerOrganization:
		return pb.QuotaInfo_ORGANIZATION
	}
	return pb.QuotaInfo_PROJECT
}

func pageSize(n int32) int {
	if n <= 0 || n > 1000 {
		return 1000
	}
	return int(n)
}

// ListQuotaInfos lists a served API's quotas.
func (s *Server) ListQuotaInfos(_ context.Context, req *pb.ListQuotaInfosRequest) (*pb.ListQuotaInfosResponse, error) {
	parent, service, ok := strings.Cut(req.GetParent(), "/services/")
	if !ok || service == "" || strings.Contains(service, "/") {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/locations/global/services/{service}", req.GetParent())
	}
	project, canonical, level, err := s.container(parent)
	if err != nil {
		return nil, err
	}
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	prefs, err := st.preferences()
	if err != nil {
		return nil, err
	}
	byID := map[string]*pb.QuotaInfo{}
	var ids []string
	if s.listed(service) {
		for _, q := range s.Quotas(project) {
			if q.Service == service && q.Container == level {
				byID[q.QuotaID] = info(canonical, q, prefs)
				ids = append(ids, q.QuotaID)
			}
		}
	}
	sort.Strings(ids)
	page, next, err := paging.Page("cloudquotas-infos:"+req.GetParent(), ids, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid page_token: %v", err)
	}
	out := &pb.ListQuotaInfosResponse{NextPageToken: next}
	for _, id := range page {
		out.QuotaInfos = append(out.QuotaInfos, byID[id])
	}
	return out, nil
}

// GetQuotaInfo describes one quota.
func (s *Server) GetQuotaInfo(_ context.Context, req *pb.GetQuotaInfoRequest) (*pb.QuotaInfo, error) {
	parent, rest, ok := strings.Cut(req.GetName(), "/services/")
	service, quotaID, ok2 := strings.Cut(rest, "/quotaInfos/")
	if !ok || !ok2 || service == "" || quotaID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name %q", req.GetName())
	}
	project, canonical, level, err := s.container(parent)
	if err != nil {
		return nil, err
	}
	q, found := s.lookup(project, service, quotaID)
	if !found || !s.listed(service) || q.Container != level {
		return nil, status.Errorf(codes.NotFound, "QuotaInfo %s not found", req.GetName())
	}
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	prefs, err := st.preferences()
	if err != nil {
		return nil, err
	}
	return info(canonical, q, prefs), nil
}

var preferenceID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func splitPreference(name string) (parent, id string, err error) {
	parent, id, ok := strings.Cut(name, "/quotaPreferences/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", "", status.Errorf(codes.InvalidArgument, "invalid name %q: want projects/{project}/locations/global/quotaPreferences/{id}", name)
	}
	return parent, id, nil
}

// ListQuotaPreferences lists a project's preferences: filter takes
// service, quota_id (quotaId), reconciling and dimensions.region terms
// joined by AND; order_by takes a field and desc.
func (s *Server) ListQuotaPreferences(_ context.Context, req *pb.ListQuotaPreferencesRequest) (*pb.ListQuotaPreferencesResponse, error) {
	project, canonical, _, err := s.container(req.GetParent())
	if err != nil {
		return nil, err
	}
	match, err := parseFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	prefs, err := st.preferences()
	if err != nil {
		return nil, err
	}
	byName := map[string]*pb.QuotaPreference{}
	var names []string
	for _, p := range prefs {
		_, id, _ := splitPreference(p.GetName())
		p.Name = canonical + "/quotaPreferences/" + id
		if match(p) {
			byName[p.Name] = p
			names = append(names, p.Name)
		}
	}
	if err := order(names, byName, req.GetOrderBy()); err != nil {
		return nil, err
	}
	page, next, err := pageOrdered("cloudquotas-prefs:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), names, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	out := &pb.ListQuotaPreferencesResponse{NextPageToken: next}
	for _, n := range page {
		out.QuotaPreferences = append(out.QuotaPreferences, byName[n])
	}
	return out, nil
}

// pageOrdered pages names in the order given (paging.Page sorts).
func pageOrdered(scope string, names []string, token string, size int) ([]string, string, error) {
	start := 0
	if token != "" {
		after, err := paging.DecodeToken(scope, token)
		if err != nil {
			return nil, "", status.Errorf(codes.InvalidArgument, "invalid page_token: %v", err)
		}
		n, err := strconv.Atoi(after)
		if err != nil || n < 0 || n > len(names) {
			return nil, "", status.Errorf(codes.InvalidArgument, "invalid page_token")
		}
		start = n
	}
	end := min(start+size, len(names))
	next := ""
	if end < len(names) {
		next = paging.EncodeToken(scope, strconv.Itoa(end))
	}
	return names[start:end], next, nil
}

func order(names []string, byName map[string]*pb.QuotaPreference, orderBy string) error {
	field, desc := strings.TrimSpace(orderBy), false
	if f, ok := strings.CutSuffix(field, " desc"); ok {
		field, desc = strings.TrimSpace(f), true
	}
	key := func(p *pb.QuotaPreference) string { return p.GetName() }
	switch field {
	case "", "name":
	case "service":
		key = func(p *pb.QuotaPreference) string { return p.GetService() + "\x00" + p.GetName() }
	case "quota_id", "quotaId":
		key = func(p *pb.QuotaPreference) string { return p.GetQuotaId() + "\x00" + p.GetName() }
	case "create_time", "createTime":
		key = func(p *pb.QuotaPreference) string { return p.GetCreateTime().AsTime().Format(time.RFC3339Nano) }
	case "update_time", "updateTime":
		key = func(p *pb.QuotaPreference) string { return p.GetUpdateTime().AsTime().Format(time.RFC3339Nano) }
	default:
		return status.Errorf(codes.InvalidArgument, "invalid order_by %q", orderBy)
	}
	sort.SliceStable(names, func(i, j int) bool {
		a, b := key(byName[names[i]]), key(byName[names[j]])
		if desc {
			return a > b
		}
		return a < b
	})
	return nil
}

func parseFilter(f string) (func(*pb.QuotaPreference) bool, error) {
	var tests []func(*pb.QuotaPreference) bool
	for _, term := range strings.Split(f, " AND ") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		k, v, ok := strings.Cut(term, "=")
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "invalid filter %q", f)
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "service":
			tests = append(tests, func(p *pb.QuotaPreference) bool { return p.GetService() == v })
		case "quota_id", "quotaId":
			tests = append(tests, func(p *pb.QuotaPreference) bool { return p.GetQuotaId() == v })
		case "reconciling":
			want := v == "true"
			tests = append(tests, func(p *pb.QuotaPreference) bool { return p.GetReconciling() == want })
		case "dimensions.region", "region":
			tests = append(tests, func(p *pb.QuotaPreference) bool { return p.GetDimensions()["region"] == v })
		default:
			return nil, status.Errorf(codes.InvalidArgument, "invalid filter field %q", k)
		}
	}
	return func(p *pb.QuotaPreference) bool {
		for _, t := range tests {
			if !t(p) {
				return false
			}
		}
		return true
	}, nil
}

// GetQuotaPreference returns one preference.
func (s *Server) GetQuotaPreference(_ context.Context, req *pb.GetQuotaPreferenceRequest) (*pb.QuotaPreference, error) {
	parent, id, err := splitPreference(req.GetName())
	if err != nil {
		return nil, err
	}
	project, canonical, _, err := s.container(parent)
	if err != nil {
		return nil, err
	}
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	raw, ok := st.Preferences[id]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "QuotaPreference %s not found", req.GetName())
	}
	p := &pb.QuotaPreference{}
	if err := protojson.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	p.Name = canonical + "/quotaPreferences/" + id
	return p, nil
}

// CreateQuotaPreference creates a preference and grants it.
func (s *Server) CreateQuotaPreference(_ context.Context, req *pb.CreateQuotaPreferenceRequest) (*pb.QuotaPreference, error) {
	project, canonical, level, err := s.container(req.GetParent())
	if err != nil {
		return nil, err
	}
	id := req.GetQuotaPreferenceId()
	if id == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	if !preferenceID.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid quota_preference_id %q: lowercase letters, digits, hyphens and underscores, at most 63", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	if _, dup := st.Preferences[id]; dup {
		return nil, status.Errorf(codes.AlreadyExists, "QuotaPreference %s/quotaPreferences/%s already exists", canonical, id)
	}
	p, err := s.grant(project, level, nil, req.GetQuotaPreference(), req.GetIgnoreSafetyChecks())
	if err != nil {
		return nil, err
	}
	now := timestamppb.New(s.now())
	p.Name, p.CreateTime, p.UpdateTime = canonical+"/quotaPreferences/"+id, now, now
	p.Etag = etag(p)
	if err := st.put(id, p); err != nil {
		return nil, err
	}
	if err := s.save(project, st); err != nil {
		return nil, err
	}
	return p, nil
}

// UpdateQuotaPreference changes a preference (or creates it with
// allow_missing) and grants the new value.
func (s *Server) UpdateQuotaPreference(_ context.Context, req *pb.UpdateQuotaPreferenceRequest) (*pb.QuotaPreference, error) {
	in := req.GetQuotaPreference()
	parent, id, err := splitPreference(in.GetName())
	if err != nil {
		return nil, err
	}
	if !preferenceID.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid quota preference id %q", id)
	}
	project, canonical, level, err := s.container(parent)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	var old *pb.QuotaPreference
	if raw, ok := st.Preferences[id]; ok {
		old = &pb.QuotaPreference{}
		if err := protojson.Unmarshal(raw, old); err != nil {
			return nil, err
		}
		if in.GetEtag() != "" && in.GetEtag() != old.GetEtag() {
			return nil, status.Errorf(codes.Aborted, "the etag %q does not match the QuotaPreference's current etag", in.GetEtag())
		}
	} else if !req.GetAllowMissing() {
		return nil, status.Errorf(codes.NotFound, "QuotaPreference %s/quotaPreferences/%s not found", canonical, id)
	}
	merged := proto.Clone(in).(*pb.QuotaPreference)
	if old != nil {
		merged = proto.Clone(old).(*pb.QuotaPreference)
		paths := req.GetUpdateMask().GetPaths()
		if len(paths) == 0 {
			paths = []string{"quota_config.preferred_value", "dimensions", "justification", "contact_email", "service", "quota_id"}
		}
		for _, path := range paths {
			switch path {
			case "quota_config", "quota_config.preferred_value", "quotaConfig", "quotaConfig.preferredValue":
				merged.QuotaConfig = &pb.QuotaConfig{PreferredValue: in.GetQuotaConfig().GetPreferredValue(), Annotations: in.GetQuotaConfig().GetAnnotations()}
			case "quota_config.annotations", "quotaConfig.annotations":
				if merged.QuotaConfig == nil {
					merged.QuotaConfig = &pb.QuotaConfig{}
				}
				merged.QuotaConfig.Annotations = in.GetQuotaConfig().GetAnnotations()
			case "dimensions":
				if !maps.Equal(old.GetDimensions(), in.GetDimensions()) {
					return nil, status.Errorf(codes.InvalidArgument, "dimensions cannot be changed")
				}
			case "service", "quota_id", "quotaId":
				if (path == "service" && in.GetService() != "" && in.GetService() != old.GetService()) ||
					(path != "service" && in.GetQuotaId() != "" && in.GetQuotaId() != old.GetQuotaId()) {
					return nil, status.Errorf(codes.InvalidArgument, "%s cannot be changed", path)
				}
			case "justification":
				merged.Justification = in.GetJustification()
			case "contact_email", "contactEmail":
				merged.ContactEmail = in.GetContactEmail()
			default:
				return nil, status.Errorf(codes.InvalidArgument, "invalid update_mask path %q", path)
			}
		}
	}
	p, err := s.grant(project, level, old, merged, req.GetIgnoreSafetyChecks())
	if err != nil {
		return nil, err
	}
	now := timestamppb.New(s.now())
	p.Name, p.UpdateTime = canonical+"/quotaPreferences/"+id, now
	p.CreateTime = now
	if old != nil {
		p.CreateTime = old.GetCreateTime()
	}
	p.Etag = etag(p)
	if req.GetValidateOnly() {
		return p, nil
	}
	if err := st.put(id, p); err != nil {
		return nil, err
	}
	if err := s.save(project, st); err != nil {
		return nil, err
	}
	return p, nil
}

func etag(p *pb.QuotaPreference) string {
	return strconv.FormatInt(p.GetUpdateTime().AsTime().UnixNano(), 36)
}

// grant validates a preference against its quota and the safety checks,
// and grants it: the value is applied at once (a local instance has no
// review), so reconciling is false and granted_value is the preferred one.
func (s *Server) grant(project string, level Container, old, p *pb.QuotaPreference, ignore []pb.QuotaSafetyCheck) (*pb.QuotaPreference, error) {
	if p == nil || p.GetQuotaConfig() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "quota_preference.quota_config is required")
	}
	if p.GetService() == "" || p.GetQuotaId() == "" {
		return nil, status.Errorf(codes.InvalidArgument, "quota_preference.service and quota_preference.quota_id are required")
	}
	q, ok := s.lookup(project, p.GetService(), p.GetQuotaId())
	if !ok || !s.listed(p.GetService()) {
		return nil, status.Errorf(codes.InvalidArgument, "the quota %s of %s does not exist", p.GetQuotaId(), p.GetService())
	}
	if q.Container != level {
		return nil, status.Errorf(codes.InvalidArgument, "the quota %s of %s applies to a %s: set its preference under %s/{id}/locations/global",
			q.QuotaID, q.Service, strings.TrimSuffix(q.Container.Collection(), "s"), q.Container.Collection())
	}
	if q.Fixed {
		return nil, status.Errorf(codes.InvalidArgument, "the quota %s of %s is fixed and cannot be adjusted", q.QuotaID, q.Service)
	}
	for k := range p.GetDimensions() {
		if !contains(q.Dimensions, k) {
			return nil, status.Errorf(codes.InvalidArgument, "the quota %s has no dimension %q", q.QuotaID, k)
		}
	}
	v := p.GetQuotaConfig().GetPreferredValue()
	if v < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "quota_config.preferred_value must not be negative")
	}
	current := q.Default
	if old != nil && old.GetQuotaConfig().GetGrantedValue() != nil {
		current = old.GetQuotaConfig().GetGrantedValue().GetValue()
	}
	if v < current {
		if !containsCheck(ignore, pb.QuotaSafetyCheck_QUOTA_DECREASE_BELOW_USAGE) {
			used, known, err := quotalimit.Usage(project, q.Service, q.QuotaID, p.GetDimensions())
			if err != nil {
				return nil, err
			}
			if known && v < used {
				return nil, status.Errorf(codes.FailedPrecondition,
					"QUOTA_DECREASE_BELOW_USAGE: the preferred value %d is below the current usage %d of %s; set ignore_safety_checks to decrease it anyway", v, used, q.QuotaID)
			}
		}
		if current > 0 && (current-v)*10 > current && !containsCheck(ignore, pb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"QUOTA_DECREASE_PERCENTAGE_TOO_HIGH: the preferred value %d decreases %s by more than 10%% from %d; set ignore_safety_checks to decrease it anyway", v, q.QuotaID, current)
		}
	}
	out := proto.Clone(p).(*pb.QuotaPreference)
	out.Reconciling = false
	out.QuotaConfig.GrantedValue = wrapperspb.Int64(v)
	out.QuotaConfig.StateDetail = "Granted by the local instance."
	if out.QuotaConfig.TraceId == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		out.QuotaConfig.TraceId = hex.EncodeToString(b)
	}
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func containsCheck(xs []pb.QuotaSafetyCheck, x pb.QuotaSafetyCheck) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Value is quotalimit.Source: a granted preference's value for the
// dimensions, the most specific first (a region's, then the container's).
// project is a project ID, or folders/{n} or organizations/{n} for a
// folder's or organization's quota.
func (s *Server) Value(project, service, quotaID string, dims map[string]string) (int64, bool) {
	// A request quota is charged to a project by ID or by number.
	project, err := s.resolve(project)
	if err != nil {
		return 0, false
	}
	st, err := s.load(project)
	if err != nil {
		return 0, false
	}
	prefs, err := st.preferences()
	if err != nil {
		return 0, false
	}
	var whole *int64
	for _, p := range prefs {
		if p.GetService() != service || p.GetQuotaId() != quotaID || p.GetQuotaConfig().GetGrantedValue() == nil {
			continue
		}
		v := p.GetQuotaConfig().GetGrantedValue().GetValue()
		if len(p.GetDimensions()) == 0 {
			whole = &v
			continue
		}
		if maps.Equal(p.GetDimensions(), dims) {
			return v, true
		}
	}
	if whole != nil {
		return *whole, true
	}
	return 0, false
}

// Preferences is a project's preferences, for the console.
func (s *Server) Preferences(project string) ([]*pb.QuotaPreference, error) {
	return s.PreferencesOf("projects/" + project)
}

// PreferencesOf is a container's own preferences (projects/{id},
// folders/{n} or organizations/{n}), for the console.
func (s *Server) PreferencesOf(container string) ([]*pb.QuotaPreference, error) {
	_, canonical, _, err := s.container(container + "/locations/global")
	if err != nil {
		return nil, err
	}
	resp, err := s.ListQuotaPreferences(context.Background(), &pb.ListQuotaPreferencesRequest{Parent: canonical})
	if err != nil {
		return nil, err
	}
	return resp.GetQuotaPreferences(), nil
}

// adjuster serves QuotaAdjusterSettingsManager: the project's quota
// adjuster's enablement, stored and returned (the adjuster itself, which
// raises quotas from usage on Google, has nothing to watch locally).
type adjuster struct {
	pb.UnimplementedQuotaAdjusterSettingsManagerServer
	s *Server
}

func adjusterProject(s *Server, name string) (string, string, error) {
	parent, ok := strings.CutSuffix(name, "/quotaAdjusterSettings")
	if !ok {
		return "", "", status.Errorf(codes.InvalidArgument, "invalid name %q: want projects/{project}/locations/global/quotaAdjusterSettings", name)
	}
	key, canonical, _, err := s.container(parent)
	return key, canonical, err
}

// inherited is the nearest ancestor of a container (its store key) whose
// quota adjuster enablement is set, and that enablement; "default" and
// DISABLED when none is, as Google's inheritedFrom documents.
func (a adjuster) inherited(key string) (string, pb.QuotaAdjusterSettings_Enablement, error) {
	if a.s.Hierarchy == nil {
		return "default", pb.QuotaAdjusterSettings_DISABLED, nil
	}
	name := key
	if !strings.HasPrefix(key, "folders/") && !strings.HasPrefix(key, "organizations/") {
		name = "projects/" + key
	}
	for i := 0; i < 16; i++ {
		parent, err := a.s.Hierarchy.Parent(name)
		if err != nil || parent == "" {
			break
		}
		st, err := a.s.load(parent)
		if err != nil {
			return "", 0, err
		}
		if len(st.Adjuster) > 0 {
			set := &pb.QuotaAdjusterSettings{}
			if err := protojson.Unmarshal(st.Adjuster, set); err != nil {
				return "", 0, err
			}
			return parent, set.GetEnablement(), nil
		}
		name = parent
	}
	return "default", pb.QuotaAdjusterSettings_DISABLED, nil
}

func (a adjuster) GetQuotaAdjusterSettings(_ context.Context, req *pb.GetQuotaAdjusterSettingsRequest) (*pb.QuotaAdjusterSettings, error) {
	project, canonical, err := adjusterProject(a.s, req.GetName())
	if err != nil {
		return nil, err
	}
	st, err := a.s.load(project)
	if err != nil {
		return nil, err
	}
	out := &pb.QuotaAdjusterSettings{Enablement: pb.QuotaAdjusterSettings_DISABLED}
	if len(st.Adjuster) > 0 {
		if err := protojson.Unmarshal(st.Adjuster, out); err != nil {
			return nil, err
		}
	} else {
		from, en, err := a.inherited(project)
		if err != nil {
			return nil, err
		}
		out.Enablement, out.Inherited, out.InheritedFrom = en, true, from
	}
	out.Name = canonical + "/quotaAdjusterSettings"
	return out, nil
}

func (a adjuster) UpdateQuotaAdjusterSettings(_ context.Context, req *pb.UpdateQuotaAdjusterSettingsRequest) (*pb.QuotaAdjusterSettings, error) {
	in := req.GetQuotaAdjusterSettings()
	project, canonical, err := adjusterProject(a.s, in.GetName())
	if err != nil {
		return nil, err
	}
	switch in.GetEnablement() {
	case pb.QuotaAdjusterSettings_ENABLED, pb.QuotaAdjusterSettings_DISABLED:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "enablement must be ENABLED or DISABLED")
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	st, err := a.s.load(project)
	if err != nil {
		return nil, err
	}
	now := a.s.now()
	out := &pb.QuotaAdjusterSettings{Name: canonical + "/quotaAdjusterSettings", Enablement: in.GetEnablement(),
		UpdateTime: timestamppb.New(now), Etag: strconv.FormatInt(now.UnixNano(), 36)}
	if req.GetValidateOnly() {
		return out, nil
	}
	b, err := protojson.Marshal(out)
	if err != nil {
		return nil, err
	}
	st.Adjuster = b
	if err := a.s.save(project, st); err != nil {
		return nil, err
	}
	return out, nil
}
