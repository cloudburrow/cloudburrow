package main

import (
	"context"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/protobuf/encoding/protojson"
)

// docAIObjects is an in-memory bucket store for the local AI endpoint.
type docAIObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
	ct   map[string]string
}

func (m *docAIObjects) Read(_ context.Context, bucket, object string, _ int64) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[bucket+"/"+object]
	if !ok {
		return nil, "", errors.New("not found")
	}
	return b, m.ct[bucket+"/"+object], nil
}

func (m *docAIObjects) Write(_ context.Context, bucket, object, ct string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[bucket+"/"+object], m.ct[bucket+"/"+object] = b, ct
	return nil
}

func (m *docAIObjects) List(_ context.Context, bucket, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if n, ok := strings.CutPrefix(k, bucket+"/"); ok && strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// TestConsoleDocumentAICustomProcessors (#2124): a Custom Extractor's
// page through the official clients on the local AI endpoint: Get
// started's Create new field and Edit document prompt, Build's Configure
// dataset and Import documents (a scan into training, a labeled Document
// JSON into test), the labeling page (the scan's page image) and a label
// from a box over the invoice number's words, Filter documents, Create
// new version (a counting model: no local generation model here), its
// evaluation's page at the optimal and a chosen threshold, Deploy & use,
// Assign to test and Delete.
func TestConsoleDocumentAICustomProcessors(t *testing.T) {
	ctx := context.Background()
	s := localAIForConsole(t)
	store := &docAIObjects{objs: map[string][]byte{}, ct: map[string]string{}}
	s.UseStorage(store)
	scan, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Write(ctx, "b", "train/scan.png", "image/png", scan)
	text := "INVOICE No. 2026-0999\nDate: 2 April 2026\n"
	test, _ := protojson.Marshal(&documentaipb.Document{Text: text, Entities: []*documentaipb.Document_Entity{{Type: "invoice_id",
		MentionText: "2026-0999", TextAnchor: &documentaipb.Document_TextAnchor{TextSegments: []*documentaipb.Document_TextAnchor_TextSegment{{StartIndex: 12, EndIndex: 21}}}}}})
	_ = store.Write(ctx, "b", "test/a.json", "application/json", test)

	d := docAI{defaultProject: "p1", addr: s.Addr}
	procs := docAIProcessorsProvider{d: d, labels: s}
	id, err := procs.Create(ctx, "p1", map[string]string{"type": "CUSTOM_EXTRACTION_PROCESSOR", "name": "ext", "region": "us"})
	if err != nil {
		t.Fatal(err)
	}
	act := func(path []string, action string, v map[string]string) string {
		t.Helper()
		out, err := procs.ActAtResult(ctx, "p1", path, action, v)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if out == nil {
			return ""
		}
		var b strings.Builder
		for _, r := range out.Items {
			b.WriteString(r.Name + " ")
			for _, f := range r.Fields {
				b.WriteString(f + " ")
			}
		}
		return b.String() + out.Open
	}
	tabs := func(path ...string) map[string]string {
		t.Helper()
		det, err := procs.Detail(ctx, "p1", path)
		if err != nil || det.Unavailable != "" {
			t.Fatalf("Detail %v: %v %s", path, err, det.Unavailable)
		}
		out := map[string]string{}
		for _, sec := range det.Sections {
			var b strings.Builder
			b.WriteString(sec.Label + "|" + sec.Note + "|" + sec.Text + "|" + sec.Unavailable)
			for _, r := range sec.Listing.Items {
				b.WriteString("|" + r.Name + " " + r.Link)
				for _, f := range r.Fields {
					b.WriteString(" " + f)
				}
			}
			out[sec.ID] = b.String()
		}
		return out
	}
	got := tabs(id)
	for _, want := range []string{"overview", "get-started", "build", "versions", "evaluate"} {
		if got[want] == "" {
			t.Fatalf("the custom processor's page has no %s tab: %v", want, got)
		}
	}
	if !strings.HasPrefix(got["versions"], "Deploy & use") {
		t.Errorf("versions tab %q", got["versions"])
	}
	act([]string{id}, "add-field", map[string]string{"name": "invoice_id", "valueType": "string", "occurrence": "REQUIRED_ONCE"})
	act([]string{id}, "edit-prompt", map[string]string{"prompt": "Supplier invoices"})
	if got = tabs(id); !strings.Contains(got["get-started"], "invoice_id") || !strings.Contains(got["get-started"], "Supplier invoices") {
		t.Errorf("Get started after its actions: %q", got["get-started"])
	}
	act([]string{id}, "configure-dataset", map[string]string{"storage": "managed", "gcsPrefix": "gs://b/ds"})
	if r := act([]string{id}, "import-documents", map[string]string{"source": "gs://b/train/", "split": "DATASET_SPLIT_TRAIN"}); !strings.Contains(r, "Imported") {
		t.Fatalf("Import documents: %q", r)
	}
	act([]string{id}, "import-documents", map[string]string{"source": "gs://b/test/a.json", "split": "DATASET_SPLIT_TEST"})
	pr, _ := d.processor(ctx, "p1", id)
	ds, err := d.dataset(ctx, pr, "")
	if err != nil || len(ds.docs) != 2 {
		t.Fatalf("dataset: %+v %v", ds.docs, err)
	}
	scanID := ""
	for _, m := range ds.docs {
		if m.GetDisplayName() == "scan.png" {
			scanID = m.GetDocumentId().GetGcsManagedDocId().GetCwDocId()
		}
	}
	// The labeling page, and a box drawn over the invoice number.
	det, _ := procs.Detail(ctx, "p1", []string{id, docAIDocumentSegment, scanID})
	if det.Unavailable != "" || len(det.Sections) != 2 || len(det.Sections[0].Listing.Pages) != 1 || det.Sections[0].LabelAction != "add-label" {
		t.Fatalf("labeling page: %+v", det)
	}
	doc, _, _ := d.document(ctx, pr, scanID)
	var box [4]float32
	for _, tk := range doc.GetPages()[0].GetTokens() {
		sg := tk.GetLayout().GetTextAnchor().GetTextSegments()[0]
		if strings.HasPrefix(doc.GetText()[sg.GetStartIndex():sg.GetEndIndex()], "2026-0417") {
			v := tk.GetLayout().GetBoundingPoly().GetNormalizedVertices()
			box = [4]float32{v[0].GetX(), v[0].GetY(), v[2].GetX() - v[0].GetX(), v[2].GetY() - v[0].GetY()}
		}
	}
	f := func(x float32) string { return strconv.FormatFloat(float64(x), 'f', 4, 64) }
	act([]string{id, docAIDocumentSegment, scanID}, "add-label", map[string]string{"type": "invoice_id", "page": "1",
		"x": f(box[0] - 0.005), "y": f(box[1] - 0.005), "w": f(box[2] + 0.01), "h": f(box[3] + 0.01)})
	doc, _, _ = d.document(ctx, pr, scanID)
	if len(doc.GetEntities()) != 1 || !strings.HasPrefix(doc.GetEntities()[0].GetMentionText(), "2026-0417") {
		t.Fatalf("the box's label: %v", doc.GetEntities())
	}
	if _, err := procs.ActAtResult(ctx, "p1", []string{id, docAIDocumentSegment, scanID}, "add-label",
		map[string]string{"type": "invoice_id", "value": "not in the text"}); err == nil {
		t.Error("a label of a value the text does not have")
	}
	if r := act([]string{id}, "filter-documents", map[string]string{"state": "DOCUMENT_LABELED", "split": "DATASET_SPLIT_TRAIN"}); !strings.Contains(r, "scan.png") || strings.Contains(r, "a.json") {
		t.Errorf("Filter documents: %q", r)
	}

	// Create new version, its evaluation, Deploy & use.
	if r := act([]string{id}, "train-version", map[string]string{"name": "v1"}); !strings.Contains(r, "Counting model") || !strings.Contains(r, "v1") {
		t.Fatalf("Create new version: %q", r)
	}
	vers, _ := d.versions(ctx, pr.GetName())
	vid := lastSegment(vers[0].GetName())
	got = tabs(id)
	if !strings.Contains(got["evaluate"], "/evaluations/"+vid+"/") || !strings.Contains(got["versions"], "Counting model") {
		t.Fatalf("Evaluate & test and Deploy & use after training: %q %q", got["evaluate"], got["versions"])
	}
	eid := lastSegment(vers[0].GetLatestEvaluation().GetEvaluation())
	ev := tabs(id, docAIEvaluationSegment, vid, eid)
	if !strings.Contains(ev["labels"], "All labels") || !strings.Contains(ev["labels"], "invoice_id") || len(strings.Split(ev["thresholds"], "|")) < 21 {
		t.Errorf("evaluation page: %v", ev)
	}
	if r := act([]string{id, docAIEvaluationSegment, vid, eid}, "set-threshold", map[string]string{"threshold": "0.50"}); !strings.HasSuffix(strings.Split(r, "?")[0], "/0.50") {
		t.Errorf("Confidence threshold opens %q", r)
	}
	det, _ = procs.Detail(ctx, "p1", []string{id, docAIEvaluationSegment, vid, eid, "0.50"})
	if !strings.Contains(det.Summary[4].Value, "0.50, chosen") {
		t.Errorf("threshold: %+v", det.Summary[4])
	}
	if r := act([]string{id}, "evaluate-version", map[string]string{"version": vid}); !strings.Contains(r, "/evaluations/"+vid+"/") {
		t.Errorf("Run new evaluation opens %q", r)
	}
	if err := procs.ActAt(ctx, "p1", []string{id}, "deploy-versions", map[string]string{docAIVersionsField: vid}); err != nil {
		t.Fatal(err)
	}
	if err := procs.ActAt(ctx, "p1", []string{id}, "set-default-version", map[string]string{"version": vid}); err != nil {
		t.Fatal(err)
	}
	act([]string{id}, "assign-test", map[string]string{docAIVersionsField: "scan.png"})
	if r := act([]string{id}, "filter-documents", map[string]string{"split": "DATASET_SPLIT_TEST"}); !strings.Contains(r, "scan.png") {
		t.Errorf("after Assign to test: %q", r)
	}
	act([]string{id}, "delete-documents", map[string]string{docAIVersionsField: "a.json"})
	if ds, _ = d.dataset(ctx, pr, ""); len(ds.docs) != 1 {
		t.Errorf("after Delete: %d documents", len(ds.docs))
	}
	// Every tab action is offered for the page.
	have := map[string]bool{}
	for _, a := range procs.DetailActions(ctx, "p1", []string{id}) {
		have[a.ID] = true
	}
	for _, want := range []string{"add-field", "edit-prompt", "configure-dataset", "import-documents", "filter-documents", "train-version",
		"evaluate-version", "delete-documents", "assign-train", "delete-fields", "deploy-versions"} {
		if !have[want] {
			t.Errorf("DetailActions has no %s", want)
		}
	}
}
