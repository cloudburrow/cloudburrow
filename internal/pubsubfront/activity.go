package pubsubfront

// The expiration clocks, read and restored (#880), so `cloudburrow state
// save` keeps how long each subscription has been idle and `state load`
// gives it back: a subscription saved a day into a two-day ttl expires a day
// after it is loaded, not two. Like ClockMethod they are CloudBurrow methods
// on the Pub/Sub port, not part of the Pub/Sub API, and grant nothing a
// caller could not already do.

import (
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// ActivityExportMethod reads how long each subscription the front knows
	// has been idle. Its request is a google.protobuf.Empty, and its response
	// a google.protobuf.Struct whose fields are subscription names and whose
	// values are seconds idle, by the front's clock; one with a streaming
	// pull open is idle for 0.
	ActivityExportMethod = "/cloudburrow.pubsub.v1.SubscriptionActivity/Export"
	// ActivityImportMethod sets how long subscriptions have been idle. Its
	// request is a Struct of the same form, whose values must not be
	// negative, and its response a google.protobuf.Empty. A subscription
	// not named keeps its clock.
	ActivityImportMethod = "/cloudburrow.pubsub.v1.SubscriptionActivity/Import"
)

// Idle is how long each subscription the front knows has been idle.
func (f *Front) Idle() map[string]time.Duration {
	now := f.clock.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]time.Duration, len(f.subs))
	for name, s := range f.subs {
		d := now.Sub(s.last)
		if s.streams > 0 || d < 0 {
			d = 0
		}
		out[name] = d
	}
	return out
}

// SetIdle makes each named subscription idle for its duration as of now.
func (f *Front) SetIdle(idle map[string]time.Duration) {
	now := f.clock.Now()
	f.mu.Lock()
	for name, d := range idle {
		s := f.subs[name]
		if s == nil {
			s = &subState{}
			f.subs[name] = s
		}
		s.last = now.Add(-d)
	}
	f.mu.Unlock()
	f.persist()
}

func (f *Front) handleActivityExport(ss grpc.ServerStream) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	fields := map[string]*structpb.Value{}
	for name, d := range f.Idle() {
		fields[name] = structpb.NewNumberValue(d.Seconds())
	}
	b, err := proto.Marshal(&structpb.Struct{Fields: fields})
	if err != nil {
		return status.Errorf(codes.Internal, "encode the activity: %v", err)
	}
	out := frame(b)
	return ss.SendMsg(&out)
}

func (f *Front) handleActivityImport(ss grpc.ServerStream) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var st structpb.Struct
	if err := proto.Unmarshal(in, &st); err != nil {
		return status.Errorf(codes.InvalidArgument, "the request is not a google.protobuf.Struct: %v", err)
	}
	idle := map[string]time.Duration{}
	for name, v := range st.GetFields() {
		n, ok := v.GetKind().(*structpb.Value_NumberValue)
		if !ok || n.NumberValue < 0 {
			return status.Errorf(codes.InvalidArgument, "%s: idle seconds must be a number of at least 0", name)
		}
		idle[name] = time.Duration(n.NumberValue * float64(time.Second))
	}
	f.SetIdle(idle)
	b, _ := proto.Marshal(&emptypb.Empty{})
	out := frame(b)
	return ss.SendMsg(&out)
}

// ProjectsMethod lists every project a call through the front has named, so
// `cloudburrow state save` finds the resources of projects CloudBurrow's
// registry does not know: the emulator serves any project and cannot list
// them. Its request is a google.protobuf.Empty, and its response a
// google.protobuf.ListValue of project IDs, sorted. A project stays listed
// once named, whether or not anything is left in it.
const ProjectsMethod = "/cloudburrow.pubsub.v1.Projects/List"

// sawProject records the project a request names in its first field. In
// every call that creates, reads, lists, publishes or pulls, that field is a
// resource's name or its parent (projects/{project}/...), or the project
// itself; an update's first field is the resource, as a message, and is
// skipped, since the call that created the resource named its project. The
// field is read from the encoded bytes, so no request type is needed and a
// message's data is never looked at.
func (f *Front) sawProject(fr frame) {
	// Field 1, length-delimited: tag 0x0a, then a varint length.
	if len(fr) < 2 || fr[0] != 0x0a {
		return
	}
	n, i := 0, 1
	for shift := 0; i < len(fr) && shift < 28; shift += 7 {
		b := fr[i]
		i++
		n |= int(b&0x7f) << shift
		if b < 0x80 {
			break
		}
	}
	if n <= 0 || i+n > len(fr) {
		return
	}
	f.sawProjectID(projectOf(string(fr[i : i+n])))
}

// sawProjectID records a project a call named, if it is a project ID.
func (f *Front) sawProjectID(project string) {
	if !validProject(project) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.projects[project] {
		f.projects[project] = true
		f.markDirty()
	}
}

// projectOf is the project a resource name names, or "".
func projectOf(name string) string {
	rest, ok := strings.CutPrefix(name, "projects/")
	if !ok {
		return ""
	}
	p, _, _ := strings.Cut(rest, "/")
	if !validProject(p) {
		return ""
	}
	return p
}

// validProject is Google's project ID form: 6 to 30 lowercase letters,
// digits and hyphens, starting with a letter and not ending with a hyphen.
func validProject(p string) bool {
	if len(p) < 6 || len(p) > 30 || p[0] < 'a' || p[0] > 'z' || p[len(p)-1] == '-' {
		return false
	}
	for _, c := range p {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Projects is every project a call has named, sorted.
func (f *Front) Projects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.projects))
	for p := range f.projects {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (f *Front) handleProjects(ss grpc.ServerStream) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	var l structpb.ListValue
	for _, p := range f.Projects() {
		l.Values = append(l.Values, structpb.NewStringValue(p))
	}
	b, err := proto.Marshal(&l)
	if err != nil {
		return status.Errorf(codes.Internal, "encode the projects: %v", err)
	}
	out := frame(b)
	return ss.SendMsg(&out)
}
