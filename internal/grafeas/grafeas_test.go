package grafeas

import (
	"context"
	"testing"

	capb "google.golang.org/genproto/googleapis/devtools/containeranalysis/v1"
	pb "google.golang.org/genproto/googleapis/grafeas/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func TestFilter(t *testing.T) {
	o := &pb.Occurrence{Name: "projects/p/occurrences/1", ResourceUri: "https://us-docker.pkg.dev/p/r/i@sha256:1",
		NoteName: "projects/goog-vulnz/notes/CVE-1", Kind: pb.NoteKind_VULNERABILITY,
		Details: &pb.Occurrence_Vulnerability{Vulnerability: &pb.VulnerabilityOccurrence{Severity: pb.Severity_HIGH, FixAvailable: true,
			PackageIssue: []*pb.VulnerabilityOccurrence_PackageIssue{{AffectedPackage: "zlib"}}}}}
	for _, c := range []struct {
		f    string
		want bool
	}{
		{``, true},
		{`kind="VULNERABILITY"`, true},
		{`kind = "DISCOVERY"`, false},
		{`(kind="BUILD") OR (kind="VULNERABILITY")`, true},
		{`kind="VULNERABILITY" AND resourceUrl="https://us-docker.pkg.dev/p/r/i@sha256:1"`, true},
		{`((kind="VULNERABILITY")) AND ((resource_url="x") OR (resourceUrl="https://us-docker.pkg.dev/p/r/i@sha256:1"))`, true},
		{`has_prefix(resourceUrl, "https://us-docker.pkg.dev/p/")`, true},
		{`has_prefix(resourceUrl, "https://other/") OR has_prefix(resourceUrl, "us-docker")`, false},
		{`noteProjectId="goog-vulnz" AND noteId="CVE-1"`, true},
		{`NOT kind="VULNERABILITY"`, false},
		{`dependencyPackageName="zlib" severity="HIGH"`, true},
		{`fixAvailable=true`, true},
	} {
		f, want := c.f, c.want
		p, err := parseFilter(f, knownOccurrenceField)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if got := p.match(occurrenceFields(o)); got != want {
			t.Errorf("%s: %v, want %v", f, got, want)
		}
	}
	for _, bad := range []string{`nope="x"`, `kind=`, `(kind="X"`, `has_prefix(kind "x")`, `"x"`} {
		if _, err := parseFilter(bad, knownOccurrenceField); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestNotesOccurrencesAndSummary(t *testing.T) {
	saved := 0
	st := NewStore(func([]byte) error { saved++; return nil })
	s := NewServer(st)
	ctx := context.Background()
	vn := &pb.Note{Type: &pb.Note_Vulnerability{Vulnerability: &pb.VulnerabilityNote{Severity: pb.Severity_HIGH}}}
	if _, err := s.CreateNote(ctx, &pb.CreateNoteRequest{Parent: "projects/p", NoteId: "CVE-1", Note: vn}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNote(ctx, &pb.CreateNoteRequest{Parent: "projects/p", NoteId: "CVE-1", Note: vn}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := s.CreateNote(ctx, &pb.CreateNoteRequest{Parent: "folders/f", NoteId: "x", Note: vn}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad parent: %v", err)
	}
	occ := func(uri string, sev pb.Severity, fix bool) *pb.Occurrence {
		return &pb.Occurrence{ResourceUri: uri, NoteName: "projects/p/notes/CVE-1",
			Details: &pb.Occurrence_Vulnerability{Vulnerability: &pb.VulnerabilityOccurrence{EffectiveSeverity: sev, FixAvailable: fix}}}
	}
	b, err := s.BatchCreateOccurrences(ctx, &pb.BatchCreateOccurrencesRequest{Parent: "projects/p", Occurrences: []*pb.Occurrence{
		occ("a", pb.Severity_HIGH, true), occ("a", pb.Severity_HIGH, false), occ("a", pb.Severity_LOW, false), occ("b", pb.Severity_CRITICAL, true)}})
	if err != nil || len(b.GetOccurrences()) != 4 {
		t.Fatalf("batch: %v %v", b, err)
	}
	if _, err := s.CreateOccurrence(ctx, &pb.CreateOccurrenceRequest{Parent: "projects/p", Occurrence: &pb.Occurrence{ResourceUri: "a",
		NoteName: "projects/p/notes/CVE-1", Details: &pb.Occurrence_Build{Build: &pb.BuildOccurrence{}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("kind mismatch: %v", err)
	}
	l1, err := s.ListOccurrences(ctx, &pb.ListOccurrencesRequest{Parent: "projects/p", PageSize: 3})
	if err != nil || len(l1.GetOccurrences()) != 3 || l1.GetNextPageToken() == "" {
		t.Fatalf("page 1: %v %v", l1, err)
	}
	l2, err := s.ListOccurrences(ctx, &pb.ListOccurrencesRequest{Parent: "projects/p", PageSize: 3, PageToken: l1.GetNextPageToken()})
	if err != nil || len(l2.GetOccurrences()) != 1 || l2.GetNextPageToken() != "" {
		t.Fatalf("page 2: %v %v", l2, err)
	}
	sum, err := (&CA{st: st}).GetVulnerabilityOccurrencesSummary(ctx, &capb.GetVulnerabilityOccurrencesSummaryRequest{Parent: "projects/p", Filter: `resourceUrl="a"`})
	if err != nil || len(sum.GetCounts()) != 2 {
		t.Fatalf("summary: %v %v", sum, err)
	}
	if c := sum.GetCounts()[0]; c.GetSeverity() != pb.Severity_HIGH || c.GetTotalCount() != 2 || c.GetFixableCount() != 1 {
		t.Errorf("HIGH count: %v", c)
	}
	no, err := s.ListNoteOccurrences(ctx, &pb.ListNoteOccurrencesRequest{Name: "projects/p/notes/CVE-1", PageSize: 100})
	if err != nil || len(no.GetOccurrences()) != 4 {
		t.Errorf("note occurrences: %v %v", no, err)
	}
	name := b.GetOccurrences()[0].GetName()
	u, err := s.UpdateOccurrence(ctx, &pb.UpdateOccurrenceRequest{Name: name, Occurrence: &pb.Occurrence{Remediation: "upgrade"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"remediation"}}})
	if err != nil || u.GetRemediation() != "upgrade" || u.GetVulnerability() == nil {
		t.Errorf("update: %v %v", u, err)
	}
	if _, err := s.DeleteOccurrence(ctx, &pb.DeleteOccurrenceRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetOccurrence(ctx, &pb.GetOccurrenceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted: %v", err)
	}
	// The store reloads what it saved.
	var raw []byte
	st.save = func(b []byte) error { raw = b; return nil }
	st.PutNote(&pb.Note{Name: "projects/goog-vulnz/notes/CVE-2", Type: vn.Type})
	st2 := NewStore(nil)
	if err := st2.Load(raw); err != nil {
		t.Fatal(err)
	}
	if len(st2.notes) != 2 || len(st2.occs) != 3 || saved == 0 {
		t.Errorf("reloaded %d notes, %d occurrences", len(st2.notes), len(st2.occs))
	}
}
