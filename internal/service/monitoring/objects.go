package monitoring

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/monitoring/dashboard/apiv1/dashboardpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// The configuration objects of the services beyond MetricService (#1964),
// each kept by its resource name under its collection.
const (
	kindPolicies   = "alertPolicies"
	kindChannels   = "notificationChannels"
	kindUptime     = "uptimeCheckConfigs"
	kindGroups     = "groups"
	kindServices   = "services"
	kindSLOs       = "serviceLevelObjectives"
	kindSnoozes    = "snoozes"
	kindDashboards = "dashboards"
)

// newObject is an empty message of a kind, for loading state.
func newObject(kind string) proto.Message {
	switch kind {
	case kindPolicies:
		return &monitoringpb.AlertPolicy{}
	case kindChannels:
		return &monitoringpb.NotificationChannel{}
	case kindUptime:
		return &monitoringpb.UptimeCheckConfig{}
	case kindGroups:
		return &monitoringpb.Group{}
	case kindServices:
		return &monitoringpb.Service{}
	case kindSLOs:
		return &monitoringpb.ServiceLevelObjective{}
	case kindSnoozes:
		return &monitoringpb.Snooze{}
	case kindDashboards:
		return &dashboardpb.Dashboard{}
	case kindDashboardSQL:
		return &structpb.Struct{}
	}
	return nil
}

// put stores an object; the caller holds mu.
func (s *Server) put(kind, name string, m proto.Message) {
	if s.objects[kind] == nil {
		s.objects[kind] = map[string]proto.Message{}
	}
	s.objects[kind][name] = m
}

// get finds an object; the caller holds mu.
func (s *Server) get(kind, name string) proto.Message { return s.objects[kind][name] }

// under lists the objects of a kind whose names start with prefix, by name;
// the caller holds mu.
func (s *Server) under(kind, prefix string) []proto.Message {
	names := make([]string, 0, len(s.objects[kind]))
	for n := range s.objects[kind] {
		if strings.HasPrefix(n, prefix) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]proto.Message, len(names))
	for i, n := range names {
		out[i] = s.objects[kind][n]
	}
	return out
}

// newID is a numeric ID as Google gives alert policies, channels, groups
// and snoozes: a decimal of up to 19 digits.
func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprint(binary.BigEndian.Uint64(b[:]) % 9e18)
}

// newSlug is an ID made from a display name, as Google makes uptime check
// IDs ("my-check-Ab3dE..."): lower-cased words and a random suffix.
func newSlug(display string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(display) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	var r [6]byte
	_, _ = rand.Read(r[:])
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return hex.EncodeToString(r[:])
	}
	return slug + "-" + hex.EncodeToString(r[:])
}

// record is a MutationRecord of now.
func (s *Server) record() *monitoringpb.MutationRecord {
	return &monitoringpb.MutationRecord{MutateTime: timestamppb.New(s.now()), MutatedBy: "cloudburrow"}
}

// applyMask copies the fields paths names from src to dst, top-level or
// dotted; an unknown path is INVALID_ARGUMENT.
func applyMask(dst, src proto.Message, mask *fieldmaskpb.FieldMask) error {
	for _, p := range mask.GetPaths() {
		if err := copyPath(dst.ProtoReflect(), src.ProtoReflect(), strings.Split(p, ".")); err != nil {
			return apierror.InvalidArgument("Field updateMask had an invalid value of %q: %v", p, err)
		}
	}
	return nil
}

func copyPath(dst, src protoreflect.Message, path []string) error {
	fd := fieldByName(dst.Descriptor(), path[0])
	if fd == nil {
		return fmt.Errorf("no field %s in %s", path[0], dst.Descriptor().FullName())
	}
	if len(path) == 1 || fd.Message() == nil || fd.IsList() || fd.IsMap() {
		if src.Has(fd) {
			dst.Set(fd, src.Get(fd))
		} else {
			dst.Clear(fd)
		}
		return nil
	}
	if !src.Has(fd) {
		dst.Clear(fd)
		return nil
	}
	return copyPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), path[1:])
}

// fieldByName finds a field by its proto or JSON name.
func fieldByName(md protoreflect.MessageDescriptor, n string) protoreflect.FieldDescriptor {
	if fd := md.Fields().ByName(protoreflect.Name(n)); fd != nil {
		return fd
	}
	return md.Fields().ByJSONName(n)
}

// pageOf pages a list of named objects.
func pageOf[M proto.Message](scope string, items []M, name func(M) string, token string, size int32) ([]M, string, error) {
	keys := make([]string, len(items))
	by := make(map[string]M, len(items))
	for i, it := range items {
		keys[i] = name(it)
		by[keys[i]] = it
	}
	page, next, err := paging.Page(scope, keys, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("Field pageToken had an invalid value: %v", err)
	}
	out := make([]M, len(page))
	for i, k := range page {
		out[i] = clone(by[k])
	}
	return out, next, nil
}

// checkUserLabels applies Google's user label rules: at most 64, keys and
// values at most 63 characters of lowercase letters, digits, underscores
// and dashes, keys starting with a letter
// (docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/projects.alertPolicies).
func checkUserLabels(field string, m map[string]string) error {
	if len(m) > 64 {
		return apierror.InvalidArgument("Field %s had an invalid value: at most 64 user labels are allowed", field)
	}
	for k, v := range m {
		if len(k) == 0 || len(k) > 63 || !userLabelKeyRE.MatchString(k) {
			return apierror.InvalidArgument("Field %s had an invalid value: the key %q must be 1 to 63 lowercase letters, digits, underscores or dashes, starting with a letter", field, k)
		}
		if len(v) > 63 || !userLabelValueRE.MatchString(v) {
			return apierror.InvalidArgument("Field %s had an invalid value: the value %q of %q must be at most 63 lowercase letters, digits, underscores or dashes", field, v, k)
		}
	}
	return nil
}

// projectOfName is the project of a resource name projects/P/...
func projectOfName(name string) string {
	rest, ok := strings.CutPrefix(name, "projects/")
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(rest, "/")
	return p
}

// childName checks name is projects/P/<collection>/<id> and returns P.
func childName(field, name, collection string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != collection || parts[3] == "" || parts[1] == "" {
		return "", apierror.InvalidArgument("Field %s had an invalid value of %q: expected projects/PROJECT/%s/ID", field, name, collection)
	}
	return parts[1], nil
}

// simpleFilter is the list filter the configuration lists take, a
// conjunction of `field = "value"`, `field != "value"` and `field :
// "value"` terms over the object's JSON fields (dotted paths; user_labels.K
// and labels.K reach map values), joined by AND
// (docs.cloud.google.com/monitoring/api/v3/sorting-and-filtering). Anything
// else is INVALID_ARGUMENT.
type simpleTerm struct {
	path   []string
	op     string
	value  string
	negate bool
}

func parseSimpleFilter(field, f string) ([]simpleTerm, error) {
	f = strings.TrimSpace(f)
	if f == "" {
		return nil, nil
	}
	var out []simpleTerm
	for _, part := range splitAnd(f) {
		part = strings.TrimSpace(part)
		t := simpleTerm{}
		if strings.HasPrefix(part, "NOT ") {
			t.negate = true
			part = strings.TrimSpace(strings.TrimPrefix(part, "NOT "))
		}
		i := strings.IndexAny(part, "=:!")
		if i <= 0 {
			return nil, apierror.InvalidArgument("Field %s had an invalid value of %q: expected terms like field = \"value\"", field, f)
		}
		key := strings.TrimSpace(part[:i])
		rest := part[i:]
		switch {
		case strings.HasPrefix(rest, "!="):
			t.op, rest = "!=", rest[2:]
		case strings.HasPrefix(rest, "="):
			t.op, rest = "=", rest[1:]
		case strings.HasPrefix(rest, ":"):
			t.op, rest = ":", rest[1:]
		default:
			return nil, apierror.InvalidArgument("Field %s had an invalid value of %q", field, f)
		}
		v := strings.TrimSpace(rest)
		if uq, ok := unquote(v); ok {
			v = uq
		}
		t.path, t.value = strings.Split(key, "."), v
		out = append(out, t)
	}
	return out, nil
}

func splitAnd(f string) []string {
	var out []string
	inQ := false
	start := 0
	for i := 0; i < len(f); i++ {
		switch {
		case f[i] == '"':
			inQ = !inQ
		case !inQ && strings.HasPrefix(f[i:], " AND "):
			out = append(out, f[start:i])
			start = i + 5
			i += 4
		}
	}
	return append(out, f[start:])
}

// matchSimple applies terms to a message.
func matchSimple(terms []simpleTerm, m proto.Message) bool {
	for _, t := range terms {
		got, ok := fieldString(m.ProtoReflect(), t.path)
		var hit bool
		switch t.op {
		case "=":
			hit = ok && got == t.value
		case "!=":
			hit = !ok || got != t.value
		case ":":
			hit = ok && (t.value == "*" || strings.Contains(got, t.value))
		}
		if hit == t.negate {
			return false
		}
	}
	return true
}

func fieldString(m protoreflect.Message, path []string) (string, bool) {
	fd := fieldByName(m.Descriptor(), path[0])
	if fd == nil && path[0] == "type" {
		fd = fieldByName(m.Descriptor(), "type")
	}
	if fd == nil {
		return "", false
	}
	v := m.Get(fd)
	switch {
	case fd.IsMap():
		if len(path) < 2 {
			return "", false
		}
		mv := v.Map().Get(protoreflect.ValueOfString(strings.Join(path[1:], ".")).MapKey())
		if !mv.IsValid() {
			return "", false
		}
		return mv.String(), true
	case fd.IsList():
		var parts []string
		for i := 0; i < v.List().Len(); i++ {
			e := v.List().Get(i)
			if fd.Message() != nil {
				if len(path) > 1 {
					if s, ok := fieldString(e.Message(), path[1:]); ok {
						parts = append(parts, s)
					}
				}
				continue
			}
			parts = append(parts, e.String())
		}
		return strings.Join(parts, ","), len(parts) > 0
	case fd.Message() != nil:
		if fd.Message().FullName() == "google.protobuf.BoolValue" {
			return fmt.Sprint(v.Message().Get(fd.Message().Fields().ByName("value")).Bool()), true
		}
		if len(path) < 2 || !m.Has(fd) {
			return "", m.Has(fd)
		}
		return fieldString(v.Message(), path[1:])
	case fd.Enum() != nil:
		ev := fd.Enum().Values().ByNumber(v.Enum())
		if ev == nil {
			return fmt.Sprint(v.Enum()), true
		}
		return string(ev.Name()), true
	}
	return v.String(), true
}

// durationOK reports whether d is a whole number of seconds in [min, max].
func durationOK(d time.Duration, min, max time.Duration) bool {
	return d%time.Second == 0 && d >= min && d <= max
}
