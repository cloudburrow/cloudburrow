package grafeas

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	capb "google.golang.org/genproto/googleapis/devtools/containeranalysis/v1"
	pb "google.golang.org/genproto/googleapis/grafeas/v1"
	iampb "google.golang.org/genproto/googleapis/iam/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server serves grafeas.v1.Grafeas and
// google.devtools.containeranalysis.v1.ContainerAnalysis over a Store.
type Server struct {
	pb.UnimplementedGrafeasServer
	st *Store
}

// CA is the ContainerAnalysis service over the same store.
type CA struct {
	capb.UnimplementedContainerAnalysisServer
	st *Store
}

// NewServer serves st.
func NewServer(st *Store) *Server { return &Server{st: st} }

// Store is the server's store.
func (s *Server) Store() *Store { return s.st }

// Register adds both services to a gRPC server or a JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	g.RegisterService(&grafeasDesc, s)
	g.RegisterService(&caDesc, &CA{st: s.st})
}

const (
	defaultPageSize = 20
	maxPageSize     = 1000
	maxBatch        = 1000
)

var (
	parentRE = regexp.MustCompile(`^projects/[^/]+(/locations/[^/]+)?$`)
	idRE     = regexp.MustCompile(`^[A-Za-z0-9._~:@-]{1,255}$`)
)

func invalid(format string, a ...any) error {
	return status.Errorf(codes.InvalidArgument, format, a...)
}

func checkParent(p string) error {
	if !parentRE.MatchString(p) {
		return invalid("invalid parent %q: must be projects/{project_id} or projects/{project_id}/locations/{location}", p)
	}
	return nil
}

// split returns the parent and ID of {parent}/{collection}/{id}.
func split(name, collection string) (parent, id string, err error) {
	i := strings.LastIndex(name, "/"+collection+"/")
	if i < 0 || checkParent(name[:i]) != nil || !idRE.MatchString(name[i+len(collection)+2:]) {
		return "", "", invalid("invalid %s name %q", strings.TrimSuffix(collection, "s"), name)
	}
	return name[:i], name[i+len(collection)+2:], nil
}

func page(n int, size int32, token string) (start, end int, next string, err error) {
	if size < 0 {
		return 0, 0, "", invalid("page_size must not be negative")
	}
	if size == 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	if token != "" {
		start, err = strconv.Atoi(token)
		if err != nil || start < 0 || start > n {
			return 0, 0, "", invalid("invalid page_token %q", token)
		}
	}
	end = start + int(size)
	if end >= n {
		end = n
	} else {
		next = strconv.Itoa(end)
	}
	return start, end, next, nil
}

// --- notes ---

func (s *Server) GetNote(_ context.Context, req *pb.GetNoteRequest) (*pb.Note, error) {
	if _, _, err := split(req.GetName(), "notes"); err != nil {
		return nil, err
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	n, ok := s.st.notes[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "note %q not found", req.GetName())
	}
	return proto.Clone(n).(*pb.Note), nil
}

func (s *Server) ListNotes(_ context.Context, req *pb.ListNotesRequest) (*pb.ListNotesResponse, error) {
	if err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	f, err := parseFilter(req.GetFilter(), knownNoteField)
	if err != nil {
		return nil, invalid("invalid filter: %v", err)
	}
	s.st.mu.Lock()
	var all []*pb.Note
	for _, name := range sortedKeys(s.st.notes) {
		if strings.HasPrefix(name, req.GetParent()+"/notes/") && f.match(noteFields(s.st.notes[name])) {
			all = append(all, proto.Clone(s.st.notes[name]).(*pb.Note))
		}
	}
	s.st.mu.Unlock()
	a, b, next, err := page(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pb.ListNotesResponse{Notes: all[a:b], NextPageToken: next}, nil
}

func (s *Server) checkNewNote(parent, id string, n *pb.Note) error {
	if err := checkParent(parent); err != nil {
		return err
	}
	if id == "" {
		return invalid("note_id is required")
	}
	if !idRE.MatchString(id) {
		return invalid("invalid note_id %q", id)
	}
	if n == nil {
		return invalid("note is required")
	}
	if noteKind(n) == pb.NoteKind_NOTE_KIND_UNSPECIFIED {
		return invalid("note must have a type (vulnerability, build, image, package, deployment, discovery, attestation, upgrade, compliance, ...)")
	}
	return nil
}

func (s *Server) createNoteLocked(parent, id string, n *pb.Note) *pb.Note {
	n = proto.Clone(n).(*pb.Note)
	n.Name = parent + "/notes/" + id
	n.Kind = noteKind(n)
	now := timestamppb.New(s.st.now())
	n.CreateTime, n.UpdateTime = now, now
	s.st.notes[n.Name] = n
	return proto.Clone(n).(*pb.Note)
}

func (s *Server) CreateNote(_ context.Context, req *pb.CreateNoteRequest) (*pb.Note, error) {
	if err := s.checkNewNote(req.GetParent(), req.GetNoteId(), req.GetNote()); err != nil {
		return nil, err
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	name := req.GetParent() + "/notes/" + req.GetNoteId()
	if _, ok := s.st.notes[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "note %q already exists", name)
	}
	out := s.createNoteLocked(req.GetParent(), req.GetNoteId(), req.GetNote())
	s.st.persistLocked()
	return out, nil
}

func (s *Server) BatchCreateNotes(_ context.Context, req *pb.BatchCreateNotesRequest) (*pb.BatchCreateNotesResponse, error) {
	if len(req.GetNotes()) == 0 {
		return nil, invalid("notes must not be empty")
	}
	if len(req.GetNotes()) > maxBatch {
		return nil, invalid("at most %d notes can be created at once", maxBatch)
	}
	for id, n := range req.GetNotes() {
		if err := s.checkNewNote(req.GetParent(), id, n); err != nil {
			return nil, err
		}
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	for _, id := range sortedKeys(req.GetNotes()) {
		if _, ok := s.st.notes[req.GetParent()+"/notes/"+id]; ok {
			return nil, status.Errorf(codes.AlreadyExists, "note %q already exists", req.GetParent()+"/notes/"+id)
		}
	}
	out := &pb.BatchCreateNotesResponse{}
	for _, id := range sortedKeys(req.GetNotes()) {
		out.Notes = append(out.Notes, s.createNoteLocked(req.GetParent(), id, req.GetNotes()[id]))
	}
	s.st.persistLocked()
	return out, nil
}

func (s *Server) UpdateNote(_ context.Context, req *pb.UpdateNoteRequest) (*pb.Note, error) {
	if _, _, err := split(req.GetName(), "notes"); err != nil {
		return nil, err
	}
	if req.GetNote() == nil {
		return nil, invalid("note is required")
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	cur, ok := s.st.notes[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "note %q not found", req.GetName())
	}
	next := proto.Clone(cur).(*pb.Note)
	if err := applyMask(next, req.GetNote(), req.GetUpdateMask()); err != nil {
		return nil, err
	}
	next.Name, next.CreateTime = cur.Name, cur.CreateTime
	next.Kind = noteKind(next)
	if next.Kind != cur.Kind {
		return nil, invalid("a note's type cannot be changed")
	}
	next.UpdateTime = timestamppb.New(s.st.now())
	s.st.notes[req.GetName()] = next
	s.st.persistLocked()
	return proto.Clone(next).(*pb.Note), nil
}

func (s *Server) DeleteNote(_ context.Context, req *pb.DeleteNoteRequest) (*emptypb.Empty, error) {
	if _, _, err := split(req.GetName(), "notes"); err != nil {
		return nil, err
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	if _, ok := s.st.notes[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "note %q not found", req.GetName())
	}
	delete(s.st.notes, req.GetName())
	delete(s.st.policies, req.GetName())
	s.st.persistLocked()
	return &emptypb.Empty{}, nil
}

func (s *Server) ListNoteOccurrences(_ context.Context, req *pb.ListNoteOccurrencesRequest) (*pb.ListNoteOccurrencesResponse, error) {
	if _, _, err := split(req.GetName(), "notes"); err != nil {
		return nil, err
	}
	f, err := parseFilter(req.GetFilter(), knownOccurrenceField)
	if err != nil {
		return nil, invalid("invalid filter: %v", err)
	}
	s.st.mu.Lock()
	if _, ok := s.st.notes[req.GetName()]; !ok {
		s.st.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "note %q not found", req.GetName())
	}
	var all []*pb.Occurrence
	for _, n := range sortedKeys(s.st.occs) {
		o := s.st.occs[n]
		if o.GetNoteName() == req.GetName() && f.match(occurrenceFields(o)) {
			all = append(all, proto.Clone(o).(*pb.Occurrence))
		}
	}
	s.st.mu.Unlock()
	a, b, next, err := page(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pb.ListNoteOccurrencesResponse{Occurrences: all[a:b], NextPageToken: next}, nil
}

// --- occurrences ---

func (s *Server) GetOccurrence(_ context.Context, req *pb.GetOccurrenceRequest) (*pb.Occurrence, error) {
	if _, _, err := split(req.GetName(), "occurrences"); err != nil {
		return nil, err
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	o, ok := s.st.occs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "occurrence %q not found", req.GetName())
	}
	return proto.Clone(o).(*pb.Occurrence), nil
}

func (s *Server) GetOccurrenceNote(_ context.Context, req *pb.GetOccurrenceNoteRequest) (*pb.Note, error) {
	if _, _, err := split(req.GetName(), "occurrences"); err != nil {
		return nil, err
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	o, ok := s.st.occs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "occurrence %q not found", req.GetName())
	}
	n, ok := s.st.notes[o.GetNoteName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "note %q not found", o.GetNoteName())
	}
	return proto.Clone(n).(*pb.Note), nil
}

// matchOccurrencesLocked is the parent's occurrences that the filter keeps.
func (s *Server) matchOccurrencesLocked(parent string, f filter) []*pb.Occurrence {
	var all []*pb.Occurrence
	for _, n := range sortedKeys(s.st.occs) {
		o := s.st.occs[n]
		if strings.HasPrefix(n, parent+"/occurrences/") && f.match(occurrenceFields(o)) {
			all = append(all, o)
		}
	}
	return all
}

func (s *Server) ListOccurrences(_ context.Context, req *pb.ListOccurrencesRequest) (*pb.ListOccurrencesResponse, error) {
	if err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	f, err := parseFilter(req.GetFilter(), knownOccurrenceField)
	if err != nil {
		return nil, invalid("invalid filter: %v", err)
	}
	s.st.mu.Lock()
	all := s.matchOccurrencesLocked(req.GetParent(), f)
	out := make([]*pb.Occurrence, len(all))
	for i, o := range all {
		out[i] = proto.Clone(o).(*pb.Occurrence)
	}
	s.st.mu.Unlock()
	a, b, next, err := page(len(out), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &pb.ListOccurrencesResponse{Occurrences: out[a:b], NextPageToken: next}, nil
}

func (s *Server) checkNewOccurrenceLocked(parent string, o *pb.Occurrence) error {
	if err := checkParent(parent); err != nil {
		return err
	}
	if o == nil {
		return invalid("occurrence is required")
	}
	if o.GetResourceUri() == "" {
		return invalid("occurrence.resource_uri is required")
	}
	if o.GetNoteName() == "" {
		return invalid("occurrence.note_name is required")
	}
	if _, _, err := split(o.GetNoteName(), "notes"); err != nil {
		return err
	}
	n, ok := s.st.notes[o.GetNoteName()]
	if !ok {
		return status.Errorf(codes.NotFound, "note %q not found", o.GetNoteName())
	}
	k := occurrenceKind(o)
	if k == pb.NoteKind_NOTE_KIND_UNSPECIFIED {
		return invalid("occurrence must have details (vulnerability, build, image, package, deployment, discovery, attestation, ...)")
	}
	if k != n.GetKind() {
		return invalid("occurrence kind %s does not match note %q of kind %s", k, n.GetName(), n.GetKind())
	}
	return nil
}

func (s *Server) createOccurrenceLocked(parent string, o *pb.Occurrence) *pb.Occurrence {
	o = proto.Clone(o).(*pb.Occurrence)
	o.Name = parent + "/occurrences/" + s.st.newID()
	o.Kind = occurrenceKind(o)
	now := timestamppb.New(s.st.now())
	o.CreateTime, o.UpdateTime = now, now
	s.st.occs[o.Name] = o
	return proto.Clone(o).(*pb.Occurrence)
}

func (s *Server) CreateOccurrence(_ context.Context, req *pb.CreateOccurrenceRequest) (*pb.Occurrence, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	if err := s.checkNewOccurrenceLocked(req.GetParent(), req.GetOccurrence()); err != nil {
		return nil, err
	}
	out := s.createOccurrenceLocked(req.GetParent(), req.GetOccurrence())
	s.st.persistLocked()
	return out, nil
}

func (s *Server) BatchCreateOccurrences(_ context.Context, req *pb.BatchCreateOccurrencesRequest) (*pb.BatchCreateOccurrencesResponse, error) {
	if len(req.GetOccurrences()) == 0 {
		return nil, invalid("occurrences must not be empty")
	}
	if len(req.GetOccurrences()) > maxBatch {
		return nil, invalid("at most %d occurrences can be created at once", maxBatch)
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	for _, o := range req.GetOccurrences() {
		if err := s.checkNewOccurrenceLocked(req.GetParent(), o); err != nil {
			return nil, err
		}
	}
	out := &pb.BatchCreateOccurrencesResponse{}
	for _, o := range req.GetOccurrences() {
		out.Occurrences = append(out.Occurrences, s.createOccurrenceLocked(req.GetParent(), o))
	}
	s.st.persistLocked()
	return out, nil
}

func (s *Server) UpdateOccurrence(_ context.Context, req *pb.UpdateOccurrenceRequest) (*pb.Occurrence, error) {
	if _, _, err := split(req.GetName(), "occurrences"); err != nil {
		return nil, err
	}
	if req.GetOccurrence() == nil {
		return nil, invalid("occurrence is required")
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	cur, ok := s.st.occs[req.GetName()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "occurrence %q not found", req.GetName())
	}
	next := proto.Clone(cur).(*pb.Occurrence)
	if err := applyMask(next, req.GetOccurrence(), req.GetUpdateMask()); err != nil {
		return nil, err
	}
	next.Name, next.CreateTime = cur.Name, cur.CreateTime
	next.Kind = occurrenceKind(next)
	if next.Kind != cur.Kind {
		return nil, invalid("an occurrence's kind cannot be changed")
	}
	if next.GetNoteName() != cur.GetNoteName() {
		if _, ok := s.st.notes[next.GetNoteName()]; !ok {
			return nil, status.Errorf(codes.NotFound, "note %q not found", next.GetNoteName())
		}
	}
	next.UpdateTime = timestamppb.New(s.st.now())
	s.st.occs[req.GetName()] = next
	s.st.persistLocked()
	return proto.Clone(next).(*pb.Occurrence), nil
}

func (s *Server) DeleteOccurrence(_ context.Context, req *pb.DeleteOccurrenceRequest) (*emptypb.Empty, error) {
	if _, _, err := split(req.GetName(), "occurrences"); err != nil {
		return nil, err
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	if _, ok := s.st.occs[req.GetName()]; !ok {
		return nil, status.Errorf(codes.NotFound, "occurrence %q not found", req.GetName())
	}
	delete(s.st.occs, req.GetName())
	delete(s.st.policies, req.GetName())
	s.st.persistLocked()
	return &emptypb.Empty{}, nil
}

// v1beta1Names are a Note's Grafeas v1beta1 field names that UpdateNote's
// mask still accepts for their v1 fields (#1578): hashicorp/google's
// google_container_analysis_note sends its attestation hint as v1's
// `attestation` with updateMask=attestationAuthority, v1beta1's name
// (resource_container_analysis_note.go, 8.4.0 to 8.12.0), and its update
// succeeds against Google, so Google's server maps the old name.
var v1beta1Names = map[string]string{"attestation_authority": "attestation", "attestationAuthority": "attestation"}

// applyMask copies the masked top-level fields of src into dst; with no
// mask, every field src sets.
func applyMask(dst, src proto.Message, mask *fieldmaskpb.FieldMask) error {
	d, sr := dst.ProtoReflect(), src.ProtoReflect()
	fields := sr.Descriptor().Fields()
	if len(mask.GetPaths()) == 0 {
		sr.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			d.Set(fd, v)
			return true
		})
		return nil
	}
	for _, p := range mask.GetPaths() {
		top := strings.SplitN(p, ".", 2)[0]
		if alias, ok := v1beta1Names[top]; ok && sr.Descriptor().FullName() == "grafeas.v1.Note" {
			top = alias
		}
		fd := fields.ByName(protoreflect.Name(top))
		if fd == nil {
			fd = fields.ByJSONName(top)
		}
		if fd == nil {
			return invalid("invalid update_mask path %q", p)
		}
		if sr.Has(fd) {
			d.Set(fd, sr.Get(fd))
		} else {
			d.Clear(fd)
		}
	}
	return nil
}

// --- ContainerAnalysis ---

func (c *CA) resourceLocked(name string) error {
	if _, _, err := split(name, "notes"); err == nil {
		if _, ok := c.st.notes[name]; ok {
			return nil
		}
		return status.Errorf(codes.NotFound, "note %q not found", name)
	}
	if _, _, err := split(name, "occurrences"); err == nil {
		if _, ok := c.st.occs[name]; ok {
			return nil
		}
		return status.Errorf(codes.NotFound, "occurrence %q not found", name)
	}
	return invalid("resource %q must be a note, projects/{project}/notes/{note}, or an occurrence, projects/{project}/occurrences/{occurrence}", name)
}

func (c *CA) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	if err := c.resourceLocked(req.GetResource()); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if p, ok := c.st.policies[req.GetResource()]; ok {
		cur = &p
	}
	return iampolicy.Get(cur, req.GetOptions())
}

func (c *CA) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	if err := c.resourceLocked(req.GetResource()); err != nil {
		return nil, err
	}
	var cur *iampolicy.Stored
	if p, ok := c.st.policies[req.GetResource()]; ok {
		cur = &p
	}
	next, err := iampolicy.Set(cur, req)
	if err != nil {
		return nil, err
	}
	c.st.policies[req.GetResource()] = next
	c.st.persistLocked()
	return next.Proto(), nil
}

func (c *CA) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	c.st.mu.Lock()
	err := c.resourceLocked(req.GetResource())
	c.st.mu.Unlock()
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return &iampb.TestIamPermissionsResponse{}, nil
		}
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// GetVulnerabilityOccurrencesSummary counts the parent's VULNERABILITY
// occurrences the filter keeps, by resource and effective severity, with
// how many have a fix.
func (c *CA) GetVulnerabilityOccurrencesSummary(_ context.Context, req *capb.GetVulnerabilityOccurrencesSummaryRequest) (*capb.VulnerabilityOccurrencesSummary, error) {
	if err := checkParent(req.GetParent()); err != nil {
		return nil, err
	}
	f, err := parseFilter(req.GetFilter(), knownOccurrenceField)
	if err != nil {
		return nil, invalid("invalid filter: %v", err)
	}
	type key struct {
		uri string
		sev pb.Severity
	}
	counts := map[key]*capb.VulnerabilityOccurrencesSummary_FixableTotalByDigest{}
	c.st.mu.Lock()
	for _, o := range (&Server{st: c.st}).matchOccurrencesLocked(req.GetParent(), f) {
		v := o.GetVulnerability()
		if v == nil {
			continue
		}
		sev := v.GetEffectiveSeverity()
		if sev == pb.Severity_SEVERITY_UNSPECIFIED {
			sev = v.GetSeverity()
		}
		k := key{o.GetResourceUri(), sev}
		e := counts[k]
		if e == nil {
			e = &capb.VulnerabilityOccurrencesSummary_FixableTotalByDigest{ResourceUri: k.uri, Severity: sev}
			counts[k] = e
		}
		e.TotalCount++
		if v.GetFixAvailable() {
			e.FixableCount++
		}
	}
	c.st.mu.Unlock()
	out := &capb.VulnerabilityOccurrencesSummary{}
	for _, e := range counts {
		out.Counts = append(out.Counts, e)
	}
	sort.Slice(out.Counts, func(i, j int) bool {
		a, b := out.Counts[i], out.Counts[j]
		if a.ResourceUri != b.ResourceUri {
			return a.ResourceUri < b.ResourceUri
		}
		return a.Severity > b.Severity
	})
	return out, nil
}

// IAMPolicy is a note's or an occurrence's stored policy, for IAM
// enforcement (#1879), which applies it.
func (s *Server) IAMPolicy(ctx context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	return (&CA{st: s.st}).GetIamPolicy(ctx, req)
}
