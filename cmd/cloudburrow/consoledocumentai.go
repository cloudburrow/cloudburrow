package main

// Document AI's console (#1930), in the order of Google's menu: Overview,
// Processor gallery (console.cloud.google.com/ai/document-ai/processor-library)
// and My processors (…/ai/document-ai/processors), as
// https://docs.cloud.google.com/document-ai/docs/create-processor names them
// (read 2026-11-19). Every read and change goes through the official
// cloud.google.com/go/documentai client on the local AI endpoint (#1799), as
// an SDK's would; nothing is sent to Google.
//
//   - Processor gallery: every processor type Google's gallery has. The two
//     the local endpoint serves (Enterprise Document OCR and Layout Parser,
//     FetchProcessorTypes) open a page with Create processor; every other
//     type says "Not available locally" and why.
//   - My processors: ListProcessors in each served location, Google's
//     columns (Name, Status, Region, Type), Create processor, Enable
//     processor and Disable processor on each row, and Delete.
//   - A processor's page: its Overview (Name, ID, Type, Prediction
//     endpoint), Manage versions (ListProcessorVersions' pretrained
//     versions, the Default version picker, SetDefaultProcessorVersion, and
//     Deploy, Undeploy and Delete over the checked versions, which Google
//     refuses for a pretrained or default version), Evaluate & test (the
//     default version's evaluations, which no served type has, #1953),
//     Batch process (a gs:// source and destination, the version, and
//     pages per shard, #1953), and Upload test
//     document, which sends the file to ProcessDocument and draws the page
//     images it answers with the blocks, paragraphs and lines boxed over
//     them, beside the extracted text and the elements' rows (the layout
//     parser's blocks and chunks beside its text) (#1930).
//   - The Processor gallery is Google's cards, under its category headings.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// docAIRegions are the locations Document AI serves locally, with the
// names Google's Region list gives them.
var docAIRegions = []string{"us", "eu"}

var docAIRegionLabels = map[string]string{"us": "US (United States)", "eu": "EU (European Union)"}

// docAIGalleryType is one card of Google's processor gallery.
type docAIGalleryType struct {
	Type, Name, Category, About string
}

// docAIGallery is Google's processor gallery, in its order
// (https://docs.cloud.google.com/document-ai/docs/processors-list, read
// 2026-11-19). The served types are the ones FetchProcessorTypes names;
// for the rest the page says why they are not available.
var docAIGallery = []docAIGalleryType{
	{"OCR_PROCESSOR", "Enterprise Document OCR", "Digitize", "Text and layout of scanned and digital documents"},
	{"LAYOUT_PARSER_PROCESSOR", "Layout Parser", "Digitize", "Document structure (paragraphs) and chunks for retrieval"},
	{"FORM_PARSER_PROCESSOR", "Form Parser", "Digitize", "Key-value pairs, checkboxes and tables of forms"},
	{"CUSTOM_EXTRACTION_PROCESSOR", "Custom Extractor", "Extract", "Fields you define, extracted by a generative model"},
	{"SUMMARIZER_PROCESSOR", "Summarizer", "Extract", "A summary of a document by a generative model"},
	{"CUSTOM_CLASSIFICATION_PROCESSOR", "Custom Classifier", "Classify and split", "Classes you define, trained on your documents"},
	{"CUSTOM_SPLITTING_PROCESSOR", "Custom Splitter", "Classify and split", "Logical documents in a file, trained on your documents"},
	{"INVOICE_PROCESSOR", "Invoice Parser", "Specialized", "Fields of invoices"},
	{"EXPENSE_PROCESSOR", "Expense Parser", "Specialized", "Fields of receipts"},
	{"BANK_STATEMENT_PROCESSOR", "Bank Statement Parser", "Specialized", "Fields of bank statements"},
	{"PAYSTUB_PROCESSOR", "Pay Slip Parser", "Specialized", "Fields of pay slips"},
	{"UTILITY_PROCESSOR", "Utility Parser", "Specialized", "Fields of utility bills"},
	{"W2_PROCESSOR", "W2 Parser", "Specialized", "Fields of US W-2 forms"},
	{"FORM_W9_PROCESSOR", "W9 Parser", "Specialized", "Fields of US W-9 forms"},
	{"US_DRIVER_LICENSE_PROCESSOR", "US Driver License Parser", "Specialized", "Fields of US driver licenses"},
	{"US_PASSPORT_PROCESSOR", "US Passport Parser", "Specialized", "Fields of US passports"},
	{"ID_PROOFING_PROCESSOR", "Identity Proofing", "Specialized", "Signals that an identity document is genuine"},
	{"LENDING_DOCUMENT_SPLIT_PROCESSOR", "Lending Document Splitter", "Specialized", "Lending documents in a file, classified"},
	{"PROCUREMENT_DOCUMENT_SPLIT_PROCESSOR", "Procurement Document Splitter", "Specialized", "Procurement documents in a file, classified"},
}

// docAINotLocal is why a type the endpoint does not serve is not available.
const docAINotLocal = "Not available locally: this processor runs a model Google trains and hosts, which CloudBurrow has no " +
	"local equivalent of, and it never calls Google. Enterprise Document OCR and Layout Parser run here, on Tesseract."

func docAIGalleryEntry(t string) (docAIGalleryType, bool) {
	for _, g := range docAIGallery {
		if g.Type == t {
			return g, true
		}
	}
	return docAIGalleryType{}, false
}

// docAITypeLabel is a type as the console names it.
func docAITypeLabel(t string) string {
	if g, ok := docAIGalleryEntry(t); ok {
		return g.Name
	}
	return t
}

// docAI is what Document AI's pages share: the project and the client.
type docAI struct {
	defaultProject string
	// addr is the local AI endpoint's address, read per request: the
	// console is built before the endpoint binds.
	addr func() string
}

func (d docAI) project(project string) string {
	if project == "" {
		return d.defaultProject
	}
	return project
}

func (d docAI) withClient(ctx context.Context, fn func(*documentai.DocumentProcessorClient) error) error {
	addr := ""
	if d.addr != nil {
		addr = d.addr()
	}
	if addr == "" {
		return errors.New("the local AI endpoint, which serves Document AI, is not running")
	}
	c, err := documentai.NewDocumentProcessorClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Document AI: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// servedTypes are the types FetchProcessorTypes names in a location.
func (d docAI) servedTypes(ctx context.Context, project, region string) (map[string]*documentaipb.ProcessorType, error) {
	out := map[string]*documentaipb.ProcessorType{}
	err := d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		r, err := c.FetchProcessorTypes(ctx, &documentaipb.FetchProcessorTypesRequest{
			Parent: "projects/" + d.project(project) + "/locations/" + region})
		for _, t := range r.GetProcessorTypes() {
			out[t.GetType()] = t
		}
		return err
	})
	return out, err
}

// processors are the project's processors in every served location.
func (d docAI) processors(ctx context.Context, project string) ([]*documentaipb.Processor, error) {
	var out []*documentaipb.Processor
	err := d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		for _, loc := range docAIRegions {
			it := c.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: "projects/" + d.project(project) + "/locations/" + loc})
			for {
				p, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					return err
				}
				out = append(out, p)
			}
		}
		return nil
	})
	return out, err
}

// processor resolves a processor by its ID, in whichever served location
// holds it.
func (d docAI) processor(ctx context.Context, project, id string) (*documentaipb.Processor, error) {
	all, err := d.processors(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if lastSegment(p.GetName()) == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("processor %q not found", id)
}

func (d docAI) create(ctx context.Context, project, typ, name, region, kmsKey string) (*documentaipb.Processor, error) {
	if region == "" {
		region = "us"
	}
	var p *documentaipb.Processor
	err := d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		var e error
		p, e = c.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: "projects/" + d.project(project) + "/locations/" + region,
			Processor: &documentaipb.Processor{Type: typ, DisplayName: strings.TrimSpace(name), KmsKeyName: strings.TrimSpace(kmsKey)}})
		return e
	})
	return p, err
}

// processorFields are Google's Create processor side panel: a name, a
// region, and under Advanced options the encryption (#1950): Google-managed,
// or a customer-managed key of this instance's Cloud KMS.
func processorFields() []console.Field {
	return []console.Field{
		{Name: "name", Label: "Processor name", Type: "text", Required: true, Pattern: "^.{1,63}$",
			Help: "a display name for the processor"},
		{Name: "region", Label: "Region", Type: "select", Options: docAIRegions, OptionLabels: docAIRegionLabels, Default: "us"},
		{Name: "encryption", Label: "Encryption", Type: "radio", Section: "Advanced options", Default: "google",
			Options: []string{"google", "cmek"}, OptionLabels: map[string]string{"google": "Google-managed encryption key",
				"cmek": "Customer-managed encryption key (CMEK)"}},
		{Name: "kmsKeyName", Label: "Customer-managed encryption key", Type: "text", Section: "Advanced options", ShowWhen: "encryption=cmek",
			Browse: "kms", Help: "A key of this instance's Cloud KMS in us-central1 for the US region, europe-west4 for the EU. " +
				"The Document AI service agent needs Cloud KMS CryptoKey Encrypter/Decrypter on it. It cannot be changed once the processor is created."},
	}
}

// processorKey is the key a Create processor form names, "" for
// Google-managed encryption.
func processorKey(v map[string]string) string {
	if v["encryption"] != "cmek" {
		return ""
	}
	return v["kmsKeyName"]
}

// --- Overview -------------------------------------------------------------

// docAIOverviewProvider is Document AI's Overview: what runs here, and the
// way to the gallery and the processors.
type docAIOverviewProvider struct{ d docAI }

func (docAIOverviewProvider) ID() string    { return "documentai-overview" }
func (docAIOverviewProvider) Title() string { return "Overview" }

func (p docAIOverviewProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Noun: "processors", SummaryOnly: true}
	procs, err := p.d.processors(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	enabled := 0
	for _, pr := range procs {
		if pr.GetState() == documentaipb.Processor_ENABLED {
			enabled++
		}
	}
	q := "?project=" + url.QueryEscape(p.d.project(project))
	out.Summary = []console.PropertyGroup{
		{Heading: "Get started", Properties: []console.Property{
			{Label: "Processor gallery", Value: "Choose a processor type and create a processor", Link: "/document-ai/processor-library" + q},
			{Label: "My processors", Value: fmt.Sprintf("%d processors, %d enabled", len(procs), enabled), Link: "/document-ai/processors" + q},
		}},
		{Heading: "What runs locally", Properties: []console.Property{
			{Label: "Enterprise Document OCR", Value: "Tesseract's LSTM engine, in process: text, pages, blocks, paragraphs, lines and tokens"},
			{Label: "Layout Parser", Value: "Tesseract's paragraphs as layout blocks, and chunks of whole paragraphs"},
			{Label: "Regions", Value: "us, eu"},
			{Label: "Other processor types", Value: "Not available locally: Google's trained models have no local equivalent"},
			{Label: "Processing", Value: "Online (ProcessDocument) and batch (BatchProcessDocuments, Cloud Storage to Cloud Storage)"},
			{Label: "Custom processors", Value: "Custom Extractor, Classifier and Splitter: a dataset, labeling, training, evaluation " +
				"and deployment; versions are generative on the local model when one is installed (up --local-ai-model), counting models otherwise"},
			{Label: "Versions", Value: "Google's pretrained versions of OCR and Layout Parser, all read with Tesseract; custom processors' versions are trained here"},
		}},
	}
	out.Total = len(procs)
	return out, nil
}

// --- Processor gallery ------------------------------------------------------

// docAIGalleryProvider is the Processor gallery.
type docAIGalleryProvider struct{ d docAI }

func (docAIGalleryProvider) ID() string    { return "documentai-gallery" }
func (docAIGalleryProvider) Title() string { return "Processor gallery" }

func (p docAIGalleryProvider) List(ctx context.Context, project string) (console.Listing, error) {
	served, err := p.d.servedTypes(ctx, project, "us")
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	// Google's gallery is a card per type under its category's heading,
	// each card naming the type and what it does, with Create processor
	// (#1930); the rows carry the same for a script.
	out := console.Listing{Columns: []string{"Category", "Description", "Type"}, NameColumn: "Processor", Noun: "processor types",
		AlwaysStatus: true, SummaryOnly: true, Note: "Enterprise Document OCR and Layout Parser run on this instance. Every other type " +
			"is in Google's gallery and not available locally."}
	q := "?project=" + url.QueryEscape(p.d.project(project))
	category := ""
	for _, g := range docAIGallery {
		page := "/document-ai/processor-library/" + g.Type + q
		r := console.Resource{Name: g.Name, Fields: map[string]string{"Category": g.Category, "Description": g.About, "Type": g.Type},
			Link: page}
		card := console.PropertyGroup{Heading: g.Name, Properties: []console.Property{{Label: "Description", Value: g.About}}}
		if g.Category != category {
			card.Section, category = g.Category, g.Category
		}
		if _, ok := served[g.Type]; ok {
			r.Status = "Available"
			r.Actions = []console.Action{{ID: "create-processor", Label: "Create processor", Button: true,
				Href: page + "&action=create-processor"}}
			card.Properties = append(card.Properties, console.Property{Label: "Status", Value: "Available"},
				console.Property{Label: "Create processor", Value: "Create " + g.Name + " processor", Link: page + "&action=create-processor"})
		} else {
			r.Status = "Not available locally"
			card.Properties = append(card.Properties, console.Property{Label: "Status", Value: "Not available locally"},
				console.Property{Label: "Details", Value: "Why " + g.Name + " is not available", Link: page})
		}
		out.Items = append(out.Items, r)
		out.Summary = append(out.Summary, card)
	}
	out.Total = len(out.Items)
	return out, nil
}

// Actions implements console.Actor: a served card's Create processor,
// a link to its page.
func (p docAIGalleryProvider) Actions(r console.Resource) []console.Action { return r.Actions }

func (p docAIGalleryProvider) Act(context.Context, string, string, string) error {
	return errors.New("Create processor opens the processor type's page")
}

// Detail implements console.Driller: a processor type's page.
func (p docAIGalleryProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	g, ok := docAIGalleryEntry(path[0])
	if !ok {
		return console.Detail{Unavailable: fmt.Sprintf("Google's gallery has no processor type %q", path[0])}, nil
	}
	d := console.Detail{Title: g.Name, Summary: []console.Property{{Label: "Type", Value: g.Type},
		{Label: "Category", Value: g.Category}, {Label: "Description", Value: g.About}}}
	served, err := p.d.servedTypes(ctx, project, "us")
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	t, ok := served[g.Type]
	if !ok {
		d.Summary = append(d.Summary, console.Property{Label: "Status", Value: "Not available locally"})
		d.Sections = []console.Section{{ID: "about", Label: "Availability", Kind: console.KindText, Text: docAINotLocal}}
		return d, nil
	}
	var locs []string
	for _, l := range t.GetAvailableLocations() {
		locs = append(locs, l.GetLocationId())
	}
	d.Summary = append(d.Summary, console.Property{Label: "Status", Value: "Available"},
		console.Property{Label: "Regions", Value: strings.Join(locs, ", ")})
	d.Sections = []console.Section{{ID: "about", Label: "About", Kind: console.KindText, Text: docAIAbout(g.Type)}}
	return d, nil
}

func docAIAbout(t string) string {
	switch docAICustomKinds[t] {
	case "extractor":
		return "Custom Extractor extracts the fields you define. Define the fields in Get started, import and label documents in " +
			"Build, then Create new version. Where this instance serves a local generation model, a version is generative: " +
			"zero-shot from the fields and the document prompt, or few-shot from labeled documents; otherwise it learns each " +
			"field's anchor words and value shape from the labeled documents. Pages are read with Tesseract."
	case "classifier":
		return "Custom Classifier classifies documents into the document types you define, from labeled documents: few-shot on " +
			"the local generation model where one is installed, a naive Bayes model over the words otherwise."
	case "splitter":
		return "Custom Splitter finds the logical documents in a file and classifies each, from labeled files: few-shot on the " +
			"local generation model where one is installed, naive Bayes models over each page's words otherwise."
	}
	if t == "LAYOUT_PARSER_PROCESSOR" {
		return "Layout Parser reads each page with Tesseract and answers documentLayout, a text block per paragraph, " +
			"and chunkedDocument, chunks of whole paragraphs of at most 500 words by default.\n" +
			"Headings, lists and tables, which Google's model also finds, are paragraphs here."
	}
	return "Enterprise Document OCR reads PDF (each page rendered at 300 DPI), PNG, JPEG, GIF, TIFF, BMP and WebP with " +
		"Tesseract's LSTM engine, and answers Document.text and, a page each, its blocks, paragraphs, lines and tokens, " +
		"each with its text anchor, confidence and bounding box."
}

// DetailActions implements console.PathActor: Create processor on a served
// type's page.
func (p docAIGalleryProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	served, err := p.d.servedTypes(ctx, project, "us")
	if _, ok := served[path[0]]; err != nil || !ok {
		return nil
	}
	return []console.Action{{ID: "create-processor", Label: "Create processor", Submit: "Create", Fields: processorFields()}}
}

func (p docAIGalleryProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, v)
	return err
}

// ActAtResult implements console.ResultActor: Create processor answers the
// processor made, which opens its page.
func (p docAIGalleryProvider) ActAtResult(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, error) {
	if len(path) != 1 || action != "create-processor" {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	pr, err := p.d.create(ctx, project, path[0], v["name"], v["region"], processorKey(v))
	if err != nil {
		return nil, err
	}
	id := lastSegment(pr.GetName())
	return &console.Listing{Columns: []string{"Name", "Region", "Status"}, NameColumn: "Processor ID", Items: []console.Resource{{
		Name: id, Link: "/document-ai/processors/" + id + "?project=" + url.QueryEscape(p.d.project(project)),
		Fields: map[string]string{"Name": pr.GetDisplayName(), "Region": locationOf(pr.GetName()), "Status": stateWord(pr.GetState().String())}}}}, nil
}

// --- My processors ------------------------------------------------------------

// docAIProcessorsProvider is My processors.
type docAIProcessorsProvider struct {
	d docAI
	// analyses are Upload test document's results, each opened as a page
	// of its own (#2084). Nil answers them in the dialog.
	analyses *docAIAnalyses
	// labels saves a custom processor's dataset document's labels, in
	// process (consoledocumentaicustom.go, #2124). Nil refuses labeling.
	labels docAILabeler
}

func (docAIProcessorsProvider) ID() string    { return "documentai" }
func (docAIProcessorsProvider) Title() string { return "My processors" }

func (p docAIProcessorsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Name", "Region", "Type", "Created"}, NameColumn: "Processor ID", Noun: "processors",
		AlwaysStatus: true, RowsOpenable: true}
	procs, err := p.d.processors(ctx, project)
	if err != nil {
		return console.Listing{Unavailable: consoleErr(err)}, nil
	}
	sort.SliceStable(procs, func(i, j int) bool { return procs[i].GetCreateTime().AsTime().After(procs[j].GetCreateTime().AsTime()) })
	for _, pr := range procs {
		out.Items = append(out.Items, console.Resource{Name: lastSegment(pr.GetName()), Status: stateWord(pr.GetState().String()),
			Fields: map[string]string{"Name": pr.GetDisplayName(), "Region": locationOf(pr.GetName()),
				"Type": docAITypeLabel(pr.GetType()), "Created": stampAge(pr.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (p docAIProcessorsProvider) CreateForm() (string, []console.Field) {
	var types []string
	labels := map[string]string{}
	for _, g := range docAIGallery {
		if g.Type != "OCR_PROCESSOR" && g.Type != "LAYOUT_PARSER_PROCESSOR" && docAICustomKinds[g.Type] == "" {
			continue
		}
		types = append(types, g.Type)
		labels[g.Type] = g.Name
	}
	return "Create processor", append([]console.Field{{Name: "type", Label: "Processor type", Type: "select", Options: types,
		OptionLabels: labels, Default: types[0], Help: "the types that run locally; the Processor gallery lists the others"}},
		processorFields()...)
}

func (p docAIProcessorsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	pr, err := p.d.create(ctx, project, v["type"], v["name"], v["region"], processorKey(v))
	if err != nil {
		return "", err
	}
	return lastSegment(pr.GetName()), nil
}

func (p docAIProcessorsProvider) Delete(ctx context.Context, project, id string) error {
	pr, err := p.d.processor(ctx, project, id)
	if err != nil {
		return err
	}
	return p.d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		op, err := c.DeleteProcessor(ctx, &documentaipb.DeleteProcessorRequest{Name: pr.GetName()})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}

// Actions implements console.Actor: Google's row menu, Enable processor or
// Disable processor.
func (p docAIProcessorsProvider) Actions(r console.Resource) []console.Action {
	if r.Status == "Enabled" {
		return []console.Action{{ID: "disable", Label: "Disable processor"}}
	}
	if r.Status == "Disabled" {
		return []console.Action{{ID: "enable", Label: "Enable processor"}}
	}
	return nil
}

func (p docAIProcessorsProvider) Act(ctx context.Context, project, id, action string) error {
	pr, err := p.d.processor(ctx, project, id)
	if err != nil {
		return err
	}
	return p.d.setEnabled(ctx, pr.GetName(), action)
}

func (d docAI) setEnabled(ctx context.Context, name, action string) error {
	return d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		switch action {
		case "enable":
			op, err := c.EnableProcessor(ctx, &documentaipb.EnableProcessorRequest{Name: name})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		case "disable":
			op, err := c.DisableProcessor(ctx, &documentaipb.DisableProcessorRequest{Name: name})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		}
		return fmt.Errorf("unknown action %q", action)
	})
}

// predictionEndpoint is the processor's :process URL on the local endpoint,
// where Google's names its regional host.
func (p docAIProcessorsProvider) predictionEndpoint(pr *documentaipb.Processor) string {
	addr := ""
	if p.d.addr != nil {
		addr = p.d.addr()
	}
	return "http://" + addr + "/v1/" + pr.GetName() + ":process"
}

// Detail implements console.Driller: a processor's page.
func (p docAIProcessorsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) == 3 && path[1] == docAIAnalysisSegment {
		return p.analysisDetail(project, path[0], path[2]), nil
	}
	if d, ok := p.customDetail(ctx, project, path); ok {
		return d, nil
	}
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	pr, err := p.d.processor(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	overview := []console.Property{{Label: "Name", Value: pr.GetDisplayName()}, {Label: "ID", Value: lastSegment(pr.GetName())},
		{Label: "Type", Value: docAITypeLabel(pr.GetType())}, {Label: "Region", Value: locationOf(pr.GetName())},
		{Label: "Status", Value: stateWord(pr.GetState().String())},
		{Label: "Prediction endpoint", Value: p.predictionEndpoint(pr), Copy: "Copy"},
		{Label: "Resource name", Value: pr.GetName()}, {Label: "Created", Value: stampAge(pr.GetCreateTime())}}
	enc := "Google-managed"
	if k := pr.GetKmsKeyName(); k != "" {
		enc = k
	}
	overview = append(overview, console.Property{Label: "Encryption key", Value: enc})
	d := console.Detail{Title: pr.GetDisplayName(), Summary: overview}
	vers, err := p.d.versions(ctx, pr.GetName())
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	// Google's Manage versions tab: the versions with their state, the
	// Default version picker, and Deploy, Undeploy and Delete over the
	// checked versions (#1930).
	custom := docAICustomKinds[pr.GetType()] != ""
	versions := console.Section{ID: "versions", Label: "Manage versions", Listing: console.Listing{
		Columns: []string{"Version name", "Default", "Google managed", "Created"}, NameColumn: "Version ID", Noun: "versions",
		AlwaysStatus: true, SelectActions: docAIVersionSelectActions()},
		Actions: []console.Action{docAIDefaultVersionAction(pr, vers)},
		Note: "Google's pretrained versions of " + docAITypeLabel(pr.GetType()) + ", all read here with Tesseract. Google does not let " +
			"a pretrained version or the default be deployed, undeployed or deleted; training creates versions of custom processors only."}
	for _, v := range vers {
		def := "No"
		if v.GetName() == pr.GetDefaultProcessorVersion() {
			def = "Yes"
		}
		gm := "No"
		if v.GetGoogleManaged() {
			gm = "Yes"
		}
		versions.Listing.Items = append(versions.Listing.Items, console.Resource{Name: lastSegment(v.GetName()), Selectable: true,
			Status: stateWord(v.GetState().String()), Fields: map[string]string{"Version name": v.GetDisplayName(), "Default": def,
				"Google managed": gm, "Created": stampAge(v.GetCreateTime())}})
	}
	versions.Listing.Total = len(versions.Listing.Items)
	if custom {
		// Google's Deploy & use: the trained versions, their state and
		// model, deployed, undeployed, deleted or made the default.
		versions.Label = "Deploy & use"
		versions.Note = "The versions trained in Build. Deploy a version, then set it as the default, which a request that names " +
			"the processor uses; a request may also name a deployed version. The default and a deployed version cannot be deleted."
		versions.Listing.Columns = []string{"Version name", "Default", "Model", "F1 score", "Created"}
		for i, v := range vers {
			model := "Counting model"
			if v.GetModelType() == documentaipb.ProcessorVersion_MODEL_TYPE_GENERATIVE {
				model = "Generative"
			}
			f1 := "Not evaluated"
			if m := v.GetLatestEvaluation().GetAggregateMetrics(); m != nil {
				f1 = pct(m.GetF1Score())
			}
			versions.Listing.Items[i].Fields["Model"], versions.Listing.Items[i].Fields["F1 score"] = model, f1
		}
		if len(vers) == 0 {
			versions.Actions = nil
		}
		cs := p.customSections(ctx, project, pr, vers)
		d.Sections = append([]console.Section{{ID: "overview", Label: "Overview", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Properties: overview}}}}, cs[:len(cs)-1]...)
		d.Sections = append(d.Sections, versions, cs[len(cs)-1])
		return d, nil
	}
	d.Sections = []console.Section{
		{ID: "overview", Label: "Overview", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Properties: overview}}},
		versions,
		p.d.evaluations(ctx, pr),
	}
	return d, nil
}

// evaluations is Google's Evaluate & test tab (#1953): the default
// version's evaluations, ListEvaluations', which no served type has.
func (d docAI) evaluations(ctx context.Context, pr *documentaipb.Processor) console.Section {
	sec := console.Section{ID: "evaluate", Label: "Evaluate & test", Listing: console.Listing{
		Columns: []string{"Created", "Documents"}, NameColumn: "Evaluation", Noun: "evaluations"},
		Note: "Evaluations of " + lastSegment(pr.GetDefaultProcessorVersion()) + ". Google evaluates the entities a custom extractor, " +
			"classifier or splitter finds against a labeled dataset; " + docAITypeLabel(pr.GetType()) + " extracts none, so it cannot be evaluated."}
	err := d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		it := c.ListEvaluations(ctx, &documentaipb.ListEvaluationsRequest{Parent: pr.GetDefaultProcessorVersion()})
		for {
			e, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(e.GetName()), Fields: map[string]string{
				"Created": stampAge(e.GetCreateTime()), "Documents": strconv.Itoa(int(e.GetDocumentCounters().GetEvaluatedDocumentsCount()))}})
		}
	})
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// docAIMaxUpload is Document AI's online file limit.
const docAIMaxUpload = 40 << 20

// DetailActions implements console.PathActor.
func (p docAIProcessorsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) > 1 {
		return p.customPathActions(ctx, project, path)
	}
	if len(path) != 1 {
		return nil
	}
	pr, err := p.d.processor(ctx, project, path[0])
	if err != nil {
		return nil
	}
	vers, verr := p.d.versions(ctx, pr.GetName())
	acts := []console.Action{docAIUploadTestAction(pr, vers, verr)}
	if verr == nil {
		acts = append(acts, docAIDefaultVersionAction(pr, vers))
	}
	acts = append(acts, docAIVersionSelectActions()...)
	if kind := docAICustomKinds[pr.GetType()]; kind != "" && verr == nil {
		// The Get started, Build and Evaluate & test tabs' own.
		ds, err := p.d.dataset(ctx, pr, "")
		if err == nil {
			acts = append(acts, docAISchemaActions(kind)...)
			acts = append(acts, p.buildActions(pr, kind, ds.schema, vers)...)
		}
		acts = append(acts, console.Action{ID: "delete-fields", Label: "Delete", Destructive: true, SelectionField: docAIVersionsField,
			Fields: []console.Field{{Name: docAIVersionsField, Type: "hidden"}}})
		acts = append(acts, docAIDocumentSelectActions()...)
		acts = append(acts, docAIEvaluateAction(vers))
	}
	batch := []console.Field{
		{Name: "input", Label: "Source path", Type: "text", Required: true, Browse: "storage-path", Pattern: `^gs://[^\/]+(\/.*)?$`,
			Help: "gs://bucket/folder/ to process every document under it, or gs://bucket/file.pdf for one"},
		{Name: "output", Label: "Destination path", Type: "text", Required: true, Browse: "storage", Pattern: `^gs://[^\/]+(\/.*)?$`,
			Help: "gs://bucket/folder: each result is written under {operation ID}/{index}/"}}
	// The version to process with, the default unless another is chosen,
	// and the output's sharding (#1953).
	if verr == nil {
		def := docAIDefaultVersionAction(pr, vers).Fields[0]
		batch = append(batch, console.Field{Name: "version", Label: "Processor version", Type: "select", Options: def.Options,
			OptionLabels: def.OptionLabels, Default: def.Default, Help: "the processor's default unless you choose another"})
	}
	batch = append(batch, console.Field{Name: "pagesPerShard", Label: "Pages per shard", Type: "number",
		Help: "optional: split each result into {file name}-{shard}.json files of this many pages"})
	acts = append(acts, console.Action{ID: "batch-process", Label: "Batch process", Submit: "Process", Fields: batch})
	if pr.GetState() == documentaipb.Processor_ENABLED {
		acts = append(acts, console.Action{ID: "disable", Label: "Disable processor"})
	} else {
		acts = append(acts, console.Action{ID: "enable", Label: "Enable processor"})
	}
	return append(acts, console.Action{ID: "delete", Label: "Delete processor", Destructive: true, Leaves: true,
		Confirm: "the processor"})
}

// docAIUploadTestAction is Upload Test Document (#1930) with the options
// Google's test panel sets on ProcessDocument: the processor version, the
// pages, and the processor's own config: the layout parser's Edit Layout
// Config (its chunk size,
// https://docs.cloud.google.com/document-ai/docs/layout-parse-quickstart,
// read 2026-11-20) and the OCR processor's symbols.
func docAIUploadTestAction(pr *documentaipb.Processor, vers []*documentaipb.ProcessorVersion, verr error) console.Action {
	fields := []console.Field{{Name: "file", Label: "Document", Type: console.FileFieldType, Required: true,
		Help: "PDF, PNG, JPEG, GIF, TIFF, BMP or WebP, at most 40 MB and 15 pages"}}
	if verr == nil {
		def := docAIDefaultVersionAction(pr, vers).Fields[0]
		fields = append(fields, console.Field{Name: "version", Label: "Processor version", Type: "select", Options: def.Options,
			OptionLabels: def.OptionLabels, Default: def.Default, Section: "Process options",
			Help: "the processor's default unless you choose another"})
	}
	fields = append(fields, console.Field{Name: "pages", Label: "Pages", Type: "text", Section: "Process options",
		Pattern: `^\s*([0-9]+\s*(,\s*[0-9]+\s*)*)?$`, Help: "optional: the page numbers to process, such as 1,3; empty for every page"})
	switch pr.GetType() {
	case "LAYOUT_PARSER_PROCESSOR":
		fields = append(fields, console.Field{Name: "chunkSize", Label: "Chunk size", Type: "number", Section: "Layout config",
			Help: "optional: the most words in a chunk; empty for the parser's default"})
	case "OCR_PROCESSOR":
		fields = append(fields, console.Field{Name: "enableSymbol", Label: "Enable symbols", Type: "checkbox", Section: "OCR config",
			Help: "each character a row of its own, with its box"})
	}
	return console.Action{ID: "upload-test", Label: "Upload test document", Submit: "Upload", Fields: fields}
}

// docAIProcessOptions are an Upload test document's options as
// ProcessDocument takes them, and the processor or version it names.
func docAIProcessOptions(pr *documentaipb.Processor, v map[string]string) (string, *documentaipb.ProcessOptions, error) {
	name := pr.GetName()
	if id := strings.TrimSpace(v["version"]); id != "" {
		name += "/processorVersions/" + id
	}
	var opts *documentaipb.ProcessOptions
	if ps := strings.TrimSpace(v["pages"]); ps != "" {
		var pages []int32
		for _, f := range strings.Split(ps, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 {
				return "", nil, fmt.Errorf("pages are page numbers separated by commas, not %q", ps)
			}
			pages = append(pages, int32(n))
		}
		opts = &documentaipb.ProcessOptions{PageRange: &documentaipb.ProcessOptions_IndividualPageSelector_{
			IndividualPageSelector: &documentaipb.ProcessOptions_IndividualPageSelector{Pages: pages}}}
	}
	ensure := func() *documentaipb.ProcessOptions {
		if opts == nil {
			opts = &documentaipb.ProcessOptions{}
		}
		return opts
	}
	if cs := strings.TrimSpace(v["chunkSize"]); cs != "" && pr.GetType() == "LAYOUT_PARSER_PROCESSOR" {
		n, err := strconv.Atoi(cs)
		if err != nil || n < 1 {
			return "", nil, fmt.Errorf("the chunk size must be a positive number, not %q", cs)
		}
		ensure().LayoutConfig = &documentaipb.ProcessOptions_LayoutConfig{
			ChunkingConfig: &documentaipb.ProcessOptions_LayoutConfig_ChunkingConfig{ChunkSize: int32(n)}}
	}
	if v["enableSymbol"] == "true" && pr.GetType() == "OCR_PROCESSOR" {
		ensure().OcrConfig = &documentaipb.OcrConfig{EnableSymbol: true}
	}
	return name, opts, nil
}

// ActAtResult implements console.ResultActor: Batch process answers each
// document's status and where its result was written; every other action
// answers nothing.
func (p docAIProcessorsProvider) ActAtResult(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, error) {
	if out, ok, err := p.customActAt(ctx, project, path, action, v); ok {
		return out, err
	}
	if action != "batch-process" {
		return nil, p.ActAt(ctx, project, path, action, v)
	}
	if len(path) != 1 {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	pr, err := p.d.processor(ctx, project, path[0])
	if err != nil {
		return nil, err
	}
	in := strings.TrimSpace(v["input"])
	src := &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsPrefix{
		GcsPrefix: &documentaipb.GcsPrefix{GcsUriPrefix: in}}}
	if mt := docAIMimeTypes[strings.ToLower(filepath.Ext(in))]; mt != "" && !strings.HasSuffix(in, "/") {
		src = &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsDocuments{
			GcsDocuments: &documentaipb.GcsDocuments{Documents: []*documentaipb.GcsDocument{{GcsUri: in, MimeType: mt}}}}}
	}
	name := pr.GetName()
	if id := strings.TrimSpace(v["version"]); id != "" {
		name += "/processorVersions/" + id
	}
	gcs := &documentaipb.DocumentOutputConfig_GcsOutputConfig{GcsUri: strings.TrimSpace(v["output"])}
	if n := strings.TrimSpace(v["pagesPerShard"]); n != "" {
		pages, err := strconv.Atoi(n)
		if err != nil || pages <= 0 {
			return nil, fmt.Errorf("pages per shard must be a positive number, not %q", n)
		}
		gcs.ShardingConfig = &documentaipb.DocumentOutputConfig_GcsOutputConfig_ShardingConfig{PagesPerShard: int32(pages)}
	}
	out := &console.Listing{Columns: []string{"Status", "Result"}, NameColumn: "Document", Noun: "documents"}
	err = p.d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		op, err := c.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: name, InputDocuments: src,
			DocumentOutputConfig: &documentaipb.DocumentOutputConfig{Destination: &documentaipb.DocumentOutputConfig_GcsOutputConfig_{
				GcsOutputConfig: gcs}}})
		if err != nil {
			return err
		}
		_, werr := op.Wait(ctx)
		meta, _ := op.Metadata()
		for _, st := range meta.GetIndividualProcessStatuses() {
			state, res := "Succeeded", st.GetOutputGcsDestination()
			if st.GetStatus().GetCode() != 0 {
				state, res = "Failed", st.GetStatus().GetMessage()
			}
			out.Items = append(out.Items, console.Resource{Name: st.GetInputGcsSource(), Fields: map[string]string{"Status": state, "Result": res}})
		}
		out.Note = "Operation " + op.Name() + ": " + stateWord(meta.GetState().String()) + "."
		if werr != nil && len(out.Items) == 0 {
			return werr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.Total = len(out.Items)
	return out, nil
}

func (p docAIProcessorsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if _, ok, err := p.customActAt(ctx, project, path, action, v); ok {
		return err
	}
	if action == "batch-process" {
		_, err := p.ActAtResult(ctx, project, path, action, v)
		return err
	}
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if action == "delete" {
		return p.Delete(ctx, project, path[0])
	}
	pr, err := p.d.processor(ctx, project, path[0])
	if err != nil {
		return err
	}
	switch action {
	case "set-default-version":
		return p.d.setDefaultVersion(ctx, pr.GetName(), v["version"])
	case "deploy-versions", "undeploy-versions", "delete-versions":
		return p.d.changeVersions(ctx, pr.GetName(), strings.TrimSuffix(action, "-versions"), v[docAIVersionsField])
	}
	return p.d.setEnabled(ctx, pr.GetName(), action)
}

// docAIVersionsField is the selection field of Manage versions' actions.
const docAIVersionsField = "versions"

// docAIVersionSelectActions are Manage versions' Deploy, Undeploy and
// Delete over the checked versions, as
// https://docs.cloud.google.com/document-ai/docs/manage-processor-versions
// describes them (read 2026-11-19).
func docAIVersionSelectActions() []console.Action {
	sel := []console.Field{{Name: docAIVersionsField, Type: "hidden"}}
	return []console.Action{
		{ID: "deploy-versions", Label: "Deploy", SelectionField: docAIVersionsField, Fields: sel,
			Confirm: "The checked versions are deployed, so they can process documents."},
		{ID: "undeploy-versions", Label: "Undeploy", SelectionField: docAIVersionsField, Fields: sel,
			Confirm: "The checked versions are undeployed and stop processing documents."},
		{ID: "delete-versions", Label: "Delete", Destructive: true, SelectionField: docAIVersionsField, Fields: sel,
			Confirm: "The checked versions are deleted."},
	}
}

// docAIDefaultVersionAction is Google's Default version picker.
func docAIDefaultVersionAction(pr *documentaipb.Processor, vers []*documentaipb.ProcessorVersion) console.Action {
	var ids []string
	labels := map[string]string{}
	for _, v := range vers {
		id := lastSegment(v.GetName())
		ids = append(ids, id)
		labels[id] = v.GetDisplayName() + " (" + id + ")"
	}
	return console.Action{ID: "set-default-version", Label: "Default version", Submit: "Save", Fields: []console.Field{
		{Name: "version", Label: "Default version", Type: "select", Options: ids, OptionLabels: labels,
			Default: lastSegment(pr.GetDefaultProcessorVersion()), Required: true,
			Help: "the version a request that names the processor uses"}}}
}

// versions are a processor's versions, ListProcessorVersions'.
func (d docAI) versions(ctx context.Context, processor string) ([]*documentaipb.ProcessorVersion, error) {
	var out []*documentaipb.ProcessorVersion
	err := d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		it := c.ListProcessorVersions(ctx, &documentaipb.ListProcessorVersionsRequest{Parent: processor})
		for {
			v, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, v)
		}
	})
	return out, err
}

func (d docAI) setDefaultVersion(ctx context.Context, processor, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("choose a version")
	}
	return d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		op, err := c.SetDefaultProcessorVersion(ctx, &documentaipb.SetDefaultProcessorVersionRequest{Processor: processor,
			DefaultProcessorVersion: processor + "/processorVersions/" + strings.TrimSpace(id)})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
}

// changeVersions deploys, undeploys or deletes the versions named one per
// line, each through its own call, as the console sends one a version.
func (d docAI) changeVersions(ctx context.Context, processor, verb, ids string) error {
	var names []string
	for _, id := range strings.Fields(ids) {
		names = append(names, processor+"/processorVersions/"+id)
	}
	if len(names) == 0 {
		return errors.New("check the versions to " + verb)
	}
	return d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		for _, n := range names {
			var err error
			switch verb {
			case "deploy":
				var op *documentai.DeployProcessorVersionOperation
				if op, err = c.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: n}); err == nil {
					_, err = op.Wait(ctx)
				}
			case "undeploy":
				var op *documentai.UndeployProcessorVersionOperation
				if op, err = c.UndeployProcessorVersion(ctx, &documentaipb.UndeployProcessorVersionRequest{Name: n}); err == nil {
					_, err = op.Wait(ctx)
				}
			case "delete":
				var op *documentai.DeleteProcessorVersionOperation
				if op, err = c.DeleteProcessorVersion(ctx, &documentaipb.DeleteProcessorVersionRequest{Name: n}); err == nil {
					err = op.Wait(ctx)
				}
			default:
				return fmt.Errorf("unknown action %q", verb)
			}
			if err != nil {
				return fmt.Errorf("%s %s: %w", verb, lastSegment(n), err)
			}
		}
		return nil
	})
}

// docAIMimeTypes are the documented file types by extension, for a browser
// that names none.
var docAIMimeTypes = map[string]string{".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg",
	".jpeg": "image/jpeg", ".gif": "image/gif", ".tif": "image/tiff", ".tiff": "image/tiff", ".bmp": "image/bmp",
	".webp": "image/webp"}

func docAIMimeType(f console.UploadedFile) string {
	if m := docAIMimeTypes[strings.ToLower(filepath.Ext(f.Name))]; m != "" {
		return m
	}
	if m, _, err := mime.ParseMediaType(f.ContentType); err == nil && m != "" && m != "application/octet-stream" {
		return m
	}
	return ""
}

// ActAtFile implements console.FileActor: Upload test document.
func (p docAIProcessorsProvider) ActAtFile(ctx context.Context, project string, path []string, action string,
	v map[string]string, f console.UploadedFile) (*console.Listing, error) {
	if len(path) != 1 || action != "upload-test" {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	pr, err := p.d.processor(ctx, project, path[0])
	if err != nil {
		return nil, err
	}
	content, err := io.ReadAll(io.LimitReader(f, docAIMaxUpload+1))
	if err != nil {
		return nil, err
	}
	if len(content) > docAIMaxUpload {
		return nil, errors.New("the document is larger than Document AI's 40 MB online limit")
	}
	mt := docAIMimeType(f)
	if mt == "" {
		return nil, fmt.Errorf("%s is not a file type Document AI reads: PDF, PNG, JPEG, GIF, TIFF, BMP or WebP", f.Name)
	}
	name, opts, err := docAIProcessOptions(pr, v)
	if err != nil {
		return nil, err
	}
	var doc *documentaipb.Document
	err = p.d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		r, err := c.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: name, ProcessOptions: opts,
			Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: content, MimeType: mt}}})
		doc = r.GetDocument()
		return err
	})
	if err != nil {
		return nil, err
	}
	out := docAIResult(doc)
	if p.analyses != nil {
		out.Open = p.analyses.add(p.d.project(project), lastSegment(pr.GetName()), pr.GetType(), f.Name, name, out)
	}
	return out, nil
}

// docAILayoutBlocks are a document layout's blocks with the blocks nested
// in each, depth first.
func docAILayoutBlocks(in []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock) []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock {
	var out []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock
	for _, b := range in {
		out = append(out, b)
		out = append(out, docAILayoutBlocks(b.GetTextBlock().GetBlocks())...)
	}
	return out
}

// docAIResult is a processed document as the test panel shows it: its text,
// and its layout a row per element.
func docAIResult(doc *documentaipb.Document) *console.Listing {
	text := []rune(doc.GetText())
	slice := func(a *documentaipb.Document_TextAnchor) string {
		var b strings.Builder
		for _, s := range a.GetTextSegments() {
			st, en := s.GetStartIndex(), s.GetEndIndex()
			if st >= 0 && en <= int64(len(text)) && st <= en {
				b.WriteString(string(text[st:en]))
			}
		}
		return strings.Join(strings.Fields(b.String()), " ")
	}
	box := func(l *documentaipb.Document_Page_Layout) string {
		v := l.GetBoundingPoly().GetNormalizedVertices()
		if len(v) < 3 {
			return ""
		}
		return fmt.Sprintf("(%.3f, %.3f)–(%.3f, %.3f)", v[0].GetX(), v[0].GetY(), v[2].GetX(), v[2].GetY())
	}
	out := &console.Listing{Columns: []string{"Page", "Element", "Text", "Confidence", "Bounding box"}, NameColumn: "#",
		Noun: "layout elements", Text: doc.GetText()}
	var rp *console.ResultPage
	add := func(page int32, kind string, l *documentaipb.Document_Page_Layout) {
		name := strconv.Itoa(len(out.Items) + 1)
		out.Items = append(out.Items, console.Resource{Name: name, Fields: map[string]string{
			"Page": strconv.Itoa(int(page)), "Element": kind, "Text": slice(l.GetTextAnchor()),
			"Confidence": strconv.FormatFloat(float64(l.GetConfidence()), 'f', 2, 32), "Bounding box": box(l)}})
		if v := l.GetBoundingPoly().GetNormalizedVertices(); rp != nil && len(v) >= 3 {
			x0, y0, x1, y1 := float64(v[0].GetX()), float64(v[0].GetY()), float64(v[2].GetX()), float64(v[2].GetY())
			rp.Boxes = append(rp.Boxes, console.ResultBox{Row: name, Kind: kind, X: x0, Y: y0, W: x1 - x0, H: y1 - y0})
		}
	}
	for _, pg := range doc.GetPages() {
		// The page's image, the boxes of its layout drawn over it (#1930).
		rp = nil
		if img := pg.GetImage(); len(img.GetContent()) > 0 && img.GetMimeType() != "" {
			out.Pages = append(out.Pages, console.ResultPage{Label: fmt.Sprintf("Page %d", pg.GetPageNumber()),
				Image: "data:" + img.GetMimeType() + ";base64," + base64.StdEncoding.EncodeToString(img.GetContent()),
				Width: int(img.GetWidth()), Height: int(img.GetHeight())})
			rp = &out.Pages[len(out.Pages)-1]
		}
		for _, b := range pg.GetBlocks() {
			add(pg.GetPageNumber(), "Block", b.GetLayout())
		}
		for _, b := range pg.GetParagraphs() {
			add(pg.GetPageNumber(), "Paragraph", b.GetLayout())
		}
		for _, b := range pg.GetLines() {
			add(pg.GetPageNumber(), "Line", b.GetLayout())
		}
		for _, b := range pg.GetTokens() {
			add(pg.GetPageNumber(), "Token", b.GetLayout())
		}
		for _, b := range pg.GetSymbols() {
			add(pg.GetPageNumber(), "Symbol", b.GetLayout())
		}
	}
	// A custom processor's entities: the extractor's values, the
	// classifier's classes, the splitter's subdocuments (#2124).
	for _, e := range doc.GetEntities() {
		var pages []string
		for _, r := range e.GetPageAnchor().GetPageRefs() {
			pages = append(pages, strconv.Itoa(int(r.GetPage())+1))
		}
		out.Items = append(out.Items, console.Resource{Name: strconv.Itoa(len(out.Items) + 1), Fields: map[string]string{
			"Page": strings.Join(pages, ", "), "Element": "Entity (" + e.GetType() + ")", "Text": entityText(doc, e),
			"Confidence": strconv.FormatFloat(float64(e.GetConfidence()), 'f', 2, 32)}})
	}
	// The layout parser's blocks and chunks.
	var lt bytes.Buffer
	// The layout parser's blocks organized by their detected type, as
	// Google's layout parser analysis page shows them, in reading order
	// within each type.
	blocks := docAILayoutBlocks(doc.GetDocumentLayout().GetBlocks())
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].GetTextBlock().GetType() < blocks[j].GetTextBlock().GetType()
	})
	for _, b := range blocks {
		tb := b.GetTextBlock()
		out.Items = append(out.Items, console.Resource{Name: strconv.Itoa(len(out.Items) + 1), Fields: map[string]string{
			"Page": strconv.Itoa(int(b.GetPageSpan().GetPageStart())), "Element": "Layout block (" + tb.GetType() + ")",
			"Text": tb.GetText()}})
	}
	// The text in reading order: the top-level blocks, each holding its own.
	for _, b := range doc.GetDocumentLayout().GetBlocks() {
		lt.WriteString(b.GetTextBlock().GetText() + "\n\n")
	}
	for _, c := range doc.GetChunkedDocument().GetChunks() {
		out.Items = append(out.Items, console.Resource{Name: strconv.Itoa(len(out.Items) + 1), Fields: map[string]string{
			"Page": strconv.Itoa(int(c.GetPageSpan().GetPageStart())), "Element": "Chunk " + c.GetChunkId(),
			"Text": strings.Join(strings.Fields(c.GetContent()), " ")}})
	}
	if out.Text == "" {
		out.Text = strings.TrimSpace(lt.String())
	}
	out.Total = len(out.Items)
	out.Note = fmt.Sprintf("%d pages; the text, then each layout element with its text, confidence and normalized bounding box, "+
		"drawn over its page. Point at a row or a box to light up the other.", len(doc.GetPages()))
	if len(doc.GetPages()) == 0 {
		out.Note = "The document's layout blocks and chunks, as the layout parser answers them."
	}
	return out
}

var (
	_ console.Driller     = docAIGalleryProvider{}
	_ console.ResultActor = docAIGalleryProvider{}
	_ console.Actor       = docAIGalleryProvider{}
	_ console.Creator     = docAIProcessorsProvider{}
	_ console.Deleter     = docAIProcessorsProvider{}
	_ console.Driller     = docAIProcessorsProvider{}
	_ console.Actor       = docAIProcessorsProvider{}
	_ console.FileActor   = docAIProcessorsProvider{}
	_ console.ResultActor = docAIProcessorsProvider{}
)

// customActAt runs a custom processor's own actions (#2124): its deeper
// pages', and its tabs'. ok is false for any other.
func (p docAIProcessorsProvider) customActAt(ctx context.Context, project string, path []string, action string,
	v map[string]string) (*console.Listing, bool, error) {
	if len(path) > 1 {
		return p.customPathAct(ctx, project, path, action, v)
	}
	if len(path) != 1 {
		return nil, false, nil
	}
	switch action {
	case "configure-dataset", "import-documents", "filter-documents", "assign-train", "assign-test", "assign-unassigned",
		"delete-documents", "add-field", "delete-fields", "edit-prompt", "train-version", "evaluate-version":
	default:
		return nil, false, nil
	}
	pr, err := p.d.processor(ctx, project, path[0])
	if err != nil {
		return nil, true, err
	}
	if docAICustomKinds[pr.GetType()] == "" {
		return nil, true, fmt.Errorf("%s is for a custom processor; %s is %s", action, path[0], docAITypeLabel(pr.GetType()))
	}
	return p.customAct(ctx, project, pr, action, v)
}
