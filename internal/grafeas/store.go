// Package grafeas is CloudBurrow's Container Analysis (#1912): the Grafeas
// v1 API (grafeas.v1.Grafeas: notes, occurrences, ListNoteOccurrences) and
// google.devtools.containeranalysis.v1.ContainerAnalysis (the IAM calls on
// notes and occurrences, and GetVulnerabilityOccurrencesSummary), as
// containeranalysis.googleapis.com serves them, over one store an
// instance keeps in its state directory. Artifact Registry's vulnerability
// scanning (internal/osvscan, internal/adapter/run) writes its DISCOVERY,
// PACKAGE and VULNERABILITY occurrences here, against notes in the
// provider projects Google uses: goog-vulnz for vulnerabilities and
// goog-analysis for discovery.
package grafeas

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	pb "google.golang.org/genproto/googleapis/grafeas/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Provider projects whose notes Google's scanning occurrences name.
const (
	VulnzProject    = "goog-vulnz"
	AnalysisProject = "goog-analysis"
)

// Store holds every note, occurrence and IAM policy of the instance.
type Store struct {
	mu       sync.Mutex
	notes    map[string]*pb.Note
	occs     map[string]*pb.Occurrence
	policies map[string]iampolicy.Stored
	save     func([]byte) error
	now      func() time.Time
	seq      uint64
}

// NewStore returns an empty store that calls save with its whole content
// after every change; save may be nil.
func NewStore(save func([]byte) error) *Store {
	return &Store{notes: map[string]*pb.Note{}, occs: map[string]*pb.Occurrence{},
		policies: map[string]iampolicy.Stored{}, save: save, now: time.Now}
}

type persisted struct {
	Notes       []json.RawMessage           `json:"notes"`
	Occurrences []json.RawMessage           `json:"occurrences"`
	Policies    map[string]iampolicy.Stored `json:"policies,omitempty"`
}

// Load replaces the store's content with what an earlier save wrote.
func (s *Store) Load(b []byte) error {
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes, s.occs = map[string]*pb.Note{}, map[string]*pb.Occurrence{}
	for _, raw := range p.Notes {
		n := &pb.Note{}
		if err := protojson.Unmarshal(raw, n); err != nil {
			return err
		}
		s.notes[n.GetName()] = n
	}
	for _, raw := range p.Occurrences {
		o := &pb.Occurrence{}
		if err := protojson.Unmarshal(raw, o); err != nil {
			return err
		}
		s.occs[o.GetName()] = o
	}
	if p.Policies != nil {
		s.policies = p.Policies
	}
	return nil
}

func (s *Store) persistLocked() {
	if s.save == nil {
		return
	}
	p := persisted{Policies: s.policies}
	for _, n := range sortedKeys(s.notes) {
		b, _ := protojson.Marshal(s.notes[n])
		p.Notes = append(p.Notes, b)
	}
	for _, n := range sortedKeys(s.occs) {
		b, _ := protojson.Marshal(s.occs[n])
		p.Occurrences = append(p.Occurrences, b)
	}
	b, err := json.Marshal(p)
	if err == nil {
		_ = s.save(b)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// newID is an occurrence ID: Google's are UUIDs.
func (s *Store) newID() string {
	s.seq++
	t := s.now().UnixNano()
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", uint32(t>>32), uint16(t>>16), uint16(t)&0xfff, uint16(s.seq)&0xfff, uint64(t)^s.seq<<20)
}

// PutNote creates or replaces a provider's note, as scanning does for the
// vulnerabilities it finds.
func (s *Store) PutNote(n *pb.Note) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n = proto.Clone(n).(*pb.Note)
	now := timestamppb.New(s.now())
	if old, ok := s.notes[n.GetName()]; ok {
		n.CreateTime = old.GetCreateTime()
	} else {
		n.CreateTime = now
	}
	n.UpdateTime = now
	n.Kind = noteKind(n)
	s.notes[n.GetName()] = n
	s.persistLocked()
}

// ReplaceOccurrences deletes the occurrences of resourceURI that keep
// selects and adds occs in their place, under the project's parent; it is
// how a scan's results replace the previous scan's.
func (s *Store) ReplaceOccurrences(parent, resourceURI string, drop func(*pb.Occurrence) bool, occs []*pb.Occurrence) []*pb.Occurrence {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, o := range s.occs {
		if o.GetResourceUri() == resourceURI && strings.HasPrefix(name, parent+"/occurrences/") && drop(o) {
			delete(s.occs, name)
		}
	}
	out := make([]*pb.Occurrence, 0, len(occs))
	now := timestamppb.New(s.now())
	for _, o := range occs {
		o = proto.Clone(o).(*pb.Occurrence)
		if o.Name == "" {
			o.Name = parent + "/occurrences/" + s.newID()
		}
		o.ResourceUri = resourceURI
		o.Kind = occurrenceKind(o)
		if o.CreateTime == nil {
			o.CreateTime = now
		}
		o.UpdateTime = now
		s.occs[o.Name] = o
		out = append(out, proto.Clone(o).(*pb.Occurrence))
	}
	s.persistLocked()
	return out
}

// Occurrences returns copies of the occurrences that keep selects.
func (s *Store) Occurrences(keep func(*pb.Occurrence) bool) []*pb.Occurrence {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*pb.Occurrence
	for _, n := range sortedKeys(s.occs) {
		if o := s.occs[n]; keep(o) {
			out = append(out, proto.Clone(o).(*pb.Occurrence))
		}
	}
	return out
}

// UpdateOccurrenceFunc changes one occurrence in place.
func (s *Store) UpdateOccurrenceFunc(name string, f func(*pb.Occurrence)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.occs[name]
	if !ok {
		return false
	}
	f(o)
	o.UpdateTime = timestamppb.New(s.now())
	s.persistLocked()
	return true
}

// DeleteResource removes every occurrence of a resource, as Google does
// when the image they describe is deleted.
func (s *Store) DeleteResource(resourceURI string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for n, o := range s.occs {
		if o.GetResourceUri() == resourceURI {
			delete(s.occs, n)
			changed = true
		}
	}
	if changed {
		s.persistLocked()
	}
}

// ResetProject forgets a project's notes and occurrences.
func (s *Store) ResetProject(project string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pre := "projects/" + project + "/"
	for n := range s.notes {
		if strings.HasPrefix(n, pre) {
			delete(s.notes, n)
		}
	}
	for n := range s.occs {
		if strings.HasPrefix(n, pre) {
			delete(s.occs, n)
		}
	}
	for n := range s.policies {
		if strings.HasPrefix(n, pre) {
			delete(s.policies, n)
		}
	}
	s.persistLocked()
}

func noteKind(n *pb.Note) pb.NoteKind {
	switch n.GetType().(type) {
	case *pb.Note_Vulnerability:
		return pb.NoteKind_VULNERABILITY
	case *pb.Note_Build:
		return pb.NoteKind_BUILD
	case *pb.Note_Image:
		return pb.NoteKind_IMAGE
	case *pb.Note_Package:
		return pb.NoteKind_PACKAGE
	case *pb.Note_Deployment:
		return pb.NoteKind_DEPLOYMENT
	case *pb.Note_Discovery:
		return pb.NoteKind_DISCOVERY
	case *pb.Note_Attestation:
		return pb.NoteKind_ATTESTATION
	case *pb.Note_Upgrade:
		return pb.NoteKind_UPGRADE
	case *pb.Note_Compliance:
		return pb.NoteKind_COMPLIANCE
	case *pb.Note_DsseAttestation:
		return pb.NoteKind_DSSE_ATTESTATION
	case *pb.Note_VulnerabilityAssessment:
		return pb.NoteKind_VULNERABILITY_ASSESSMENT
	case *pb.Note_SbomReference:
		return pb.NoteKind_SBOM_REFERENCE
	case *pb.Note_Secret:
		return pb.NoteKind_SECRET
	}
	return pb.NoteKind_NOTE_KIND_UNSPECIFIED
}

func occurrenceKind(o *pb.Occurrence) pb.NoteKind {
	switch o.GetDetails().(type) {
	case *pb.Occurrence_Vulnerability:
		return pb.NoteKind_VULNERABILITY
	case *pb.Occurrence_Build:
		return pb.NoteKind_BUILD
	case *pb.Occurrence_Image:
		return pb.NoteKind_IMAGE
	case *pb.Occurrence_Package:
		return pb.NoteKind_PACKAGE
	case *pb.Occurrence_Deployment:
		return pb.NoteKind_DEPLOYMENT
	case *pb.Occurrence_Discovery:
		return pb.NoteKind_DISCOVERY
	case *pb.Occurrence_Attestation:
		return pb.NoteKind_ATTESTATION
	case *pb.Occurrence_Upgrade:
		return pb.NoteKind_UPGRADE
	case *pb.Occurrence_Compliance:
		return pb.NoteKind_COMPLIANCE
	case *pb.Occurrence_DsseAttestation:
		return pb.NoteKind_DSSE_ATTESTATION
	case *pb.Occurrence_SbomReference:
		return pb.NoteKind_SBOM_REFERENCE
	case *pb.Occurrence_Secret:
		return pb.NoteKind_SECRET
	}
	return pb.NoteKind_NOTE_KIND_UNSPECIFIED
}

// occurrenceFields are the values a filter compares for an occurrence.
func occurrenceFields(o *pb.Occurrence) func(string) ([]string, bool) {
	return func(f string) ([]string, bool) {
		one := func(s string) ([]string, bool) {
			if s == "" {
				return nil, true
			}
			return []string{s}, true
		}
		project, id := noteParts(o.GetNoteName())
		switch f {
		case "kind":
			return one(o.GetKind().String())
		case "resourceUrl", "resource_url", "resourceUri", "resource_uri":
			return one(o.GetResourceUri())
		case "noteName", "note_name":
			return one(o.GetNoteName())
		case "noteProjectId", "note_project_id":
			return one(project)
		case "noteId", "note_id":
			return one(id)
		case "name":
			return one(o.GetName())
		case "createTime", "create_time":
			return one(o.GetCreateTime().AsTime().UTC().Format(time.RFC3339Nano))
		case "updateTime", "update_time":
			return one(o.GetUpdateTime().AsTime().UTC().Format(time.RFC3339Nano))
		case "dependencyPackageName", "packageName", "package.name":
			if p := o.GetPackage(); p != nil {
				return one(p.GetName())
			}
			var out []string
			for _, pi := range o.GetVulnerability().GetPackageIssue() {
				out = append(out, pi.GetAffectedPackage())
			}
			return out, true
		case "vulnerability.severity", "severity":
			return one(o.GetVulnerability().GetSeverity().String())
		case "vulnerability.effectiveSeverity", "effectiveSeverity", "effective_severity":
			return one(o.GetVulnerability().GetEffectiveSeverity().String())
		case "vulnerability.fixAvailable", "fixAvailable", "fix_available":
			if o.GetVulnerability() == nil {
				return nil, true
			}
			return []string{fmt.Sprint(o.GetVulnerability().GetFixAvailable())}, true
		case "discovery.analysisStatus", "analysisStatus":
			return one(o.GetDiscovery().GetAnalysisStatus().String())
		}
		return nil, false
	}
}

func knownOccurrenceField(f string) bool {
	_, ok := occurrenceFields(&pb.Occurrence{})(f)
	return ok
}

func noteFields(n *pb.Note) func(string) ([]string, bool) {
	return func(f string) ([]string, bool) {
		one := func(s string) ([]string, bool) {
			if s == "" {
				return nil, true
			}
			return []string{s}, true
		}
		switch f {
		case "kind":
			return one(n.GetKind().String())
		case "name":
			return one(n.GetName())
		case "shortDescription", "short_description":
			return one(n.GetShortDescription())
		case "createTime", "create_time":
			return one(n.GetCreateTime().AsTime().UTC().Format(time.RFC3339Nano))
		case "updateTime", "update_time":
			return one(n.GetUpdateTime().AsTime().UTC().Format(time.RFC3339Nano))
		}
		return nil, false
	}
}

func knownNoteField(f string) bool {
	_, ok := noteFields(&pb.Note{})(f)
	return ok
}

// noteParts splits projects/{p}/notes/{id} (or with a location).
func noteParts(name string) (project, id string) {
	p := strings.Split(name, "/")
	if len(p) >= 4 && p[0] == "projects" {
		return p[1], p[len(p)-1]
	}
	return "", ""
}
