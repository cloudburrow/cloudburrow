//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// formGenericTypes are the Form Parser's documented generic entities
// (https://docs.cloud.google.com/document-ai/docs/form-parser).
var formGenericTypes = map[string]bool{"email": true, "phone": true, "url": true, "date_time": true, "address": true, "person": true,
	"organization": true, "quantity": true, "price": true, "id": true, "page_number": true}

// samplePDFs are the test's generated documents.
var (
	sampleForm = textPDF([]string{"APPLICATION FORM", "Name: Jane Doe", "Email: jane.doe@example.com", "Phone: 555-0142",
		"Date of birth: 1990-04-12", "City: Springfield", "", "Item  Quantity  Price", "Apples  3  4.50", "Pears  5  2.25"})
	sampleLicense = textPDF([]string{"CALIFORNIA DRIVER LICENSE", "DL D7654321", "Family Name: DOE", "Given Names: JANE",
		"Date Of Birth: 04/12/1990", "Expiration Date: 04/12/2031"})
	sampleInvoice = textPDF([]string{"ACME SUPPLIES LTD", "INVOICE", "Invoice number: INV-2026-0417", "Invoice date: 2026-03-14",
		"Bill to: Jane Doe", "Widgets  2  $600.00", "Gadgets  1  $34.50", "Total: $634.50"})
)

// anchored is the text a layout's anchor slices out of the Document's text,
// and whether the anchor is in the text.
func anchored(doc *documentaipb.Document, a *documentaipb.Document_TextAnchor) (string, bool) {
	runes := []rune(doc.GetText())
	var b strings.Builder
	for _, s := range a.GetTextSegments() {
		if s.GetStartIndex() < 0 || s.GetEndIndex() > int64(len(runes)) || s.GetStartIndex() >= s.GetEndIndex() {
			return "", false
		}
		b.WriteString(string(runes[s.GetStartIndex():s.GetEndIndex()]))
	}
	return b.String(), len(a.GetTextSegments()) > 0
}

// generativeProcessor creates a processor of typ, deleted when the test
// ends, or is nil where the instance runs no local generation model
// (UNIMPLEMENTED, naming up --local-ai-model).
func generativeProcessor(t *testing.T, h *Harness, project, typ string) *documentaipb.Processor {
	t.Helper()
	g, _ := documentAIClients(t, h)
	p, err := g.CreateProcessor(h.Context(), &documentaipb.CreateProcessorRequest{Parent: "projects/" + project + "/locations/us",
		Processor: &documentaipb.Processor{Type: typ, DisplayName: strings.ToLower(typ) + "-" + h.Project()}})
	if status.Code(err) == codes.Unimplemented {
		if !strings.Contains(err.Error(), "--local-ai-model") {
			t.Fatalf("%s refused without naming the local model: %v", typ, err)
		}
		t.Logf("the instance runs no local generation model: %v", err)
		return nil
	}
	if err != nil {
		t.Fatalf("CreateProcessor %s: %v", typ, err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteProcessor(h.Context(), &documentaipb.DeleteProcessorRequest{Name: p.GetName()}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	return p
}

// TestDocumentAIFormParserAndExtractors (#1799): the Form Parser and the
// Invoice Parser through the official cloud.google.com/go/documentai
// clients (gRPC and REST), on the local Gemma model with Tesseract's page
// layout: the types are listed; the Form Parser's processDocument of a
// generated form answers the OCR text and pages plus formFields (name and
// value layouts whose anchors slice the text and that carry boxes),
// tables and generic entities of the documented types; the Invoice
// Parser's version reports its document schema and its entities are of
// it, anchored, a date normalized. The values are the model's, whose
// quality Google's pages give no measure for: they are logged and their
// shape asserted, and the form's "Jane Doe" and the invoice's ID (as OCR
// spaced it) must be found.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument, google.cloud.documentai.v1.DocumentProcessorService/GetProcessorVersion
func TestDocumentAIFormParserAndExtractors(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	g, r := documentAIClients(t, h)
	project := h.Project()
	form := generativeProcessor(t, h, project, "FORM_PARSER_PROCESSOR")
	if form == nil {
		t.Skip("the Form Parser needs the local generation model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	types, err := g.FetchProcessorTypes(ctx, &documentaipb.FetchProcessorTypesRequest{Parent: "projects/" + project + "/locations/us"})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, pt := range types.GetProcessorTypes() {
		listed[pt.GetType()] = true
	}
	for _, want := range []string{"FORM_PARSER_PROCESSOR", "INVOICE_PROCESSOR", "EXPENSE_PROCESSOR", "US_DRIVER_LICENSE_PROCESSOR"} {
		if !listed[want] {
			t.Errorf("FetchProcessorTypes lacks %s: %v", want, listed)
		}
	}

	start := time.Now()
	resp, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: form.GetName(),
		Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: sampleForm, MimeType: "application/pdf"}}})
	if err != nil {
		t.Fatalf("ProcessDocument (Form Parser): %v", err)
	}
	doc := resp.GetDocument()
	t.Logf("Form Parser answered in %v", time.Since(start).Round(time.Second))
	if !strings.Contains(doc.GetText(), "Jane Doe") || len(doc.GetPages()) != 1 || len(doc.GetPages()[0].GetTokens()) == 0 {
		t.Fatalf("OCR text and pages: %q, %d pages", doc.GetText(), len(doc.GetPages()))
	}
	page := doc.GetPages()[0]
	if len(page.GetFormFields()) == 0 {
		t.Fatalf("no formFields")
	}
	jane := false
	for _, ff := range page.GetFormFields() {
		name, ok := anchored(doc, ff.GetFieldName().GetTextAnchor())
		if !ok || len(ff.GetFieldName().GetBoundingPoly().GetNormalizedVertices()) != 4 {
			t.Errorf("field name layout: %v", ff.GetFieldName())
		}
		value, _ := anchored(doc, ff.GetFieldValue().GetTextAnchor())
		if vt := ff.GetValueType(); vt != "" && vt != "filled_checkbox" && vt != "unfilled_checkbox" {
			t.Errorf("valueType %q", vt)
		}
		t.Logf("form field (recorded): %q = %q (%.2f)", name, value, ff.GetFieldValue().GetConfidence())
		jane = jane || strings.Contains(value, "Jane Doe")
	}
	if !jane {
		t.Errorf("no form field holds Jane Doe")
	}
	for _, tb := range page.GetTables() {
		for _, row := range append(tb.GetHeaderRows(), tb.GetBodyRows()...) {
			var cells []string
			for _, c := range row.GetCells() {
				s, _ := anchored(doc, c.GetLayout().GetTextAnchor())
				cells = append(cells, s)
				if c.GetRowSpan() != 1 || c.GetColSpan() != 1 {
					t.Errorf("cell spans: %v", c)
				}
			}
			t.Logf("table row (recorded): %q", cells)
		}
	}
	for _, e := range doc.GetEntities() {
		m, ok := anchored(doc, e.GetTextAnchor())
		if !formGenericTypes[e.GetType()] || !ok || m != e.GetMentionText() || len(e.GetPageAnchor().GetPageRefs()) != 1 {
			t.Errorf("generic entity: %v", e)
		}
		t.Logf("entity (recorded): %s %q", e.GetType(), e.GetMentionText())
	}

	// The Invoice Parser, over REST.
	inv := generativeProcessor(t, h, project, "INVOICE_PROCESSOR")
	v, err := r.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: inv.GetDefaultProcessorVersion()})
	if err != nil {
		t.Fatal(err)
	}
	schema := map[string]bool{}
	for _, et := range v.GetDocumentSchema().GetEntityTypes() {
		for _, p := range et.GetProperties() {
			schema[p.GetName()] = true
			schema[et.GetName()+"/"+p.GetName()] = true
		}
	}
	if !schema["invoice_id"] || !schema["line_item/amount"] {
		t.Fatalf("Invoice Parser schema: %v", v.GetDocumentSchema())
	}
	resp, err = r.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: inv.GetName(),
		Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: sampleInvoice, MimeType: "application/pdf"}}})
	if err != nil {
		t.Fatalf("ProcessDocument (Invoice Parser): %v", err)
	}
	doc = resp.GetDocument()
	id := false
	for _, e := range doc.GetEntities() {
		m, ok := anchored(doc, e.GetTextAnchor())
		if !schema[e.GetType()] || !ok || m != e.GetMentionText() {
			t.Errorf("invoice entity: %v", e)
		}
		for _, p := range e.GetProperties() {
			if !strings.HasPrefix(p.GetType(), e.GetType()+"/") || !schema[p.GetType()] {
				t.Errorf("property %s of %s", p.GetType(), e.GetType())
			}
		}
		if e.GetType() == "invoice_date" && e.GetNormalizedValue().GetDateValue().GetYear() != 2026 {
			t.Errorf("invoice_date not normalized: %v", e)
		}
		b, _ := protojson.Marshal(e.GetNormalizedValue())
		t.Logf("invoice entity (recorded): %s %q %s, %d properties", e.GetType(), e.GetMentionText(), b, len(e.GetProperties()))
		id = id || e.GetType() == "invoice_id" && strings.Contains(strings.ReplaceAll(e.GetMentionText(), " ", ""), "INV-2026-0417")
	}
	if !id {
		t.Errorf("no invoice_id INV-2026-0417")
	}
}

// longPDF is a generated PDF of n pages, each naming its number.
func longPDF(n int) []byte {
	var pages [][]string
	for i := 1; i <= n; i++ {
		pages = append(pages, []string{fmt.Sprintf("Chapter %d", i), fmt.Sprintf("This is page number %d of the report.", i)})
	}
	return textPDF(pages...)
}

// TestBigQueryMLProcessDocumentLongPDF (#1799): ML.PROCESS_DOCUMENT through
// the official cloud.google.com/go/bigquery client reads a 24-page PDF,
// over Document AI's online limit of 15, as BigQuery's documented limit of
// 130 pages allows: the layout parser's blocks reach page 24; a 131-page
// PDF is its row's error; Document AI's own processDocument of the 24-page
// PDF is still INVALID_ARGUMENT. Where the local generation model runs, a
// model over the US Driver License Parser (the extractor with the fewest
// fields, so the local model answers within BigQuery's 120 seconds) adds
// the processor fields, a column a top-level entity type ("Family Name" as
// Family_Name).
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument
func TestBigQueryMLProcessDocumentLongPDF(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	g, _ := documentAIClients(t, h)
	long := longPDF(24)
	c, ds, id := visionObjectTable(t, h, map[string][]byte{"img/long.pdf": long, "img/toolong.pdf": longPDF(131), "img/license.pdf": sampleLicense})
	conn := c.Project() + ".us." + id
	layout := createDocumentProcessor(t, h, g, c.Project(), "LAYOUT_PARSER_PROCESSOR")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: layout.GetName(),
		Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: long, MimeType: "application/pdf"}}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "exceed the limit: 15 got 24") {
		t.Errorf("online processDocument of 24 pages: %v", err)
	}

	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE MODEL `%s.layout` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_DOCUMENT_V1', DOCUMENT_PROCESSOR = '%s')",
		ds, conn, layout.GetName())); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	start := time.Now()
	rows := mlQueryIn(t, ctx, c, fmt.Sprintf("SELECT uri, ml_process_document_status AS status, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_process_document_result, '$.documentLayout.blocks')) AS blocks, "+
		"JSON_QUERY(ml_process_document_result, '$.documentLayout.blocks') AS raw "+
		"FROM ML.PROCESS_DOCUMENT(MODEL `%s.layout`, (SELECT uri, content_type FROM `%s.images` WHERE uri LIKE '%%long.pdf'))", ds, ds))
	t.Logf("ML.PROCESS_DOCUMENT of 24 and 131 pages in %v", time.Since(start).Round(time.Second))
	got := map[string]map[string]bigquery.Value{}
	for _, row := range rows {
		u := fmt.Sprint(row["uri"])
		got[u[strings.LastIndex(u, "/")+1:]] = row
	}
	lr := got["long.pdf"]
	raw := fmt.Sprint(lr["raw"])
	if lr == nil || lr["status"] != "" || lr["blocks"].(int64) < 24 || !strings.Contains(raw, "page number 24") ||
		!strings.Contains(raw, `"pageEnd":24`) {
		t.Errorf("24-page row: status %v, blocks %v", lr["status"], lr["blocks"])
	}
	if tl := got["toolong.pdf"]; tl == nil || !strings.Contains(fmt.Sprint(tl["status"]), "exceed the limit: 130 got 131") {
		t.Errorf("131-page row: %v", tl)
	}

	// The processor fields, where the extractors are served.
	dl := generativeProcessor(t, h, c.Project(), "US_DRIVER_LICENSE_PROCESSOR")
	if dl == nil {
		return
	}
	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE MODEL `%s.licenses` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_DOCUMENT_V1', DOCUMENT_PROCESSOR = '%s')",
		ds, conn, dl.GetName())); err != nil {
		t.Fatalf("CREATE MODEL over the US Driver License Parser: %v", err)
	}
	start = time.Now()
	rows = mlQueryIn(t, ctx, c, fmt.Sprintf("SELECT uri, ml_process_document_status AS status, Family_Name, Document_Id, Date_Of_Birth "+
		"FROM ML.PROCESS_DOCUMENT(MODEL `%s.licenses`, (SELECT uri, content_type FROM `%s.images` WHERE uri LIKE '%%license.pdf'))", ds, ds))
	t.Logf("license row (recorded) in %v: %v", time.Since(start).Round(time.Second), rows)
	if len(rows) != 1 || rows[0]["status"] != "" || !strings.Contains(fmt.Sprint(rows[0]["Family_Name"]), "DOE") {
		t.Errorf("license row: %v", rows)
	}
}
