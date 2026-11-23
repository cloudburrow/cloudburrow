package vertexai

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/grpc/codes"
)

// wordDoc is a one-page Document of text, a token a word, each word 10
// pixels high on its line and 10 pixels wide a rune, as ocrDocument lays
// one out.
func wordDoc(text string) *documentaipb.Document {
	page := &documentaipb.Document_Page{PageNumber: 1, Dimension: &documentaipb.Document_Page_Dimension{Width: 1000, Height: 1000, Unit: "pixels"},
		Layout: &documentaipb.Document_Page_Layout{TextAnchor: anchor(0, int64(len([]rune(text))))}}
	runes := []rune(text)
	line, col := 0, 0
	for i := 0; i < len(runes); {
		if runes[i] == '\n' {
			line, col = line+1, 0
			i++
			continue
		}
		if runes[i] == ' ' {
			col++
			i++
			continue
		}
		j := i
		for j < len(runes) && runes[j] != ' ' && runes[j] != '\n' {
			j++
		}
		x0, y0 := col*10, line*20
		x1, y1 := (col+j-i)*10, line*20+10
		page.Tokens = append(page.Tokens, &documentaipb.Document_Page_Token{Layout: &documentaipb.Document_Page_Layout{
			TextAnchor: anchor(int64(i), int64(j)), Confidence: 0.9,
			BoundingPoly: &documentaipb.BoundingPoly{Vertices: []*documentaipb.Vertex{{X: int32(x0), Y: int32(y0)}, {X: int32(x1), Y: int32(y0)},
				{X: int32(x1), Y: int32(y1)}, {X: int32(x0), Y: int32(y1)}}}}})
		col += j - i
		i = j
	}
	return &documentaipb.Document{Text: text, Pages: []*documentaipb.Document_Page{page}}
}

func spanText(d *documentaipb.Document, l *documentaipb.Document_Page_Layout) string {
	st, en := segment(l.GetTextAnchor())
	return string([]rune(d.GetText())[st:en])
}

// TestDocumentAIFormParser (#1799): the Form Parser's answers from the
// model are anchored in the OCR text, with token boxes: a key-value pair,
// a checkbox, a table and generic entities; values the text does not hold
// are dropped. Without a model the type is UNIMPLEMENTED.
func TestDocumentAIFormParser(t *testing.T) {
	ctx := context.Background()
	d := newDocAI(t, t.TempDir()+"/processors.json")
	if err := d.s.prebuiltReady("FORM_PARSER_PROCESSOR"); code(grpcError(err)) != codes.Unimplemented {
		t.Fatalf("without a model: %v", err)
	}
	for _, n := range d.s.docAITypeNames() {
		if isPrebuiltType(n) {
			t.Errorf("%s listed without a model", n)
		}
	}
	gen := &docGen{answer: func(p, schema string) string {
		return `{"fields":[{"key":"Name:","value":"Jane Doe","checkbox":false,"checked":false,"confidence":0.9},` +
			`{"key":"Member","value":"","checkbox":true,"checked":true,"confidence":0.8},` +
			`{"key":"Ghost:","value":"x","checkbox":false,"checked":false,"confidence":0.5}],` +
			`"tables":[{"header":["Item","Qty"],"rows":[["Apples","3"],["Pears","5"]]}],` +
			`"entities":[{"type":"email","value":"jane@example.com","confidence":0.9},{"type":"person","value":"Nobody Here","confidence":0.9}]}`
	}}
	d.s.gen = gen
	if err := d.s.prebuiltReady("FORM_PARSER_PROCESSOR"); err != nil {
		t.Fatal(err)
	}
	doc := wordDoc("Name: Jane Doe\nEmail: jane@example.com\n[x] Member\nItem Qty\nApples 3\nPears 5\n")
	if err := d.s.applyPrebuilt(ctx, "FORM_PARSER_PROCESSOR", doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gen.prompts[0], "Name: Jane Doe") {
		t.Errorf("prompt: %q", gen.prompts[0])
	}
	pg := doc.GetPages()[0]
	if len(pg.GetFormFields()) != 2 {
		t.Fatalf("form fields: %v", pg.GetFormFields())
	}
	ff := pg.GetFormFields()[0]
	if spanText(doc, ff.GetFieldName()) != "Name:" || spanText(doc, ff.GetFieldValue()) != "Jane Doe" || ff.GetValueType() != "" {
		t.Errorf("field: %v", ff)
	}
	if v := ff.GetFieldValue().GetBoundingPoly().GetVertices(); len(v) != 4 || v[0].GetX() != 60 || v[2].GetX() != 140 {
		t.Errorf("value box: %v", v)
	}
	if nv := ff.GetFieldValue().GetBoundingPoly().GetNormalizedVertices(); len(nv) != 4 || nv[0].GetX() != 0.06 {
		t.Errorf("value normalized box: %v", nv)
	}
	if cb := pg.GetFormFields()[1]; cb.GetValueType() != "filled_checkbox" || spanText(doc, cb.GetFieldName()) != "Member" {
		t.Errorf("checkbox: %v", cb)
	}
	if len(pg.GetTables()) != 1 {
		t.Fatalf("tables: %v", pg.GetTables())
	}
	tb := pg.GetTables()[0]
	if len(tb.GetHeaderRows()) != 1 || len(tb.GetBodyRows()) != 2 || spanText(doc, tb.GetBodyRows()[1].GetCells()[1].GetLayout()) != "5" ||
		tb.GetBodyRows()[0].GetCells()[0].GetRowSpan() != 1 || spanText(doc, tb.GetLayout()) != "Item Qty\nApples 3\nPears 5" {
		t.Errorf("table: %v", tb)
	}
	if len(doc.GetEntities()) != 1 || doc.GetEntities()[0].GetType() != "email" || doc.GetEntities()[0].GetMentionText() != "jane@example.com" ||
		doc.GetEntities()[0].GetId() != "0" || len(doc.GetEntities()[0].GetPageAnchor().GetPageRefs()) != 1 {
		t.Errorf("entities: %v", doc.GetEntities())
	}
}

// TestDocumentAISpecializedExtractor (#1799): an Invoice Parser's answer:
// top-level fields anchored and normalized (a date, an amount), a line
// item with its properties (one anchored ignoring case, so less
// confident), values not in the text dropped; the version's
// schema names the documented types.
func TestDocumentAISpecializedExtractor(t *testing.T) {
	ctx := context.Background()
	d := newDocAI(t, t.TempDir()+"/processors.json")
	gen := &docGen{answer: func(p, schema string) string {
		if !strings.Contains(schema, `"line_item":{"items":{"additionalProperties":false,"properties":{"amount":{"type":["string","null"]}`) ||
			!strings.Contains(schema, `"required":["amount_paid_since_last_invoice",`) || !strings.Contains(schema, `"x-guidance":{"whitespace_flexible":false}`) ||
			// The fields an invoice states first, the rest after them.
			!strings.Contains(schema, `"properties":{"invoice_id":{"type":["string","null"]},"invoice_date":`) ||
			!strings.Contains(schema, `"total_tax_amount":{"type":["string","null"]},"line_item":{"items"`) ||
			!strings.Contains(schema, `"type":"array"},"amount_paid_since_last_invoice":`) {
			t.Errorf("schema: %s", schema)
		}
		return `{"carrier":null,"invoice_date":"2026-03-14","invoice_id":"INV-42","line_item":[{"amount":"1,234.50","description":"widgets","unit":null}],` +
			`"supplier_name":"Not There Inc","total_amount":"$1,234.50"}`
	}}
	d.s.gen = gen
	doc := wordDoc("Invoice INV-42\nDate 2026-03-14\nWidgets $1,234.50\nTotal $1,234.50\n")
	if err := d.s.applyPrebuilt(ctx, "INVOICE_PROCESSOR", doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gen.prompts[0], "line_item (a list: one object for each, with amount, description") ||
		!strings.Contains(gen.prompts[0], "Fields: invoice_id, invoice_date, supplier_name, ") ||
		len(gen.prompts) != 1 {
		t.Errorf("prompt: %q", gen.prompts[0])
	}
	byType := map[string]*documentaipb.Document_Entity{}
	for _, e := range doc.GetEntities() {
		byType[e.GetType()] = e
	}
	if len(doc.GetEntities()) != 4 || byType["supplier_name"] != nil {
		t.Fatalf("entities: %v", doc.GetEntities())
	}
	if e := byType["invoice_date"]; e.GetNormalizedValue().GetText() != "2026-03-14" || e.GetNormalizedValue().GetDateValue().GetDay() != 14 {
		t.Errorf("date: %v", e)
	}
	if m := byType["total_amount"].GetNormalizedValue().GetMoneyValue(); m.GetUnits() != 1234 || m.GetNanos() != 500000000 || m.GetCurrencyCode() != "USD" {
		t.Errorf("money: %v", m)
	}
	li := byType["line_item"]
	if len(li.GetProperties()) != 2 || li.GetMentionText() != "Widgets $1,234.50" || li.GetProperties()[0].GetType() != "line_item/description" ||
		li.GetProperties()[0].GetConfidence() >= byType["invoice_id"].GetConfidence() {
		t.Errorf("line item: %v", li)
	}
	sc := prebuiltSchema("INVOICE_PROCESSOR")
	if len(sc.GetEntityTypes()) != 3 || sc.GetEntityTypes()[0].GetProperties()[0].GetName() != "amount_paid_since_last_invoice" {
		t.Errorf("schema: %v", sc)
	}
}

// TestTruncatedJSON: an answer cut off by the model's context keeps its
// complete array elements.
func TestTruncatedJSON(t *testing.T) {
	for in, want := range map[string]string{
		`{"entities":[{"type":"a","value":"x"},{"type":"b","val`:                        `{"entities":[{"type":"a","value":"x"}]}`,
		`{"entities":[{"type":"a"}],"items":[{"type":"li","fields":[{"v":"1"},{"v":"2"`: `{"entities":[{"type":"a"}],"items":[{"type":"li","fields":[{"v":"1"}]}]}`,
		`{"e":["a,]b", "c`: `{"e":["a,]b"]}`,
	} {
		if got, ok := truncatedJSON(in); !ok || got != want {
			t.Errorf("truncatedJSON(%s) = %s %v, want %s", in, got, ok, want)
		}
	}
	if _, ok := truncatedJSON(`{"a":"b`); ok {
		t.Error("no complete element, repaired")
	}
}
