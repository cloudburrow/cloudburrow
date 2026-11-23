package vertexai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/genproto/googleapis/type/money"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/ocr"
)

// The Form Parser and the specialized extractors (#1799), on the local
// generation model (the Gemma runtime documentaigen.go's generative custom
// processors run on, `up --local-ai-model`), with the page layout from
// local Tesseract OCR, as these pages document them (read 2026-11-21):
// https://docs.cloud.google.com/document-ai/docs/form-parser,
// https://docs.cloud.google.com/document-ai/docs/processors-list and
// https://docs.cloud.google.com/document-ai/docs/handle-response.
//
//   - FORM_PARSER_PROCESSOR answers the OCR processor's Document (text,
//     pages, blocks, paragraphs, lines, tokens) plus, a page each,
//     formFields ("key-value pairs": fieldName and fieldValue layouts, a
//     checkbox's valueType filled_checkbox or unfilled_checkbox) and tables
//     (headerRows and bodyRows of cells, rowSpan and colSpan 1, as Google's
//     "simple (no cells that span rows or columns) tables"), and the
//     documented generic entities (email, phone, url, date_time, address,
//     person, organization, quantity, price, id, page_number) in
//     Document.entities.
//   - The specialized extractors answer the OCR Document plus the entity
//     types their processors-list sections name (prebuiltFields), a parent
//     type such as line_item with its child properties (line_item/amount),
//     dates and money normalized (normalizedValue: dateValue, moneyValue).
//   - The model reads each page's OCR text (the Form Parser) or the
//     document's (the extractors), greedily (temperature 0, top-k 1), and
//     answers JSON constrained to a schema of the types; each value is
//     anchored where it first occurs in the text (case and whitespace
//     ignored, documentaigen.go's findFuzzy), its layout's boundingPoly the
//     union of the OCR tokens it spans, and a value that does not occur in
//     the text is dropped, so every answer points at the page.
//     An answer the model's context cuts off keeps its complete list
//     items (documentaigen.go's truncatedJSON).
//   - Without a local generation model these types are not listed, and
//     CreateProcessor, ProcessDocument and BatchProcessDocuments are
//     UNIMPLEMENTED, as before; CloudBurrow never calls Google.
//
// UNVERIFIED (re-read 2026-11-21: the pages document what is extracted, not
// how well): the quality of every answer, which is a small local model's,
// not Google's; the confidences (the Form Parser's the model's own, the extractors' their anchors' OCR confidence); the version IDs other
// than the W2 Parser's; that a checkbox's fieldValue has no box (Tesseract
// does not find checkboxes, only the model's reading of their text);
// the US Driver License Parser's Portrait, which is not extracted; Google's
// enrichment of supplier fields; normalizedValue.text's formats.

// prebuiltType is a served Form Parser or specialized extractor.
type prebuiltType struct {
	category, version, display string
	fields                     []prebuiltField
	// lead are the fields the model is asked for first, those such a
	// document nearly always states: the constrained decoder writes the
	// answer's fields in the schema's order, and greedy decoding that
	// began with a run of fields the document lacks (Google's names
	// sorted: amount_paid_since_last_invoice, carrier, currency, ...)
	// went on answering null for every field, invoice_id and
	// total_amount too, on the runner's CPU (main run 45102950103).
	lead []string
}

// asked is pt's fields in the order the model is asked for them: lead
// first, then the others in their declared order.
func (pt prebuiltType) asked() []prebuiltField {
	at := make(map[string]int, len(pt.lead))
	for i, n := range pt.lead {
		at[n] = i
	}
	out := append([]prebuiltField(nil), pt.fields...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := at[out[i].name]
		b, bok := at[out[j].name]
		switch {
		case aok && bok:
			return a < b
		case aok != bok:
			return aok
		}
		return false
	})
	return out
}

// prebuiltField is an entity type an extractor answers: kind is "date",
// "money" or "" (text); children are a parent's properties.
type prebuiltField struct {
	name, kind string
	multiple   bool
	children   []prebuiltField
}

func pf(name string) prebuiltField  { return prebuiltField{name: name} }
func pfd(name string) prebuiltField { return prebuiltField{name: name, kind: "date"} }
func pfm(name string) prebuiltField { return prebuiltField{name: name, kind: "money"} }
func pfp(name string, children ...prebuiltField) prebuiltField {
	for i := range children {
		children[i].name = name + "/" + children[i].name
	}
	return prebuiltField{name: name, multiple: true, children: children}
}

// formGenericTypes are the Form Parser's generic entities.
var formGenericTypes = []string{"email", "phone", "url", "date_time", "address", "person", "organization", "quantity", "price", "id", "page_number"}

var docAIPrebuilt = map[string]prebuiltType{
	"FORM_PARSER_PROCESSOR": {category: "GENERAL", version: "pretrained-form-parser-v2.0-2022-11-10", display: "Form Parser"},
	"INVOICE_PROCESSOR": {category: "SPECIALIZED", version: "pretrained-invoice-v2.0-2023-12-06", display: "Invoice Parser", fields: []prebuiltField{
		pfm("amount_paid_since_last_invoice"), pf("carrier"), pf("currency"), pf("currency_exchange_rate"), pfd("delivery_date"), pfd("due_date"),
		pfm("freight_amount"), pfd("invoice_date"), pf("invoice_id"), pfm("net_amount"), pf("payment_terms"), pf("purchase_order"),
		pf("receiver_address"), pf("receiver_email"), pf("receiver_name"), pf("receiver_phone"), pf("receiver_tax_id"), pf("receiver_website"),
		pf("remit_to_address"), pf("remit_to_name"), pf("ship_from_address"), pf("ship_from_name"), pf("ship_to_address"), pf("ship_to_name"),
		pf("supplier_address"), pf("supplier_email"), pf("supplier_iban"), pf("supplier_name"), pf("supplier_payment_ref"), pf("supplier_phone"),
		pf("supplier_registration"), pf("supplier_tax_id"), pf("supplier_website"), pfm("total_amount"), pfm("total_tax_amount"),
		pfp("line_item", pfm("amount"), pf("description"), pf("product_code"), pf("purchase_order"), pf("quantity"), pf("unit"), pfm("unit_price")),
		pfp("vat", pfm("amount"), pf("category_code"), pfm("tax_amount"), pf("tax_rate")),
	}, lead: []string{"invoice_id", "invoice_date", "supplier_name", "supplier_address", "receiver_name", "receiver_address", "due_date",
		"total_amount", "net_amount", "total_tax_amount", "line_item"}},
	"EXPENSE_PROCESSOR": {category: "SPECIALIZED", version: "pretrained-expense-v1.3-2022-07-15", display: "Expense Parser", fields: []prebuiltField{
		pf("credit_card_last_four_digits"), pf("currency"), pfd("end_date"), pfm("net_amount"), pf("payment_type"), pf("purchase_time"),
		pfd("receipt_date"), pfd("start_date"), pf("supplier_address"), pf("supplier_city"), pf("supplier_name"), pf("supplier_phone"),
		pfm("tip_amount"), pfm("total_amount"), pfm("total_tax_amount"),
		pfp("line_item", pfm("amount"), pf("description"), pf("product_code")),
	}, lead: []string{"supplier_name", "receipt_date", "total_amount", "net_amount", "total_tax_amount", "line_item"}},
	"BANK_STATEMENT_PROCESSOR": {category: "SPECIALIZED", version: "pretrained-bankstatement-v3.0-2022-05-16", display: "Bank Statement Parser", fields: []prebuiltField{
		pf("account_number"), pf("account_type"), pf("bank_address"), pf("bank_name"), pf("client_address"), pf("client_name"),
		pfm("ending_balance"), pfm("starting_balance"), pfd("statement_date"), pfd("statement_end_date"), pfd("statement_start_date"),
		pfp("table_item", pfm("transaction_deposit"), pfd("transaction_deposit_date"), pf("transaction_deposit_description"),
			pfm("transaction_withdrawal"), pfd("transaction_withdrawal_date"), pf("transaction_withdrawal_description")),
	}, lead: []string{"bank_name", "client_name", "account_number", "statement_date", "starting_balance", "ending_balance", "table_item"}},
	"PAYSTUB_PROCESSOR": {category: "SPECIALIZED", version: "pretrained-paystub-v2.0-2022-07-22", display: "Pay Slip Parser", fields: []prebuiltField{
		pf("net_pay"), pf("net_pay_ytd"), pf("employee_account_number"), pf("page_number"),
		pf("federal_additional_tax"), pf("federal_allowance"), pf("federal_marital_status"),
		pf("state_additional_tax"), pf("state_allowance"), pf("state_marital_status"),
		pfp("deduction_item", pf("deduction_type"), pf("deduction_this_period"), pf("deduction_ytd")),
		pfp("direct_deposit_item", pf("direct_deposit"), pf("employee_account_number")),
		pfp("earning_item", pf("earning_type"), pf("earning_rate"), pf("earning_hours"), pf("earning_this_period"), pf("earning_ytd")),
		pfp("tax_item", pf("tax_type"), pf("tax_this_period"), pf("tax_ytd")),
	}},
	"FORM_W2_PROCESSOR": {category: "SPECIALIZED", version: "pretrained-w2-v2.1-2022-06-08", display: "W2 Parser", fields: []prebuiltField{
		pf("AllocatedTips"), pf("ControlNumber"), pf("DependentCareBenefits"), pf("EIN"),
		pf("EmployeeAddress_AdditionalStreetAddressOrPostalBox"), pf("EmployeeAddress_City"), pf("EmployeeAddress_State"),
		pf("EmployeeAddress_StreetAddressOrPostalBox"), pf("EmployeeAddress_Zip"), pf("EmployeeName_FirstName"), pf("EmployeeName_LastName"),
		pf("EmployeeName_MiddleNameOrInitial"), pf("EmployeeName_Suffix"), pf("EmployerAddress_AdditionalStreetAddressOrPostalBox"),
		pf("EmployerAddress_City"), pf("EmployerAddress_State"), pf("EmployerAddress_StreetAddressOrPostalBox"), pf("EmployerAddress_Zip"),
		pf("EmployerName"), pf("EmployerStateIdNumber_Line1"), pf("FederalIncomeTaxWithheld"), pf("FormYear"), pf("LocalIncomeTax_Line1"),
		pf("LocalWagesTipsEtc_Line1"), pf("LocalityName_Line1"), pf("MedicareTaxWithheld"), pf("MedicareWagesAndTips"), pf("NonqualifiedPlans"),
		pf("SSN"), pf("SocialSecurityTaxWithheld"), pf("SocialSecurityTips"), pf("SocialSecurityWages"), pf("StateIncomeTax_Line1"),
		pf("StateWagesTipsEtc_Line1"), pf("State_Line1"), pf("WagesTipsOtherCompensation"), pf("a_Code"), pf("a_Value"), pf("b_Code"),
		pf("b_Value"), pf("c_Code"), pf("c_Value"), pf("d_Code"), pf("d_Value"),
	}},
	"US_DRIVER_LICENSE_PROCESSOR": {category: "SPECIALIZED", version: "pretrained-us-driver-license-v1.0-2021-06-14", display: "US Driver License Parser", fields: []prebuiltField{
		pf("Family Name"), pf("Given Names"), pf("Document Id"), pfd("Expiration Date"), pfd("Date Of Birth"), pfd("Issue Date"), pf("Address"),
	}},
}

func isPrebuiltType(t string) bool { _, ok := docAIPrebuilt[t]; return ok }

// prebuiltReady refuses a Form Parser or specialized extractor where no
// local generation model runs.
func (s *Server) prebuiltReady(t string) error {
	if !isPrebuiltType(t) || s.generativeAvailable() {
		return nil
	}
	return apierror.Unimplemented("Not implemented here: processor type %s runs on the local generation model, which this instance "+
		"does not serve (start it with up --local-ai-model); CloudBurrow never calls Google (#1799)", t)
}

// prebuiltSchema is a specialized extractor's DocumentSchema, which its
// processor versions report.
func prebuiltSchema(t string) *documentaipb.DocumentSchema {
	pt := docAIPrebuilt[t]
	if len(pt.fields) == 0 {
		return nil
	}
	occ := func(f prebuiltField) documentaipb.DocumentSchema_EntityType_Property_OccurrenceType {
		if f.multiple {
			return documentaipb.DocumentSchema_EntityType_Property_OPTIONAL_MULTIPLE
		}
		return documentaipb.DocumentSchema_EntityType_Property_OPTIONAL_ONCE
	}
	vt := func(f prebuiltField) string {
		switch f.kind {
		case "date":
			return "datetime"
		case "money":
			return "money"
		}
		return "string"
	}
	doc := &documentaipb.DocumentSchema_EntityType{Name: "custom_extraction_document_type", BaseTypes: []string{"document"}}
	out := &documentaipb.DocumentSchema{DisplayName: pt.display}
	for _, fl := range pt.fields {
		if fl.children != nil {
			child := &documentaipb.DocumentSchema_EntityType{Name: fl.name, BaseTypes: []string{"object"}}
			for _, c := range fl.children {
				child.Properties = append(child.Properties, &documentaipb.DocumentSchema_EntityType_Property{
					Name: strings.TrimPrefix(c.name, fl.name+"/"), ValueType: vt(c), OccurrenceType: occ(c)})
			}
			out.EntityTypes = append(out.EntityTypes, child)
			doc.Properties = append(doc.Properties, &documentaipb.DocumentSchema_EntityType_Property{Name: fl.name, ValueType: fl.name, OccurrenceType: occ(fl)})
			continue
		}
		doc.Properties = append(doc.Properties, &documentaipb.DocumentSchema_EntityType_Property{Name: fl.name, ValueType: vt(fl), OccurrenceType: occ(fl)})
	}
	out.EntityTypes = append([]*documentaipb.DocumentSchema_EntityType{doc}, out.EntityTypes...)
	return out
}

// applyPrebuilt adds a Form Parser's or specialized extractor's answers to
// the OCR Document.
func (s *Server) applyPrebuilt(ctx context.Context, t string, doc *documentaipb.Document) error {
	if !isPrebuiltType(t) {
		return nil
	}
	if err := s.prebuiltReady(t); err != nil {
		return err
	}
	if t == "FORM_PARSER_PROCESSOR" {
		return s.formParse(ctx, doc)
	}
	return s.extractPrebuilt(ctx, docAIPrebuilt[t].asked(), doc)
}

// --- layout anchors -------------------------------------------------------

// docRunes is a Document's text as runes and where each page's text is.
type docRunes struct {
	text  []rune
	doc   *documentaipb.Document
	spans [][2]int
}

func newDocRunes(doc *documentaipb.Document) *docRunes {
	d := &docRunes{text: []rune(doc.GetText()), doc: doc}
	for _, p := range doc.GetPages() {
		st, en := segment(p.GetLayout().GetTextAnchor())
		d.spans = append(d.spans, [2]int{st, en})
	}
	return d
}

func segment(a *documentaipb.Document_TextAnchor) (int, int) {
	segs := a.GetTextSegments()
	if len(segs) == 0 {
		return 0, 0
	}
	return int(segs[0].GetStartIndex()), int(segs[len(segs)-1].GetEndIndex())
}

// lineAround is the span of the line holding [st, en) with the lines
// before and after it.
func (d *docRunes) lineAround(st, en int) (int, int) {
	from, to := st, en
	for n := 0; from > 0; from-- {
		if d.text[from-1] == '\n' {
			if n++; n == 2 {
				break
			}
		}
	}
	for n := 0; to < len(d.text); to++ {
		if d.text[to] == '\n' {
			if n++; n == 2 {
				break
			}
		}
	}
	return from, to
}

// pageOf is the index of the page whose text holds offset at.
func (d *docRunes) pageOf(at int) int {
	for i, sp := range d.spans {
		if at >= sp[0] && at < sp[1] {
			return i
		}
	}
	return -1
}

// find anchors v in the text between from and to, the first occurrence.
func (d *docRunes) find(v string, from, to int) (int, int, bool) {
	from, to = max(0, from), min(len(d.text), to)
	if from >= to {
		return 0, 0, false
	}
	st, en, ok := findFuzzy(d.text[from:to], v)
	return st + from, en + from, ok
}

// box is the union of the page's tokens that the span [st, en) touches,
// in pixels and normalized, and their mean confidence.
func (d *docRunes) box(page, st, en int) (*documentaipb.BoundingPoly, float64, bool) {
	p := d.doc.GetPages()[page]
	w, h := int(p.GetDimension().GetWidth()), int(p.GetDimension().GetHeight())
	var q ocr.Quad
	n := 0
	conf := 0.0
	for _, tok := range p.GetTokens() {
		ts, te := segment(tok.GetLayout().GetTextAnchor())
		if te <= st || ts >= en {
			continue
		}
		var tq ocr.Quad
		for i, v := range tok.GetLayout().GetBoundingPoly().GetVertices() {
			if i < 4 {
				tq[i] = ocrPoint(int(v.GetX()), int(v.GetY()))
			}
		}
		q = unionQuad(q, tq, n == 0)
		conf += float64(tok.GetLayout().GetConfidence())
		n++
	}
	if n == 0 {
		return nil, 0, false
	}
	return docPoly(q, w, h), conf / float64(n), true
}

// layout is the Layout of the span [st, en) on page, with the model's
// confidence.
func (d *docRunes) layout(page, st, en int, conf float64) *documentaipb.Document_Page_Layout {
	l := &documentaipb.Document_Page_Layout{TextAnchor: anchor(int64(st), int64(en)), Confidence: float32(clamp(conf, 0, 1)),
		Orientation: documentaipb.Document_Page_Layout_PAGE_UP}
	if bp, _, ok := d.box(page, st, en); ok {
		l.BoundingPoly = bp
	}
	return l
}

// entity is an entity of type t over [st, en), anchored to its page.
func (d *docRunes) entity(t string, st, en int, conf float64) *documentaipb.Document_Entity {
	e := &documentaipb.Document_Entity{Type: t, MentionText: string(d.text[st:en]), Confidence: float32(clamp(conf, 0, 1)),
		TextAnchor: anchor(int64(st), int64(en))}
	if pg := d.pageOf(st); pg >= 0 {
		ref := &documentaipb.Document_PageAnchor_PageRef{Page: int64(pg)}
		if bp, _, ok := d.box(pg, st, en); ok {
			ref.BoundingPoly = bp
		}
		e.PageAnchor = &documentaipb.Document_PageAnchor{PageRefs: []*documentaipb.Document_PageAnchor_PageRef{ref}}
	}
	return e
}

// --- Form Parser ----------------------------------------------------------

type formAnswer struct {
	Fields []struct {
		Key        string  `json:"key"`
		Value      string  `json:"value"`
		Checkbox   bool    `json:"checkbox"`
		Checked    bool    `json:"checked"`
		Confidence float64 `json:"confidence"`
	} `json:"fields"`
	Tables []struct {
		Header []string   `json:"header"`
		Rows   [][]string `json:"rows"`
	} `json:"tables"`
	Entities []struct {
		Type       string  `json:"type"`
		Value      string  `json:"value"`
		Confidence float64 `json:"confidence"`
	} `json:"entities"`
}

func formSchema() map[string]any {
	str := map[string]any{"type": "string"}
	strs := map[string]any{"type": "array", "items": str}
	obj := func(props map[string]any, req ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
	}
	field := obj(map[string]any{"key": str, "value": str, "checkbox": map[string]any{"type": "boolean"},
		"checked": map[string]any{"type": "boolean"}, "confidence": confSchema()}, "key", "value", "checkbox", "checked", "confidence")
	table := obj(map[string]any{"header": strs, "rows": map[string]any{"type": "array", "items": strs, "maxItems": formMaxItems}}, "header", "rows")
	ent := obj(map[string]any{"type": map[string]any{"enum": formGenericTypes}, "value": str, "confidence": confSchema()}, "type", "value", "confidence")
	// The bounds keep greedy decoding from repeating an item until its
	// token limit.
	arr := func(item any, n int) map[string]any {
		return map[string]any{"type": "array", "items": item, "maxItems": n}
	}
	return obj(map[string]any{"fields": arr(field, formMaxItems), "tables": arr(table, 8), "entities": arr(ent, formMaxItems)},
		"fields", "tables", "entities")
}

// formMaxItems bounds each list a page's answer holds.
const formMaxItems = 64

const formPrompt = `Below is one page of a form, as OCR read it. Answer, as JSON:
- "fields": every form field on the page: its label as "key" and its filled-in value as "value", both exactly as written on the page ("" for an empty value). A checkbox is a field with "checkbox" true, its label as "key", "value" "", and "checked" whether it is ticked.
- "tables": every table: its header cells and each body row's cells, exactly as written.
- "entities": every email, phone, url, date_time, address, person, organization, quantity, price, id and page_number on the page: its type and its text exactly as written.
Give each field and entity your confidence from 0 to 1. Leave out anything that is not on the page. Answer minified JSON on one line.

Page:
`

// formParse adds formFields, tables and generic entities, page by page.
func (s *Server) formParse(ctx context.Context, doc *documentaipb.Document) error {
	d := newDocRunes(doc)
	for i, page := range doc.GetPages() {
		st, en := d.spans[i][0], d.spans[i][1]
		if strings.TrimSpace(string(d.text[st:en])) == "" {
			continue
		}
		var got formAnswer
		if err := s.genJSON(ctx, formPrompt+cutText(string(d.text[st:en]), docAIGenChars)+"\n\nAnswer:\n", formSchema(), &got); err != nil {
			return err
		}
		for _, fl := range got.Fields {
			ks, ke, ok := d.find(fl.Key, st, en)
			if !ok {
				continue
			}
			ff := &documentaipb.Document_Page_FormField{FieldName: d.layout(i, ks, ke, fl.Confidence)}
			switch {
			case fl.Checkbox:
				ff.ValueType = "unfilled_checkbox"
				if fl.Checked {
					ff.ValueType = "filled_checkbox"
				}
				ff.FieldValue = &documentaipb.Document_Page_Layout{Confidence: float32(clamp(fl.Confidence, 0, 1)),
					Orientation: documentaipb.Document_Page_Layout_PAGE_UP}
			case strings.TrimSpace(fl.Value) == "":
				ff.FieldValue = &documentaipb.Document_Page_Layout{Confidence: float32(clamp(fl.Confidence, 0, 1)),
					Orientation: documentaipb.Document_Page_Layout_PAGE_UP}
			default:
				// The value follows its label, else is anywhere on the page.
				vs, ve, ok := d.find(fl.Value, ke, en)
				if !ok {
					if vs, ve, ok = d.find(fl.Value, st, en); !ok {
						continue
					}
				}
				ff.FieldValue = d.layout(i, vs, ve, fl.Confidence)
			}
			page.FormFields = append(page.FormFields, ff)
		}
		for _, tb := range got.Tables {
			if t := d.table(i, tb.Header, tb.Rows); t != nil {
				page.Tables = append(page.Tables, t)
			}
		}
		for _, e := range got.Entities {
			es, ee, ok := d.find(e.Value, st, en)
			if !ok {
				continue
			}
			doc.Entities = append(doc.Entities, d.entity(e.Type, es, ee, e.Confidence))
		}
	}
	numberEntities(doc.Entities)
	return nil
}

// table anchors a table's cells in order on page; nil when none is found.
func (d *docRunes) table(page int, header []string, rows [][]string) *documentaipb.Document_Page_Table {
	st, en := d.spans[page][0], d.spans[page][1]
	cur := st
	found := 0
	tStart, tEnd := en, st
	row := func(cells []string) *documentaipb.Document_Page_Table_TableRow {
		r := &documentaipb.Document_Page_Table_TableRow{}
		for _, c := range cells {
			cell := &documentaipb.Document_Page_Table_TableCell{RowSpan: 1, ColSpan: 1,
				Layout: &documentaipb.Document_Page_Layout{Orientation: documentaipb.Document_Page_Layout_PAGE_UP}}
			if strings.TrimSpace(c) != "" {
				cs, ce, ok := d.find(c, cur, en)
				if !ok {
					cs, ce, ok = d.find(c, st, en)
				}
				if ok {
					cell.Layout = d.layout(page, cs, ce, 1)
					if bp, conf, ok := d.box(page, cs, ce); ok {
						cell.Layout.BoundingPoly, cell.Layout.Confidence = bp, float32(conf)
					}
					cur = ce
					found++
					tStart, tEnd = min(tStart, cs), max(tEnd, ce)
				}
			}
			r.Cells = append(r.Cells, cell)
		}
		return r
	}
	t := &documentaipb.Document_Page_Table{}
	if len(header) > 0 {
		t.HeaderRows = append(t.HeaderRows, row(header))
	}
	for _, r := range rows {
		t.BodyRows = append(t.BodyRows, row(r))
	}
	if found == 0 {
		return nil
	}
	t.Layout = d.layout(page, tStart, tEnd, 1)
	if bp, conf, ok := d.box(page, tStart, tEnd); ok {
		t.Layout.BoundingPoly, t.Layout.Confidence = bp, float32(conf)
	}
	return t
}

// numberEntities gives entities the IDs Google's answers carry: "0", "1",
// and so on.
func numberEntities(ents []*documentaipb.Document_Entity) {
	n := 0
	var walk func([]*documentaipb.Document_Entity)
	walk = func(es []*documentaipb.Document_Entity) {
		for _, e := range es {
			e.Id = strconv.Itoa(n)
			n++
			walk(e.GetProperties())
		}
	}
	walk(ents)
}

// --- specialized extractors -----------------------------------------------

// docAIGenParallel is how many pages the extractors read at once, each a
// request to the local model.
const docAIGenParallel = 4

// prebuiltAnswer is what the model answers for one page: each top-level
// field's value, and each repeated item's fields, by short name.
type prebuiltAnswer struct {
	values map[string]string
	items  []prebuiltItem
}

type prebuiltItem struct {
	typ    string
	fields map[string]string
}

// extractPrebuilt is a specialized extractor (#1799): the document's pages
// are read at once, docAIGenParallel at a time, each one request to the
// local model, so a long document takes about as long as its slowest
// pages. The model answers a compact JSON object keyed by field name, the
// values alone: the time is the tokens it writes, and naming each value's
// type and confidence tripled them (an Invoice Parser answer took over a
// minute, BigQuery's ML.PROCESS_DOCUMENT allows 120 seconds). Each value's
// confidence is the OCR confidence of the tokens it is anchored to, lower
// when the anchor is not exact. A top-level field is the first page's
// that answers it; repeated items keep page order.
func (s *Server) extractPrebuilt(ctx context.Context, fields []prebuiltField, doc *documentaipb.Document) error {
	d := newDocRunes(doc)
	if strings.TrimSpace(doc.GetText()) == "" {
		return nil
	}
	kinds := map[string]string{}
	var top []string
	var groups []string
	str := map[string]any{"type": []string{"string", "null"}}
	var props orderedProps
	var required []string
	for _, fl := range fields {
		required = append(required, fl.name)
		if fl.children == nil {
			top = append(top, fl.name)
			kinds[fl.name] = fl.kind
			props = append(props, orderedProp{fl.name, str})
			continue
		}
		child := map[string]any{}
		var names []string
		for _, c := range fl.children {
			short := strings.TrimPrefix(c.name, fl.name+"/")
			kinds[c.name] = c.kind
			child[short] = str
			names = append(names, short)
		}
		props = append(props, orderedProp{fl.name, map[string]any{"type": "array", "maxItems": formMaxItems,
			"items": map[string]any{"type": "object", "properties": child, "additionalProperties": false}}})
		groups = append(groups, fmt.Sprintf("%s (a list: one object for each, with %s)", fl.name, strings.Join(names, ", ")))
	}
	// Every field is required and may be null: the model then weighs each
	// one in turn (left optional, it stopped after a few).
	sort.Strings(required)
	schema := map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	head := "Extract fields from the document below, as OCR read it. Answer minified JSON on one line: an object with every field below, " +
		"each the value copied exactly as the document writes it (with any currency symbol), or null where the document does not state it; " +
		"never guess.\n\nFields: " + strings.Join(top, ", ")
	if len(groups) > 0 {
		head += "\nRepeated items: " + strings.Join(groups, "; ")
	}

	// The pages' texts: each page alone, or the whole text when the
	// Document has no pages.
	type piece struct{ from, to int }
	var pieces []piece
	for _, sp := range d.spans {
		if strings.TrimSpace(string(d.text[sp[0]:sp[1]])) != "" {
			pieces = append(pieces, piece{sp[0], sp[1]})
		}
	}
	if len(pieces) == 0 {
		pieces = []piece{{0, len(d.text)}}
	}
	answers := make([]prebuiltAnswer, len(pieces))
	errs := make([]error, len(pieces))
	sem := make(chan struct{}, docAIGenParallel)
	var wg sync.WaitGroup
	for i, pc := range pieces {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			prompt := head + "\n\nDocument:\n" + cutText(string(d.text[pc.from:pc.to]), docAIGenChars) + "\n\nAnswer:\n"
			var raw map[string]json.RawMessage
			if err := s.genJSON(ctx, prompt, schema, &raw); err != nil {
				errs[i] = err
				return
			}
			answers[i] = decodePrebuilt(raw, fields)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	// make1 anchors value between from and to, else anywhere unless
	// strict.
	make1 := func(typ, value string, from, to int, strict bool) *documentaipb.Document_Entity {
		if strings.TrimSpace(value) == "" {
			return nil
		}
		st, en, ok := d.find(value, from, to)
		if !ok && !strict && (from > 0 || to < len(d.text)) {
			st, en, ok = d.find(value, 0, len(d.text))
		}
		if !ok {
			return nil
		}
		if kinds[typ] == "money" && st > 0 && strings.ContainsRune("$€£¥₹", d.text[st-1]) {
			st-- // the amount's currency symbol
			value = string(d.text[st:en])
		}
		conf := 0.9
		if pg := d.pageOf(st); pg >= 0 {
			if _, c, ok := d.box(pg, st, en); ok {
				conf = c
			}
		}
		if string(d.text[st:en]) != strings.TrimSpace(value) {
			conf *= 0.9 // anchored ignoring case or spacing
		}
		e := d.entity(typ, st, en, conf)
		normalizeEntity(e, kinds[typ])
		return e
	}
	seen := map[string]bool{}
	for i, a := range answers {
		for _, name := range top {
			if seen[name] {
				continue
			}
			if e := make1(name, a.values[name], pieces[i].from, pieces[i].to, false); e != nil {
				seen[name] = true
				doc.Entities = append(doc.Entities, e)
			}
		}
	}
	for i, a := range answers {
		cur := pieces[i].from
		for _, it := range a.items {
			var props []*documentaipb.Document_Entity
			st, en := len(d.text), 0
			conf := 0.0
			// The item's longest value is anchored first, after the item
			// before; its other values only on the lines around it, so a
			// short value ("1") is not anchored elsewhere in the page.
			var children []prebuiltField
			for _, fl := range fields {
				if fl.name == it.typ {
					children = append(children, fl.children...)
				}
			}
			short := func(c prebuiltField) string { return it.fields[strings.TrimPrefix(c.name, it.typ+"/")] }
			sort.SliceStable(children, func(i, j int) bool { return len(short(children[i])) > len(short(children[j])) })
			from, to := cur, pieces[i].to
			for k, c := range children {
				e := make1(c.name, short(c), from, to, k > 0)
				if e == nil {
					if k == 0 {
						break
					}
					continue
				}
				es, ee := segment(e.GetTextAnchor())
				if k == 0 {
					from, to = d.lineAround(es, ee)
				}
				st, en = min(st, es), max(en, ee)
				conf += float64(e.GetConfidence())
				props = append(props, e)
			}
			if len(props) == 0 {
				continue
			}
			sort.SliceStable(props, func(i, j int) bool {
				a, _ := segment(props[i].GetTextAnchor())
				b, _ := segment(props[j].GetTextAnchor())
				return a < b
			})
			cur = en
			parent := d.entity(it.typ, st, en, conf/float64(len(props)))
			parent.Properties = props
			doc.Entities = append(doc.Entities, parent)
		}
	}
	numberEntities(doc.Entities)
	return nil
}

// orderedProps are a JSON schema's properties in the order the model is
// to write them (a Go map's are marshalled sorted).
type orderedProps []orderedProp

type orderedProp struct {
	name   string
	schema any
}

func (o orderedProps) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(p.name)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(p.schema)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// decodePrebuilt reads the model's answer object: a string for each
// top-level field, a list of objects of strings for each repeated one.
// A value of another JSON type is read as its text.
func decodePrebuilt(raw map[string]json.RawMessage, fields []prebuiltField) prebuiltAnswer {
	text := func(m json.RawMessage) string {
		var s string
		if json.Unmarshal(m, &s) == nil {
			return s
		}
		if string(m) == "null" {
			return ""
		}
		return string(m)
	}
	a := prebuiltAnswer{values: map[string]string{}}
	for _, fl := range fields {
		m, ok := raw[fl.name]
		if !ok {
			continue
		}
		if fl.children == nil {
			a.values[fl.name] = text(m)
			continue
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(m, &items) != nil {
			continue
		}
		for _, it := range items {
			pi := prebuiltItem{typ: fl.name, fields: map[string]string{}}
			for k, v := range it {
				pi.fields[k] = text(v)
			}
			a.items = append(a.items, pi)
		}
	}
	return a
}

var amountRE = regexp.MustCompile(`-?\d[\d,. ]*`)

// normalizeEntity sets a date's or money amount's normalizedValue, when the
// mention parses.
func normalizeEntity(e *documentaipb.Document_Entity, kind string) {
	m := strings.TrimSpace(e.GetMentionText())
	switch kind {
	case "date":
		if t, ok := parseDate(m); ok {
			e.NormalizedValue = &documentaipb.Document_Entity_NormalizedValue{Text: t.Format("2006-01-02"),
				StructuredValue: &documentaipb.Document_Entity_NormalizedValue_DateValue{DateValue: &date.Date{
					Year: int32(t.Year()), Month: int32(t.Month()), Day: int32(t.Day())}}}
		}
	case "money":
		num := amountRE.FindString(m)
		num = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(num), ",", ""), " ", "")
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return
		}
		units, frac := math.Modf(v)
		cur := ""
		switch {
		case strings.Contains(m, "$") || strings.Contains(strings.ToUpper(m), "USD"):
			cur = "USD"
		case strings.Contains(m, "€") || strings.Contains(strings.ToUpper(m), "EUR"):
			cur = "EUR"
		case strings.Contains(m, "£") || strings.Contains(strings.ToUpper(m), "GBP"):
			cur = "GBP"
		}
		text := strconv.FormatFloat(v, 'f', 2, 64)
		if cur != "" {
			text += " " + cur
		}
		e.NormalizedValue = &documentaipb.Document_Entity_NormalizedValue{Text: text,
			StructuredValue: &documentaipb.Document_Entity_NormalizedValue_MoneyValue{MoneyValue: &money.Money{
				CurrencyCode: cur, Units: int64(units), Nanos: int32(math.Round(frac * 1e9))}}}
	}
}

var dateLayouts = []string{"2006-01-02", "01/02/2006", "1/2/2006", "01-02-2006", "02.01.2006", "January 2, 2006", "Jan 2, 2006",
	"2 January 2006", "2 Jan 2006", "January 2 2006", "Jan 2 2006", "01/02/06", "1/2/06", "2006/01/02"}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(strings.TrimRight(s, ".,"))
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func ocrPoint(x, y int) image.Point { return image.Point{X: x, Y: y} }
