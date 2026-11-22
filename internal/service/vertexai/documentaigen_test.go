package vertexai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
)

// docGen is a generator standing in for the local model: it answers each
// request from the prompt with answer, and keeps the prompts.
type docGen struct {
	mu      sync.Mutex
	prompts []string
	answer  func(prompt, schema string) string
}

func (g *docGen) Model() string        { return "test-model" }
func (g *docGen) HonoursOptions() bool { return true }

func (g *docGen) Generate(_ context.Context, req Request) (<-chan Chunk, func() error, error) {
	p := req.Prompt
	if req.Conversation != nil {
		b, _ := json.Marshal(req.Conversation.Message)
		p += string(b)
	}
	g.mu.Lock()
	g.prompts = append(g.prompts, p)
	g.mu.Unlock()
	out := make(chan Chunk, 2)
	out <- Chunk{Text: g.answer(p, req.JSONSchema)}
	out <- Chunk{Last: true, FinishReason: FinishReasonStop}
	close(out)
	return out, func() error { return nil }, nil
}

// lastDoc is the target document's text in a prompt, unescaped.
func lastDoc(p string) string {
	p = strings.ReplaceAll(p, `\n`, "\n")
	i := strings.LastIndex(p, "Document:\n")
	if i < 0 {
		i = strings.LastIndex(p, "File:\n")
	}
	return p[i:]
}

// TestDocumentAIGenerativeCustom (#2124): with a local model served, a
// Custom Extractor trains zero-shot from the schema alone and few-shot
// from labeled documents (their values in the prompt), and a classifier
// reads its labeled documents as examples; the evaluation scores the
// model's answers on the test split; without a model the counting
// models answer.
func TestDocumentAIGenerativeCustom(t *testing.T) {
	ctx := context.Background()
	gen := &docGen{answer: func(p, schema string) string {
		doc := lastDoc(p)
		switch {
		case strings.Contains(schema, `"pages"`):
			return `{"pages":[{"class":"invoice","starts":true,"confidence":0.9},{"class":"receipt","starts":true,"confidence":0.8}]}`
		case strings.Contains(schema, `"class"`):
			c := "invoice"
			if strings.Contains(strings.ToLower(doc), "receipt") {
				c = "receipt"
			}
			return fmt.Sprintf(`{"class":%q,"confidence":0.9}`, c)
		}
		// The extractor: the value after "Invoice number:" as the model would.
		i := strings.Index(doc, "Invoice number: ")
		if i < 0 {
			return `{"entities":[]}`
		}
		v := strings.Fields(doc[i+len("Invoice number: "):])[0]
		return fmt.Sprintf(`{"entities":[{"type":"invoice_id","value":%q,"confidence":0.95},{"type":"invoice_id","value":"NOT-IN-TEXT","confidence":0.5}]}`, strings.ToLower(v))
	}}
	d := newDocAI(t, t.TempDir()+"/processors.json")
	d.s.gen = gen
	schema := &documentaipb.DocumentSchema{Description: "Supplier invoices", EntityTypes: []*documentaipb.DocumentSchema_EntityType{{Name: "invoice",
		BaseTypes: []string{"document"}, Properties: []*documentaipb.DocumentSchema_EntityType_Property{{Name: "invoice_id", ValueType: "string",
			OccurrenceType: documentaipb.DocumentSchema_EntityType_Property_REQUIRED_ONCE}}}}}
	ext := createProcessor(t, d, "CUSTOM_EXTRACTION_PROCESSOR", "ext")
	train := func(p *documentaipb.Processor, sc *documentaipb.DocumentSchema, tr, te []*documentaipb.Document) *customVersion {
		t.Helper()
		s := d.s
		s.mu.Lock()
		c := s.docAI().customOf(p.GetName())
		c.DatasetState = "INITIALIZED"
		for i, doc := range tr {
			c.Docs = append(c.Docs, &datasetDoc{ID: fmt.Sprint("tr", i), Split: "DATASET_SPLIT_TRAIN", Doc: doc})
		}
		for i, doc := range te {
			c.Docs = append(c.Docs, &datasetDoc{ID: fmt.Sprint("te", i), Split: "DATASET_SPLIT_TEST", Doc: doc})
		}
		s.mu.Unlock()
		op, err := d.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: p.GetName(), DocumentSchema: sc,
			ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "v"}})
		if err != nil {
			t.Fatalf("TrainProcessorVersion: %v", err)
		}
		var r documentaipb.TrainProcessorVersionResponse
		_ = op.GetResponse().UnmarshalTo(&r)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.docAI().customOf(p.GetName()).version(r.GetProcessorVersion())
	}
	inv := func(id string) *documentaipb.Document {
		return labeled("ACME\nInvoice number: "+id+"\nTotal: 1.00\n", map[string]string{"invoice_id": id})
	}

	// Zero-shot: no labeled document, only the schema; evaluated on the test split.
	zero := train(ext, schema, nil, []*documentaipb.Document{inv("INV-9")})
	if !zero.Model.Generative || len(zero.Model.Shots) != 0 || zero.Proto.GetModelType() != documentaipb.ProcessorVersion_MODEL_TYPE_GENERATIVE {
		t.Fatalf("zero-shot version: %+v %v", zero.Model, zero.Proto)
	}
	if f1 := zero.Proto.GetLatestEvaluation().GetAggregateMetrics().GetF1Score(); f1 != 1 {
		t.Errorf("zero-shot F1 on the test split %v, want 1 (the value not in the text dropped, case kept from the text)", f1)
	}
	if p := gen.prompts[len(gen.prompts)-1]; !strings.Contains(p, "invoice_id") || !strings.Contains(p, "Supplier invoices") || strings.Contains(p, "Example 1") {
		t.Errorf("zero-shot prompt: %s", p)
	}
	ents, err := d.s.docPredict(ctx, zero.Model, &documentaipb.Document{Text: "Invoice number: AB-12 due"}, schema)
	if err != nil || len(ents) != 1 || ents[0].GetMentionText() != "AB-12" || ents[0].GetTextAnchor().GetTextSegments()[0].GetStartIndex() != 16 {
		t.Fatalf("zero-shot extraction: %v %v", ents, err)
	}

	// Few-shot: the labeled training documents are the examples.
	few := train(createProcessor(t, d, "CUSTOM_EXTRACTION_PROCESSOR", "few"), schema, []*documentaipb.Document{inv("INV-1"), inv("INV-2")},
		[]*documentaipb.Document{inv("INV-3")})
	if len(few.Model.Shots) != 2 || few.Model.Shots[0].Fields[0].Value != "INV-1" {
		t.Fatalf("few-shot examples: %+v", few.Model.Shots)
	}
	if p := gen.prompts[len(gen.prompts)-1]; !strings.Contains(p, "Example 2") || !strings.Contains(p, "INV-2") {
		t.Errorf("few-shot prompt has no examples: %s", p)
	}

	// The classifier: labeled examples of each class, the model's answer an entity per class.
	cs := &documentaipb.DocumentSchema{EntityTypes: []*documentaipb.DocumentSchema_EntityType{{Name: "invoice"}, {Name: "receipt"}}}
	cv := train(createProcessor(t, d, "CUSTOM_CLASSIFICATION_PROCESSOR", "cls"), cs,
		[]*documentaipb.Document{classDoc("Invoice amount due", "invoice"), classDoc("Receipt thank you", "receipt")},
		[]*documentaipb.Document{classDoc("Invoice payment terms", "invoice"), classDoc("Receipt cash change", "receipt")})
	if len(cv.Model.Shots) != 2 || cv.Proto.GetLatestEvaluation().GetAggregateMetrics().GetF1Score() != 1 {
		t.Fatalf("generative classifier: %+v %v", cv.Model.Shots, cv.Proto.GetLatestEvaluation())
	}
	ents, err = d.s.docPredict(ctx, cv.Model, &documentaipb.Document{Text: "RECEIPT paid"}, cs)
	if err != nil || len(ents) != 2 || ents[0].GetType() != "receipt" || ents[0].GetConfidence() < 0.89 || ents[1].GetConfidence() > 0.11 {
		t.Fatalf("generative classification: %v %v", ents, err)
	}

	// The splitter: each page's class and start.
	pk := &documentaipb.Document{Text: "Invoice\nReceipt\n", Pages: []*documentaipb.Document_Page{
		{PageNumber: 1, Layout: &documentaipb.Document_Page_Layout{TextAnchor: anchor(0, 8)}},
		{PageNumber: 2, Layout: &documentaipb.Document_Page_Layout{TextAnchor: anchor(8, 16)}}}}
	split, err := d.s.genSplit(ctx, &docModel{Kind: "splitter", Generative: true}, pk, []string{"invoice", "receipt"})
	if err != nil || len(split) != 2 || split[1].GetType() != "receipt" || split[1].GetPageAnchor().GetPageRefs()[0].GetPage() != 1 {
		t.Fatalf("generative split: %v %v", split, err)
	}

	// Without a local model the version's counting model answers, and a
	// new extractor needs a labeled training document again.
	d.s.gen = nil
	ents, err = d.s.docPredict(ctx, few.Model, &documentaipb.Document{Text: "ACME\nInvoice number: INV-77\n"}, schema)
	if err != nil || len(ents) != 1 || ents[0].GetMentionText() != "INV-77" {
		t.Errorf("fallback extraction: %v %v", ents, err)
	}
	if _, err := d.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: createProcessor(t, d, "CUSTOM_EXTRACTION_PROCESSOR", "none").GetName(),
		DocumentSchema: schema, ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "v"}}); err == nil {
		t.Error("a zero-shot version trained with no local model")
	}
}

func TestFindFuzzy(t *testing.T) {
	text := []rune("Total:  $1,200.00,\nBill To\nACME   Corp")
	for v, want := range map[string]string{"$1,200.00": "$1,200.00", "acme corp": "ACME   Corp", "bill to": "Bill To"} {
		st, en, ok := findFuzzy(text, v)
		if !ok || string(text[st:en]) != want {
			t.Errorf("findFuzzy(%q) = %q %v", v, string(text[st:en]), ok)
		}
	}
	if _, _, ok := findFuzzy(text, "missing"); ok {
		t.Error("found a value not in the text")
	}
}
