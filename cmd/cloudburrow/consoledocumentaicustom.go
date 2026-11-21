package main

// Document AI's custom processors in the console (#2124): a Custom
// Extractor's, Classifier's or Splitter's page has Google's Workbench tabs
// (https://docs.cloud.google.com/document-ai/docs/workbench/build-custom-processor,
// read 2026-11-21): Get started (the fields of the schema and the document
// prompt), Build (the dataset: its Cloud Storage, Import documents with a
// data split or auto-split and a document type, the documents with their
// split and labeling state, filtered and deleted, and Create new version,
// which trains), Deploy & use (the versions, Deploy, Undeploy, Delete and
// Set as default) and Evaluate & test (each version's evaluations, F1,
// precision and recall; an evaluation's page has them for the full document
// and each label at a confidence threshold, the optimal one by default).
// A document's labeling page draws its pages with its labels boxed over
// them; a box drawn over a page labels the words in it with the field
// chosen, as Google's bounding box tool does, and Add label also takes the
// value as text (the select-text tool).
//
// Every read and change goes through the official clients (v1
// DocumentProcessorService, v1beta3 DocumentService), except the labels a
// document is saved with: Google's labeling tool saves through a method
// the public API does not have, so here the console calls the local
// endpoint's LabelDatasetDocument in process. Rechecked for #2097: v1beta3's
// dataset has no updateDocument or batchUpdateDocuments, updateDataset
// changes only the dataset's settings, and importDocuments always adds a
// document under a new ID, so none of them edits a document's labels.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	documentaibeta "cloud.google.com/go/documentai/apiv1beta3"
	beta "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// docAILabeler saves a dataset document's labels and split.
type docAILabeler interface {
	LabelDatasetDocument(ctx context.Context, processor, id string, entities []*documentaipb.Document_Entity, split string) error
}

// docAICustomKinds are the custom types by the model they train.
var docAICustomKinds = map[string]string{
	"CUSTOM_EXTRACTION_PROCESSOR":     "extractor",
	"CUSTOM_CLASSIFICATION_PROCESSOR": "classifier",
	"CUSTOM_SPLITTING_PROCESSOR":      "splitter",
}

// Path segments under a custom processor's page.
const (
	docAIDocumentSegment   = "documents"
	docAIEvaluationSegment = "evaluations"
)

// docAISplits are the dataset splits as Google's Build tab names them.
var docAISplits = map[string]string{"DATASET_SPLIT_TRAIN": "Training", "DATASET_SPLIT_TEST": "Test",
	"DATASET_SPLIT_UNASSIGNED": "Unassigned"}

func splitLabel(s string) string {
	if l := docAISplits[s]; l != "" {
		return l
	}
	return "Unassigned"
}

// docAIValueTypes are the field types of Google's Create new field.
var docAIValueTypes = []string{"string", "number", "money", "datetime", "address", "boolean"}

var docAIValueTypeLabels = map[string]string{"string": "Plain text", "number": "Number", "money": "Money",
	"datetime": "Datetime", "address": "Address", "boolean": "Checkbox"}

var docAIOccurrences = []string{"OPTIONAL_ONCE", "OPTIONAL_MULTIPLE", "REQUIRED_ONCE", "REQUIRED_MULTIPLE"}

var docAIOccurrenceLabels = map[string]string{"OPTIONAL_ONCE": "Optional once", "OPTIONAL_MULTIPLE": "Optional multiple",
	"REQUIRED_ONCE": "Required once", "REQUIRED_MULTIPLE": "Required multiple"}

func (d docAI) withDataset(ctx context.Context, fn func(*documentaibeta.DocumentClient) error) error {
	addr := ""
	if d.addr != nil {
		addr = d.addr()
	}
	if addr == "" {
		return errors.New("the local AI endpoint, which serves Document AI, is not running")
	}
	c, err := documentaibeta.NewDocumentClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Document AI: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// toV1 is a v1beta3 message as its v1 twin, field for field.
func toV1[T proto.Message](in proto.Message, out T) T {
	b, _ := protojson.Marshal(in)
	_ = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, out)
	return out
}

// docAIDataset is a custom processor's dataset, schema and documents.
type docAIDataset struct {
	ds     *beta.Dataset
	schema *documentaipb.DocumentSchema
	docs   []*beta.DocumentMetadata
}

func (d docAI) dataset(ctx context.Context, pr *documentaipb.Processor, filter string) (docAIDataset, error) {
	var out docAIDataset
	name := pr.GetName() + "/dataset"
	err := d.withDataset(ctx, func(c *documentaibeta.DocumentClient) error {
		sc, err := c.GetDatasetSchema(ctx, &beta.GetDatasetSchemaRequest{Name: name + "/datasetSchema"})
		if err != nil {
			return err
		}
		out.schema = toV1(sc.GetDocumentSchema(), &documentaipb.DocumentSchema{})
		out.ds = &beta.Dataset{Name: name}
		it := c.ListDocuments(ctx, &beta.ListDocumentsRequest{Dataset: name, Filter: filter, PageSize: 100})
		for {
			m, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				// An uninitialized dataset lists nothing.
				if strings.Contains(err.Error(), "not initialized") {
					return nil
				}
				return err
			}
			out.docs = append(out.docs, m)
		}
	})
	return out, err
}

// docAIFields are the schema's fields as Get started lists them: an
// extractor's document type's properties, a classifier's or splitter's
// document types.
func docAIFields(kind string, sc *documentaipb.DocumentSchema) []console.Resource {
	var out []console.Resource
	for _, et := range sc.GetEntityTypes() {
		if kind != "extractor" {
			if len(et.GetProperties()) == 0 {
				out = append(out, console.Resource{Name: et.GetName(), Selectable: true, Fields: map[string]string{
					"Display name": et.GetDisplayName(), "Kind": "Document type"}})
			}
			continue
		}
		for _, p := range et.GetProperties() {
			out = append(out, console.Resource{Name: p.GetName(), Selectable: true, Fields: map[string]string{
				"Display name": p.GetDisplayName(), "Type": docAIValueTypeLabels[p.GetValueType()] + orRaw(p.GetValueType()),
				"Occurrence": docAIOccurrenceLabels[p.GetOccurrenceType().String()]}})
		}
	}
	return out
}

// orRaw is a value type the labels do not name, in brackets.
func orRaw(v string) string {
	if _, ok := docAIValueTypeLabels[v]; ok || v == "" {
		return ""
	}
	return v
}

// docAILabelTypes are the types a label can have.
func docAILabelTypes(kind string, sc *documentaipb.DocumentSchema) []string {
	var out []string
	for _, f := range docAIFields(kind, sc) {
		out = append(out, f.Name)
	}
	return out
}

// customSections are a custom processor's Get started, Build and Evaluate
// & test tabs.
func (p docAIProcessorsProvider) customSections(ctx context.Context, project string, pr *documentaipb.Processor,
	vers []*documentaipb.ProcessorVersion) []console.Section {
	kind := docAICustomKinds[pr.GetType()]
	ds, err := p.d.dataset(ctx, pr, "")
	if err != nil {
		e := consoleErr(err)
		return []console.Section{{ID: "get-started", Label: "Get started", Unavailable: e}, {ID: "build", Label: "Build", Unavailable: e}}
	}
	q := "?project=" + url.QueryEscape(p.d.project(project))
	base := "/document-ai/processors/" + url.PathEscape(lastSegment(pr.GetName()))

	// Get started: the fields and the document prompt.
	cols := []string{"Display name", "Type", "Occurrence"}
	noun, what := "fields", "Fields"
	if kind != "extractor" {
		cols, noun, what = []string{"Display name", "Kind"}, "document types", "Document types"
	}
	fields := docAIFields(kind, ds.schema)
	prompt := ds.schema.GetDescription()
	if prompt == "" {
		prompt = "none"
	}
	start := console.Section{ID: "get-started", Label: "Get started", Listing: console.Listing{Columns: cols, NameColumn: "Name",
		Noun: noun, Items: fields, Total: len(fields), SelectActions: []console.Action{{ID: "delete-fields", Label: "Delete",
			Destructive: true, SelectionField: docAIVersionsField, Fields: []console.Field{{Name: docAIVersionsField, Type: "hidden"}},
			Confirm: "The checked " + noun + " are removed from the schema; labels of them stay on the documents."}}},
		Actions: docAISchemaActions(kind),
		Note:    what + " of the processor's schema, which training uses. Document prompt: " + prompt + "."}

	// Build: the dataset and its documents.
	counts := map[string]int{}
	labeled := 0
	build := console.Section{ID: "build", Label: "Build", Listing: console.Listing{
		Columns: []string{"Split", "Labeling state", "Pages"}, NameColumn: "Document", Noun: "documents", AlwaysStatus: false,
		SelectActions: docAIDocumentSelectActions()}, Actions: p.buildActions(pr, kind, ds.schema, vers)}
	for _, m := range ds.docs {
		id := m.GetDocumentId().GetGcsManagedDocId().GetCwDocId()
		counts[m.GetDatasetType().String()]++
		state := docAILabelState(m.GetLabelingState().String())
		if state == "Labeled" {
			labeled++
		}
		build.Listing.Items = append(build.Listing.Items, console.Resource{Name: m.GetDisplayName(), Selectable: true,
			Link: base + "/" + docAIDocumentSegment + "/" + url.PathEscape(id) + q,
			Fields: map[string]string{"Split": splitLabel(m.GetDatasetType().String()), "Labeling state": state,
				"Pages": strconv.Itoa(int(m.GetPageCount()))}})
	}
	build.Listing.Total = len(build.Listing.Items)
	build.Note = fmt.Sprintf("%d documents: %d training, %d test, %d unassigned; %d labeled. Open a document to label it. "+
		"Create new version trains a version on the labeled training documents and evaluates it on the labeled test documents.",
		len(ds.docs), counts["DATASET_SPLIT_TRAIN"], counts["DATASET_SPLIT_TEST"], counts["DATASET_SPLIT_UNASSIGNED"], labeled)
	if len(ds.docs) == 0 {
		build.Note = "No documents yet. Configure the dataset's storage, then Import documents. " + build.Note[strings.Index(build.Note, "Create"):]
	}
	return []console.Section{start, build, p.customEvaluations(ctx, project, pr, vers)}
}

func docAILabelState(s string) string {
	switch s {
	case "DOCUMENT_LABELED":
		return "Labeled"
	case "DOCUMENT_UNLABELED":
		return "Unlabeled"
	case "DOCUMENT_AUTO_LABELED":
		return "Auto-labeled"
	}
	return "Unlabeled"
}

func docAISchemaActions(kind string) []console.Action {
	if kind != "extractor" {
		return []console.Action{{ID: "add-field", Label: "Create new document type", Submit: "Create", Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9_\-]{0,63}$`,
				Help: "lowercase letters, digits, - and _, as the class's entity type"}}},
			docAIPromptAction()}
	}
	return []console.Action{{ID: "add-field", Label: "Create new field", Submit: "Create", Fields: []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9_\-]{0,63}$`,
			Help: "lowercase letters, digits, - and _: the entity type of the values"},
		{Name: "valueType", Label: "Data type", Type: "select", Options: docAIValueTypes, OptionLabels: docAIValueTypeLabels, Default: "string"},
		{Name: "occurrence", Label: "Occurrence", Type: "select", Options: docAIOccurrences, OptionLabels: docAIOccurrenceLabels,
			Default: "OPTIONAL_ONCE"},
		{Name: "description", Label: "Description", Type: "text", Help: "optional: what the field holds, which the model reads"}}},
		docAIPromptAction()}
}

func docAIPromptAction() console.Action {
	return console.Action{ID: "edit-prompt", Label: "Edit document prompt", Submit: "Save", Fields: []console.Field{
		{Name: "prompt", Label: "Document prompt", Type: "textarea", Pattern: `^[\s\S]{0,500}$`,
			Help: "what the documents are, at most 500 characters; the generative model reads it"}}}
}

func docAIDocumentSelectActions() []console.Action {
	sel := []console.Field{{Name: docAIVersionsField, Type: "hidden"}}
	return []console.Action{
		{ID: "assign-train", Label: "Assign to training", SelectionField: docAIVersionsField, Fields: sel},
		{ID: "assign-test", Label: "Assign to test", SelectionField: docAIVersionsField, Fields: sel},
		{ID: "assign-unassigned", Label: "Unassign", SelectionField: docAIVersionsField, Fields: sel},
		{ID: "delete-documents", Label: "Delete", Destructive: true, SelectionField: docAIVersionsField, Fields: sel,
			Confirm: "The checked documents are deleted from the dataset."},
	}
}

// buildActions are the Build tab's: Configure dataset, Import documents,
// Filter documents and Create new version.
func (p docAIProcessorsProvider) buildActions(pr *documentaipb.Processor, kind string, sc *documentaipb.DocumentSchema,
	vers []*documentaipb.ProcessorVersion) []console.Action {
	imp := []console.Field{
		{Name: "source", Label: "Source path", Type: "text", Required: true, Browse: "storage-path", Pattern: `^gs://[^\/]+(\/.*)?$`,
			Help: "gs://bucket/folder/ for every document under it, or gs://bucket/file.pdf; Document JSON is imported with its labels"},
		{Name: "split", Label: "Data split", Type: "select", Options: []string{"DATASET_SPLIT_TRAIN", "DATASET_SPLIT_TEST",
			"DATASET_SPLIT_UNASSIGNED", "auto"}, OptionLabels: map[string]string{"DATASET_SPLIT_TRAIN": "Training",
			"DATASET_SPLIT_TEST": "Test", "DATASET_SPLIT_UNASSIGNED": "Unassigned", "auto": "Auto-split (80% training, 20% test)"},
			Default: "DATASET_SPLIT_TRAIN"}}
	if kind != "extractor" {
		types := docAILabelTypes(kind, sc)
		imp = append(imp, console.Field{Name: "documentType", Label: "Document type", Type: "select", Options: append([]string{""}, types...),
			OptionLabels: map[string]string{"": "None (label later)"}, Help: "labels every imported document with this type"})
	}
	var custom []string
	labels := map[string]string{"": "None"}
	for _, v := range vers {
		custom = append(custom, lastSegment(v.GetName()))
		labels[lastSegment(v.GetName())] = v.GetDisplayName() + " (" + lastSegment(v.GetName()) + ")"
	}
	trainHelp := "Trains on the labeled training documents and evaluates on the labeled test documents. Where this instance " +
		"serves a local generation model the version is generative: the schema and up to three labeled documents per type are its " +
		"prompt (an extractor with no labeled document is zero-shot); without one, a counting model is trained."
	return []console.Action{
		{ID: "configure-dataset", Label: "Configure dataset", Submit: "Save", Fields: []console.Field{
			{Name: "storage", Label: "Dataset location", Type: "radio", Options: []string{"managed", "unmanaged"},
				OptionLabels: map[string]string{"managed": "Cloud Storage folder (managed)", "unmanaged": "Unmanaged (kept by Document AI)"},
				Default:      "managed"},
			{Name: "gcsPrefix", Label: "Cloud Storage folder", Type: "text", ShowWhen: "storage=managed", Browse: "storage", Pattern: `^(gs://[^\/]+(\/.*)?)?$`,
				Help: "gs://bucket/folder: each imported document is written there as Document JSON"}}},
		{ID: "import-documents", Label: "Import documents", Submit: "Import", Fields: imp},
		{ID: "filter-documents", Label: "Filter documents", Submit: "Filter", Fields: []console.Field{
			{Name: "split", Label: "Split", Type: "select", Options: []string{"", "DATASET_SPLIT_TRAIN", "DATASET_SPLIT_TEST", "DATASET_SPLIT_UNASSIGNED"},
				OptionLabels: map[string]string{"": "Any", "DATASET_SPLIT_TRAIN": "Training", "DATASET_SPLIT_TEST": "Test", "DATASET_SPLIT_UNASSIGNED": "Unassigned"}},
			{Name: "state", Label: "Labeling state", Type: "select", Options: []string{"", "DOCUMENT_LABELED", "DOCUMENT_UNLABELED"},
				OptionLabels: map[string]string{"": "Any", "DOCUMENT_LABELED": "Labeled", "DOCUMENT_UNLABELED": "Unlabeled"}},
			{Name: "label", Label: "Label", Type: "select", Options: append([]string{""}, docAILabelTypes(kind, sc)...),
				OptionLabels: map[string]string{"": "Any"}},
			{Name: "name", Label: "Display name", Type: "text", Help: "optional: * matches any characters"}}},
		{ID: "train-version", Label: "Create new version", Submit: "Create version", Fields: []console.Field{
			{Name: "name", Label: "Version name", Type: "text", Required: true, Pattern: `^.{1,63}$`},
			{Name: "base", Label: "Base version", Type: "select", Options: append([]string{""}, custom...), OptionLabels: labels,
				Help: "optional: uptrain from this version"}}, Title: trainHelp},
	}
}

// customEvaluations is Evaluate & test: every version's evaluations.
func (p docAIProcessorsProvider) customEvaluations(ctx context.Context, project string, pr *documentaipb.Processor,
	vers []*documentaipb.ProcessorVersion) console.Section {
	q := "?project=" + url.QueryEscape(p.d.project(project))
	base := "/document-ai/processors/" + url.PathEscape(lastSegment(pr.GetName()))
	sec := console.Section{ID: "evaluate", Label: "Evaluate & test", Listing: console.Listing{
		Columns: []string{"Version", "Created", "Documents", "F1 score", "Precision", "Recall", "Threshold"}, NameColumn: "Evaluation",
		Noun: "evaluations"}, Actions: []console.Action{docAIEvaluateAction(vers)},
		Note: "Each version is evaluated on the labeled test documents when it is trained, and again with Run new evaluation. " +
			"Scores are at the confidence threshold that maximizes F1; open an evaluation for each label and other thresholds. " +
			"Upload test document, above, runs a version on a document of your own."}
	err := p.d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		for _, v := range vers {
			it := c.ListEvaluations(ctx, &documentaipb.ListEvaluationsRequest{Parent: v.GetName()})
			for {
				e, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					return err
				}
				best := bestConfidence(e.GetAllEntitiesMetrics().GetConfidenceLevelMetrics())
				m := best.GetMetrics()
				sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(e.GetName()),
					Link: base + "/" + docAIEvaluationSegment + "/" + url.PathEscape(lastSegment(v.GetName())) + "/" + url.PathEscape(lastSegment(e.GetName())) + q,
					Fields: map[string]string{"Version": v.GetDisplayName(), "Created": stampAge(e.GetCreateTime()),
						"Documents": strconv.Itoa(int(e.GetDocumentCounters().GetEvaluatedDocumentsCount())),
						"F1 score":  pct(m.GetF1Score()), "Precision": pct(m.GetPrecision()), "Recall": pct(m.GetRecall()),
						"Threshold": strconv.FormatFloat(float64(best.GetConfidenceLevel()), 'f', 2, 32)}})
			}
		}
		return nil
	})
	if err != nil {
		sec.Unavailable = consoleErr(err)
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

func docAIEvaluateAction(vers []*documentaipb.ProcessorVersion) console.Action {
	var ids []string
	labels := map[string]string{}
	for _, v := range vers {
		ids = append(ids, lastSegment(v.GetName()))
		labels[lastSegment(v.GetName())] = v.GetDisplayName() + " (" + lastSegment(v.GetName()) + ")"
	}
	def := ""
	if len(ids) > 0 {
		def = ids[len(ids)-1]
	}
	return console.Action{ID: "evaluate-version", Label: "Run new evaluation", Submit: "Evaluate", Fields: []console.Field{
		{Name: "version", Label: "Version", Type: "select", Options: ids, OptionLabels: labels, Default: def, Required: true},
		{Name: "documents", Label: "Test documents", Type: "text", Browse: "storage", Pattern: `^(gs://[^\/]+(\/.*)?)?$`,
			Help: "optional: gs://bucket/folder/ of labeled Document JSON; empty for the dataset's test split"}}}
}

func pct(v float32) string { return strconv.FormatFloat(float64(v)*100, 'f', 1, 32) + "%" }

func bestConfidence(levels []*documentaipb.Evaluation_ConfidenceLevelMetrics) *documentaipb.Evaluation_ConfidenceLevelMetrics {
	var best *documentaipb.Evaluation_ConfidenceLevelMetrics
	for _, l := range levels {
		if best == nil || l.GetMetrics().GetF1Score() > best.GetMetrics().GetF1Score() {
			best = l
		}
	}
	return best
}

// atConfidence is the level closest to threshold.
func atConfidence(levels []*documentaipb.Evaluation_ConfidenceLevelMetrics, threshold float64) *documentaipb.Evaluation_ConfidenceLevelMetrics {
	var at *documentaipb.Evaluation_ConfidenceLevelMetrics
	for _, l := range levels {
		if at == nil || abs(float64(l.GetConfidenceLevel())-threshold) < abs(float64(at.GetConfidenceLevel())-threshold) {
			at = l
		}
	}
	return at
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// evaluationDetail is an evaluation's page: path is version, evaluation
// and optionally the confidence threshold.
func (p docAIProcessorsProvider) evaluationDetail(ctx context.Context, project, processor string, path []string) console.Detail {
	pr, err := p.d.processor(ctx, project, processor)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}
	}
	name := pr.GetName() + "/processorVersions/" + path[0] + "/evaluations/" + path[1]
	var ev *documentaipb.Evaluation
	var ver *documentaipb.ProcessorVersion
	err = p.d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		var e error
		if ev, e = c.GetEvaluation(ctx, &documentaipb.GetEvaluationRequest{Name: name}); e != nil {
			return e
		}
		ver, e = c.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: pr.GetName() + "/processorVersions/" + path[0]})
		return e
	})
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}
	}
	all := ev.GetAllEntitiesMetrics().GetConfidenceLevelMetrics()
	best := bestConfidence(all)
	threshold := float64(best.GetConfidenceLevel())
	chosen := "optimal (maximizes F1)"
	if len(path) > 2 {
		if t, err := strconv.ParseFloat(path[2], 64); err == nil && t >= 0 && t <= 1 {
			threshold, chosen = t, "chosen"
		}
	}
	at := atConfidence(all, threshold).GetMetrics()
	summary := []console.Property{{Label: "Version", Value: ver.GetDisplayName() + " (" + path[0] + ")"},
		{Label: "Created", Value: stampAge(ev.GetCreateTime())},
		{Label: "Documents evaluated", Value: strconv.Itoa(int(ev.GetDocumentCounters().GetEvaluatedDocumentsCount()))},
		{Label: "Invalid documents", Value: strconv.Itoa(int(ev.GetDocumentCounters().GetInvalidDocumentsCount()))},
		{Label: "Confidence threshold", Value: strconv.FormatFloat(threshold, 'f', 2, 64) + ", " + chosen},
		{Label: "F1 score", Value: pct(at.GetF1Score())}, {Label: "Precision", Value: pct(at.GetPrecision())},
		{Label: "Recall", Value: pct(at.GetRecall())},
		{Label: "AUPRC", Value: strconv.FormatFloat(float64(ev.GetAllEntitiesMetrics().GetAuprc()), 'f', 3, 32)},
		{Label: "Estimated calibration error", Value: strconv.FormatFloat(float64(ev.GetAllEntitiesMetrics().GetEstimatedCalibrationError()), 'f', 3, 32)}}
	row := func(label string, m *documentaipb.Evaluation_Metrics) console.Resource {
		return console.Resource{Name: label, Fields: map[string]string{"Precision": pct(m.GetPrecision()), "Recall": pct(m.GetRecall()),
			"F1 score": pct(m.GetF1Score()), "True positives": strconv.Itoa(int(m.GetTruePositivesCount())),
			"False positives": strconv.Itoa(int(m.GetFalsePositivesCount())), "False negatives": strconv.Itoa(int(m.GetFalseNegativesCount()))}}
	}
	labels := console.Listing{Columns: []string{"Precision", "Recall", "F1 score", "True positives", "False positives", "False negatives"},
		NameColumn: "Label", Noun: "labels", Items: []console.Resource{row("All labels", at)}}
	var types []string
	for t := range ev.GetEntityMetrics() {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		labels.Items = append(labels.Items, row(t, atConfidence(ev.GetEntityMetrics()[t].GetConfidenceLevelMetrics(), threshold).GetMetrics()))
	}
	labels.Total = len(labels.Items)
	levels := console.Listing{Columns: []string{"Precision", "Recall", "F1 score"}, NameColumn: "Confidence threshold", Noun: "thresholds"}
	var opts []string
	for _, l := range all {
		c := strconv.FormatFloat(float64(l.GetConfidenceLevel()), 'f', 2, 32)
		opts = append(opts, c)
		levels.Items = append(levels.Items, console.Resource{Name: c, Fields: map[string]string{"Precision": pct(l.GetMetrics().GetPrecision()),
			"Recall": pct(l.GetMetrics().GetRecall()), "F1 score": pct(l.GetMetrics().GetF1Score())}})
	}
	levels.Total = len(levels.Items)
	setT := console.Action{ID: "set-threshold", Label: "Confidence threshold", Submit: "Apply", Fields: []console.Field{
		{Name: "threshold", Label: "Confidence threshold", Type: "select", Options: opts, Required: true,
			Default: strconv.FormatFloat(threshold, 'f', 2, 64), Help: "the scores count only predictions at least this confident"}}}
	return console.Detail{Heading: "Evaluation", Title: path[1],
		Trail:   []console.Crumb{{Label: processor, Path: []string{processor}}, {Label: "Evaluation " + path[1]}},
		Summary: summary,
		Sections: []console.Section{
			{ID: "labels", Label: "Labels", Listing: labels, Actions: []console.Action{setT},
				Note: "Precision, recall and F1 for the full document and each label at the confidence threshold, matched case and " +
					"spacing ignored (fuzzy matching)."},
			{ID: "thresholds", Label: "Confidence thresholds", Listing: levels,
				Note: "All labels at each confidence threshold: a higher threshold trades recall for precision."},
		}}
}

func docAIEvaluationHref(project, processor, version, eval, threshold string) string {
	h := "/document-ai/processors/" + url.PathEscape(processor) + "/" + docAIEvaluationSegment + "/" + url.PathEscape(version) +
		"/" + url.PathEscape(eval)
	if threshold != "" {
		h += "/" + url.PathEscape(threshold)
	}
	return h + "?project=" + url.QueryEscape(project)
}

// --- labeling ----------------------------------------------------------------

func (d docAI) document(ctx context.Context, pr *documentaipb.Processor, id string) (*documentaipb.Document, *beta.DocumentMetadata, error) {
	var doc *documentaipb.Document
	var meta *beta.DocumentMetadata
	err := d.withDataset(ctx, func(c *documentaibeta.DocumentClient) error {
		r, err := c.GetDocument(ctx, &beta.GetDocumentRequest{Dataset: pr.GetName() + "/dataset",
			DocumentId: &beta.DocumentId{Type: &beta.DocumentId_GcsManagedDocId{GcsManagedDocId: &beta.DocumentId_GCSManagedDocumentId{CwDocId: id}}}})
		if err != nil {
			return err
		}
		doc = toV1(r.GetDocument(), &documentaipb.Document{})
		it := c.ListDocuments(ctx, &beta.ListDocumentsRequest{Dataset: pr.GetName() + "/dataset", PageSize: 100})
		for {
			m, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			if m.GetDocumentId().GetGcsManagedDocId().GetCwDocId() == id {
				meta = m
			}
		}
	})
	if err == nil && meta == nil {
		err = fmt.Errorf("document %s not found", id)
	}
	return doc, meta, err
}

// entityText is an entity's value.
func entityText(doc *documentaipb.Document, e *documentaipb.Document_Entity) string {
	if e.GetMentionText() != "" {
		return e.GetMentionText()
	}
	text := []rune(doc.GetText())
	var b strings.Builder
	for _, s := range e.GetTextAnchor().GetTextSegments() {
		if st, en := s.GetStartIndex(), s.GetEndIndex(); st >= 0 && en <= int64(len(text)) && st < en {
			b.WriteString(string(text[st:en]))
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// docAILabelView is a document as the labeling page draws it: its pages
// with each label's box, and a row per label.
func docAILabelView(doc *documentaipb.Document) *console.Listing {
	out := &console.Listing{Columns: []string{"Label", "Value", "Pages", "Confidence"}, NameColumn: "#", Noun: "labels"}
	pageOf := map[int]int{}
	for i, pg := range doc.GetPages() {
		if img := pg.GetImage(); len(img.GetContent()) > 0 && img.GetMimeType() != "" {
			pageOf[i] = len(out.Pages)
			out.Pages = append(out.Pages, console.ResultPage{Label: fmt.Sprintf("Page %d", i+1),
				Image: "data:" + img.GetMimeType() + ";base64," + b64(img.GetContent()), Width: int(img.GetWidth()), Height: int(img.GetHeight())})
		}
	}
	for i, e := range doc.GetEntities() {
		name := strconv.Itoa(i + 1)
		var pages []string
		for _, r := range e.GetPageAnchor().GetPageRefs() {
			pages = append(pages, strconv.Itoa(int(r.GetPage())+1))
			v := r.GetBoundingPoly().GetNormalizedVertices()
			if k, ok := pageOf[int(r.GetPage())]; ok && len(v) >= 3 {
				x0, y0, x1, y1 := float64(v[0].GetX()), float64(v[0].GetY()), float64(v[2].GetX()), float64(v[2].GetY())
				out.Pages[k].Boxes = append(out.Pages[k].Boxes, console.ResultBox{Row: name, Kind: "Label", X: x0, Y: y0, W: x1 - x0, H: y1 - y0})
			}
		}
		conf := e.GetConfidence()
		if conf == 0 {
			conf = 1
		}
		out.Items = append(out.Items, console.Resource{Name: name, Fields: map[string]string{"Label": e.GetType(),
			"Value": entityText(doc, e), "Pages": strings.Join(pages, ", "), "Confidence": strconv.FormatFloat(float64(conf), 'f', 2, 32)}})
	}
	out.Total = len(out.Items)
	out.Text = doc.GetText()
	out.Note = "Draw a box over a page to label the words in it, or use Add label with the value's text. Point at a label to light up its box."
	if len(out.Pages) == 0 {
		out.Note = "This document has no page images (it was imported as Document JSON): label it with Add label and the value's text."
	}
	return out
}

// documentDetail is a dataset document's labeling page.
func (p docAIProcessorsProvider) documentDetail(ctx context.Context, project, processor, id string) console.Detail {
	pr, err := p.d.processor(ctx, project, processor)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}
	}
	doc, meta, err := p.d.document(ctx, pr, id)
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}
	}
	ds, err := p.d.dataset(ctx, pr, "")
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}
	}
	kind := docAICustomKinds[pr.GetType()]
	view := docAILabelView(doc)
	text := view.Text
	view.Text = ""
	return console.Detail{Heading: "Label document", Title: meta.GetDisplayName(),
		Trail: []console.Crumb{{Label: processor, Path: []string{processor}}, {Label: meta.GetDisplayName()}},
		Summary: []console.Property{{Label: "Split", Value: splitLabel(meta.GetDatasetType().String())},
			{Label: "Labeling state", Value: docAILabelState(meta.GetLabelingState().String())},
			{Label: "Pages", Value: strconv.Itoa(len(doc.GetPages()))}, {Label: "Document ID", Value: id}},
		Sections: []console.Section{
			{ID: "label", Label: "Labels", Kind: console.KindResult, Listing: *view, LabelAction: "add-label",
				Actions: docAILabelActions(kind, ds.schema, doc)},
			{ID: "text", Label: "Text", Kind: console.KindText, Text: text},
		}}
}

func docAILabelActions(kind string, sc *documentaipb.DocumentSchema, doc *documentaipb.Document) []console.Action {
	types := docAILabelTypes(kind, sc)
	def := ""
	if len(types) > 0 {
		def = types[0]
	}
	add := []console.Field{{Name: "type", Label: "Field", Type: "select", Options: types, Default: def, Required: true,
		Help: "the schema's field the value is"}}
	switch kind {
	case "extractor":
		add = append(add, console.Field{Name: "value", Label: "Value", Type: "text",
			Help: "the value as the text has it; empty when a box was drawn, whose words are the value"},
			console.Field{Name: "page", Label: "Page", Type: "number", Default: "1", Section: "Bounding box"},
			console.Field{Name: "x", Label: "Left", Type: "text", Section: "Bounding box", Pattern: `^(0(\.\d+)?|1(\.0+)?)?$`,
				Help: "fractions of the page's width and height, filled in when you draw a box"},
			console.Field{Name: "y", Label: "Top", Type: "text", Section: "Bounding box", Pattern: `^(0(\.\d+)?|1(\.0+)?)?$`},
			console.Field{Name: "w", Label: "Width", Type: "text", Section: "Bounding box", Pattern: `^(0(\.\d+)?|1(\.0+)?)?$`},
			console.Field{Name: "h", Label: "Height", Type: "text", Section: "Bounding box", Pattern: `^(0(\.\d+)?|1(\.0+)?)?$`})
	case "splitter":
		add = append(add, console.Field{Name: "pages", Label: "Pages", Type: "text", Required: true, Pattern: `^\s*\d+(\s*-\s*\d+)?\s*$`,
			Help: "the subdocument's pages, such as 1-2"})
	}
	addLabel := console.Action{ID: "add-label", Label: "Add label", Submit: "Label", Fields: add}
	if kind == "classifier" {
		addLabel.Label, addLabel.Submit = "Set document type", "Save"
	}
	var rows []string
	rowLabels := map[string]string{}
	for i, e := range doc.GetEntities() {
		n := strconv.Itoa(i + 1)
		rows = append(rows, n)
		rowLabels[n] = n + ": " + e.GetType() + " " + entityText(doc, e)
	}
	acts := []console.Action{addLabel}
	if len(rows) > 0 {
		acts = append(acts, console.Action{ID: "remove-label", Label: "Remove label", Destructive: true, Submit: "Remove", Fields: []console.Field{
			{Name: "label", Label: "Label", Type: "select", Options: rows, OptionLabels: rowLabels, Default: rows[0], Required: true}}})
	}
	return append(acts, console.Action{ID: "set-split", Label: "Assign to split", Submit: "Save", Fields: []console.Field{
		{Name: "split", Label: "Data split", Type: "select", Options: []string{"DATASET_SPLIT_TRAIN", "DATASET_SPLIT_TEST", "DATASET_SPLIT_UNASSIGNED"},
			OptionLabels: docAISplits, Default: "DATASET_SPLIT_TRAIN"}}})
}

// labelFromBox is the entity the words inside a box drawn over a page are.
func labelFromBox(doc *documentaipb.Document, typ string, page int, x, y, w, h float64) (*documentaipb.Document_Entity, error) {
	if page < 1 || page > len(doc.GetPages()) {
		return nil, fmt.Errorf("the document has no page %d", page)
	}
	pg := doc.GetPages()[page-1]
	text := []rune(doc.GetText())
	type seg struct{ s, e int64 }
	var segs []seg
	minX, minY, maxX, maxY := 1.0, 1.0, 0.0, 0.0
	for _, t := range pg.GetTokens() {
		v := t.GetLayout().GetBoundingPoly().GetNormalizedVertices()
		if len(v) < 3 {
			continue
		}
		cx, cy := float64(v[0].GetX()+v[2].GetX())/2, float64(v[0].GetY()+v[2].GetY())/2
		if cx < x || cx > x+w || cy < y || cy > y+h {
			continue
		}
		for _, s := range t.GetLayout().GetTextAnchor().GetTextSegments() {
			segs = append(segs, seg{s.GetStartIndex(), s.GetEndIndex()})
		}
		minX, minY = min(minX, float64(v[0].GetX())), min(minY, float64(v[0].GetY()))
		maxX, maxY = max(maxX, float64(v[2].GetX())), max(maxY, float64(v[2].GetY()))
	}
	if len(segs) == 0 {
		return nil, errors.New("the box holds no words: draw it around the value's words")
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].s < segs[j].s })
	st, en := segs[0].s, segs[len(segs)-1].e
	if st < 0 || en > int64(len(text)) || st >= en {
		return nil, errors.New("the words in the box have no text")
	}
	val := strings.TrimSpace(string(text[st:en]))
	for en > st && (text[en-1] == '\n' || text[en-1] == ' ') {
		en--
	}
	return &documentaipb.Document_Entity{Type: typ, MentionText: val, Confidence: 1,
		TextAnchor: &documentaipb.Document_TextAnchor{TextSegments: []*documentaipb.Document_TextAnchor_TextSegment{{StartIndex: st, EndIndex: en}}},
		PageAnchor: &documentaipb.Document_PageAnchor{PageRefs: []*documentaipb.Document_PageAnchor_PageRef{{Page: int64(page - 1),
			BoundingPoly: &documentaipb.BoundingPoly{NormalizedVertices: []*documentaipb.NormalizedVertex{
				{X: float32(minX), Y: float32(minY)}, {X: float32(maxX), Y: float32(minY)}, {X: float32(maxX), Y: float32(maxY)}, {X: float32(minX), Y: float32(maxY)}}}}}}}, nil
}

// labelFromText is the entity of value's first occurrence in the text,
// case ignored.
func labelFromText(doc *documentaipb.Document, typ, value string) (*documentaipb.Document_Entity, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("give the value, or draw a box around it on a page")
	}
	text := []rune(doc.GetText())
	low, v := []rune(strings.ToLower(doc.GetText())), []rune(strings.ToLower(value))
	at := -1
	if len(low) == len(text) {
		for i := 0; i+len(v) <= len(low); i++ {
			if string(low[i:i+len(v)]) == string(v) {
				at = i
				break
			}
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("%q is not in the document's text", value)
	}
	e := &documentaipb.Document_Entity{Type: typ, MentionText: string(text[at : at+len(v)]), Confidence: 1,
		TextAnchor: &documentaipb.Document_TextAnchor{TextSegments: []*documentaipb.Document_TextAnchor_TextSegment{{StartIndex: int64(at), EndIndex: int64(at + len(v))}}}}
	for i, pg := range doc.GetPages() {
		for _, s := range pg.GetLayout().GetTextAnchor().GetTextSegments() {
			if int64(at) >= s.GetStartIndex() && int64(at) < s.GetEndIndex() {
				e.PageAnchor = &documentaipb.Document_PageAnchor{PageRefs: []*documentaipb.Document_PageAnchor_PageRef{{Page: int64(i)}}}
			}
		}
	}
	return e, nil
}

func parsePageRange(s string, n int) ([]int64, error) {
	a, b, ok := strings.Cut(strings.ReplaceAll(s, " ", ""), "-")
	from, err := strconv.Atoi(a)
	if err != nil {
		return nil, fmt.Errorf("pages are a number or a range such as 1-2, not %q", s)
	}
	to := from
	if ok {
		if to, err = strconv.Atoi(b); err != nil {
			return nil, fmt.Errorf("pages are a number or a range such as 1-2, not %q", s)
		}
	}
	if from < 1 || to < from || (n > 0 && to > n) {
		return nil, fmt.Errorf("pages %s are not pages of this %d-page document", s, n)
	}
	var out []int64
	for p := from; p <= to; p++ {
		out = append(out, int64(p-1))
	}
	return out, nil
}

// labelAct is a labeling page's action.
func (p docAIProcessorsProvider) labelAct(ctx context.Context, pr *documentaipb.Processor, id, action string, v map[string]string) error {
	if p.labels == nil {
		return errors.New("this console cannot save labels: the local AI endpoint does not run in this process")
	}
	doc, _, err := p.d.document(ctx, pr, id)
	if err != nil {
		return err
	}
	kind := docAICustomKinds[pr.GetType()]
	ents := doc.GetEntities()
	switch action {
	case "set-split":
		return p.labels.LabelDatasetDocument(ctx, pr.GetName(), id, nil, v["split"])
	case "remove-label":
		n, err := strconv.Atoi(v["label"])
		if err != nil || n < 1 || n > len(ents) {
			return fmt.Errorf("the document has no label %q", v["label"])
		}
		ents = append(append([]*documentaipb.Document_Entity{}, ents[:n-1]...), ents[n:]...)
		if ents == nil {
			ents = []*documentaipb.Document_Entity{}
		}
		return p.labels.LabelDatasetDocument(ctx, pr.GetName(), id, ents, "")
	case "add-label":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	typ := strings.TrimSpace(v["type"])
	if typ == "" {
		return errors.New("choose the field")
	}
	var e *documentaipb.Document_Entity
	switch kind {
	case "classifier":
		// A document has one class.
		return p.labels.LabelDatasetDocument(ctx, pr.GetName(), id, []*documentaipb.Document_Entity{{Type: typ, Confidence: 1}}, "")
	case "splitter":
		pages, err := parsePageRange(v["pages"], len(doc.GetPages()))
		if err != nil {
			return err
		}
		e = &documentaipb.Document_Entity{Type: typ, Confidence: 1, PageAnchor: &documentaipb.Document_PageAnchor{}}
		for _, pg := range pages {
			e.PageAnchor.PageRefs = append(e.PageAnchor.PageRefs, &documentaipb.Document_PageAnchor_PageRef{Page: pg})
		}
	default:
		if strings.TrimSpace(v["w"]) != "" && strings.TrimSpace(v["value"]) == "" {
			f := func(k string) float64 { x, _ := strconv.ParseFloat(strings.TrimSpace(v[k]), 64); return x }
			page, _ := strconv.Atoi(strings.TrimSpace(v["page"]))
			if e, err = labelFromBox(doc, typ, page, f("x"), f("y"), f("w"), f("h")); err != nil {
				return err
			}
		} else if e, err = labelFromText(doc, typ, v["value"]); err != nil {
			return err
		}
	}
	return p.labels.LabelDatasetDocument(ctx, pr.GetName(), id, append(append([]*documentaipb.Document_Entity{}, ents...), e), "")
}

// --- Build, Get started and Evaluate actions ----------------------------------

// customAct runs a custom processor's tab action; ok is false for an
// action that is not one.
func (p docAIProcessorsProvider) customAct(ctx context.Context, project string, pr *documentaipb.Processor, action string,
	v map[string]string) (*console.Listing, bool, error) {
	ds := pr.GetName() + "/dataset"
	kind := docAICustomKinds[pr.GetType()]
	switch action {
	case "configure-dataset":
		d := &beta.Dataset{Name: ds, StorageSource: &beta.Dataset_UnmanagedDatasetConfig_{UnmanagedDatasetConfig: &beta.Dataset_UnmanagedDatasetConfig{}}}
		if v["storage"] != "unmanaged" {
			prefix := strings.TrimSpace(v["gcsPrefix"])
			if prefix == "" {
				return nil, true, errors.New("give the Cloud Storage folder, gs://bucket/folder")
			}
			d.StorageSource = &beta.Dataset_GcsManagedConfig{GcsManagedConfig: &beta.Dataset_GCSManagedConfig{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: prefix}}}
		}
		return nil, true, p.d.withDataset(ctx, func(c *documentaibeta.DocumentClient) error {
			op, err := c.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: d})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	case "import-documents":
		return p.importDocuments(ctx, ds, v)
	case "filter-documents":
		return p.filterDocuments(ctx, project, pr, v)
	case "assign-train", "assign-test", "assign-unassigned":
		if p.labels == nil {
			return nil, true, errors.New("this console cannot change splits: the local AI endpoint does not run in this process")
		}
		split := map[string]string{"assign-train": "DATASET_SPLIT_TRAIN", "assign-test": "DATASET_SPLIT_TEST",
			"assign-unassigned": "DATASET_SPLIT_UNASSIGNED"}[action]
		ids, err := p.docIDs(ctx, pr, v[docAIVersionsField])
		if err != nil {
			return nil, true, err
		}
		for _, id := range ids {
			if err := p.labels.LabelDatasetDocument(ctx, pr.GetName(), id, nil, split); err != nil {
				return nil, true, err
			}
		}
		return nil, true, nil
	case "delete-documents":
		names, err := p.docIDs(ctx, pr, v[docAIVersionsField])
		if err != nil {
			return nil, true, err
		}
		var ids []*beta.DocumentId
		for _, id := range names {
			ids = append(ids, &beta.DocumentId{Type: &beta.DocumentId_GcsManagedDocId{GcsManagedDocId: &beta.DocumentId_GCSManagedDocumentId{CwDocId: id}}})
		}
		return nil, true, p.d.withDataset(ctx, func(c *documentaibeta.DocumentClient) error {
			op, err := c.BatchDeleteDocuments(ctx, &beta.BatchDeleteDocumentsRequest{Dataset: ds, DatasetDocuments: &beta.BatchDatasetDocuments{
				Criteria: &beta.BatchDatasetDocuments_IndividualDocumentIds_{IndividualDocumentIds: &beta.BatchDatasetDocuments_IndividualDocumentIds{DocumentIds: ids}}}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
	case "add-field", "delete-fields", "edit-prompt":
		return nil, true, p.changeSchema(ctx, ds, kind, action, v)
	case "train-version":
		return p.trainVersion(ctx, project, pr, v)
	case "evaluate-version":
		return p.evaluateVersion(ctx, project, pr, v)
	}
	return nil, false, nil
}

func (p docAIProcessorsProvider) importDocuments(ctx context.Context, ds string, v map[string]string) (*console.Listing, bool, error) {
	src := strings.TrimSpace(v["source"])
	in := &beta.BatchDocumentsInputConfig{Source: &beta.BatchDocumentsInputConfig_GcsPrefix{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: src}}}
	if !strings.HasSuffix(src, "/") && strings.Contains(strings.TrimPrefix(src, "gs://"), "/") && strings.Contains(lastSegment(src), ".") {
		in = &beta.BatchDocumentsInputConfig{Source: &beta.BatchDocumentsInputConfig_GcsDocuments{GcsDocuments: &beta.GcsDocuments{
			Documents: []*beta.GcsDocument{{GcsUri: src, MimeType: docAIImportMime(src)}}}}}
	}
	cfg := &beta.ImportDocumentsRequest_BatchDocumentsImportConfig{BatchInputConfig: in, DocumentType: v["documentType"]}
	if v["split"] == "auto" {
		cfg.SplitTypeConfig = &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_AutoSplitConfig_{
			AutoSplitConfig: &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_AutoSplitConfig{TrainingSplitRatio: 0.8}}
	} else {
		cfg.SplitTypeConfig = &beta.ImportDocumentsRequest_BatchDocumentsImportConfig_DatasetSplit{
			DatasetSplit: beta.DatasetSplitType(beta.DatasetSplitType_value[v["split"]])}
	}
	out := &console.Listing{Columns: []string{"Status", "Document ID"}, NameColumn: "Source", Noun: "documents"}
	err := p.d.withDataset(ctx, func(c *documentaibeta.DocumentClient) error {
		op, err := c.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: ds,
			BatchDocumentsImportConfigs: []*beta.ImportDocumentsRequest_BatchDocumentsImportConfig{cfg}})
		if err != nil {
			return err
		}
		_, werr := op.Wait(ctx)
		meta, _ := op.Metadata()
		for _, st := range meta.GetIndividualImportStatuses() {
			state := "Imported"
			if st.GetStatus().GetCode() != 0 {
				state = "Failed: " + st.GetStatus().GetMessage()
			}
			out.Items = append(out.Items, console.Resource{Name: st.GetInputGcsSource(), Fields: map[string]string{"Status": state,
				"Document ID": st.GetOutputDocumentId().GetGcsManagedDocId().GetCwDocId()}})
		}
		if werr != nil && len(out.Items) == 0 {
			return werr
		}
		return nil
	})
	out.Total = len(out.Items)
	return out, true, err
}

func docAIImportMime(uri string) string {
	if strings.HasSuffix(strings.ToLower(uri), ".json") {
		return "application/json"
	}
	i := strings.LastIndex(uri, ".")
	return docAIMimeTypes[strings.ToLower(uri[i:])]
}

func (p docAIProcessorsProvider) filterDocuments(ctx context.Context, project string, pr *documentaipb.Processor, v map[string]string) (*console.Listing, bool, error) {
	var terms []string
	if s := v["split"]; s != "" {
		terms = append(terms, "SplitType="+s)
	}
	if s := v["state"]; s != "" {
		terms = append(terms, "LabelingState="+s)
	}
	if s := v["label"]; s != "" {
		terms = append(terms, "EntityType="+s)
	}
	if s := strings.TrimSpace(v["name"]); s != "" {
		terms = append(terms, `DisplayName="`+strings.ReplaceAll(s, `"`, "")+`"`)
	}
	ds, err := p.d.dataset(ctx, pr, strings.Join(terms, " AND "))
	if err != nil {
		return nil, true, err
	}
	q := "?project=" + url.QueryEscape(p.d.project(project))
	base := "/document-ai/processors/" + url.PathEscape(lastSegment(pr.GetName()))
	out := &console.Listing{Columns: []string{"Split", "Labeling state", "Pages"}, NameColumn: "Document", Noun: "documents"}
	for _, m := range ds.docs {
		id := m.GetDocumentId().GetGcsManagedDocId().GetCwDocId()
		out.Items = append(out.Items, console.Resource{Name: m.GetDisplayName(), Link: base + "/" + docAIDocumentSegment + "/" + url.PathEscape(id) + q,
			Fields: map[string]string{"Split": splitLabel(m.GetDatasetType().String()), "Labeling state": docAILabelState(m.GetLabelingState().String()),
				"Pages": strconv.Itoa(int(m.GetPageCount()))}})
	}
	out.Total = len(out.Items)
	out.Note = "Filter: " + strings.Join(terms, " AND ")
	if len(terms) == 0 {
		out.Note = "Every document."
	}
	return out, true, nil
}

func (p docAIProcessorsProvider) changeSchema(ctx context.Context, ds, kind, action string, v map[string]string) error {
	return p.d.withDataset(ctx, func(c *documentaibeta.DocumentClient) error {
		cur, err := c.GetDatasetSchema(ctx, &beta.GetDatasetSchemaRequest{Name: ds + "/datasetSchema"})
		if err != nil {
			return err
		}
		sc := cur.GetDocumentSchema()
		if sc == nil {
			sc = &beta.DocumentSchema{}
		}
		switch action {
		case "edit-prompt":
			sc.Description = strings.TrimSpace(v["prompt"])
			if len([]rune(sc.Description)) > 500 {
				return errors.New("the document prompt is at most 500 characters")
			}
		case "add-field":
			name := strings.TrimSpace(v["name"])
			if name == "" {
				return errors.New("give the field a name")
			}
			for _, f := range docAIFields(kind, toV1(sc, &documentaipb.DocumentSchema{})) {
				if f.Name == name {
					return fmt.Errorf("the schema already has %q", name)
				}
			}
			if kind != "extractor" {
				sc.EntityTypes = append(sc.EntityTypes, &beta.DocumentSchema_EntityType{Name: name, BaseTypes: []string{"document"}})
				break
			}
			var holder *beta.DocumentSchema_EntityType
			for _, et := range sc.GetEntityTypes() {
				if len(et.GetProperties()) > 0 || slicesContains(et.GetBaseTypes(), "document") {
					holder = et
					break
				}
			}
			if holder == nil {
				holder = &beta.DocumentSchema_EntityType{Name: "custom_extraction_document_type", BaseTypes: []string{"document"}}
				sc.EntityTypes = append(sc.EntityTypes, holder)
			}
			occ := beta.DocumentSchema_EntityType_Property_OccurrenceType(beta.DocumentSchema_EntityType_Property_OccurrenceType_value[v["occurrence"]])
			holder.Properties = append(holder.Properties, &beta.DocumentSchema_EntityType_Property{Name: name, ValueType: v["valueType"],
				OccurrenceType: occ, DisplayName: strings.TrimSpace(v["description"])})
		case "delete-fields":
			gone := map[string]bool{}
			for _, n := range strings.Fields(v[docAIVersionsField]) {
				gone[n] = true
			}
			if len(gone) == 0 {
				return errors.New("check the fields to delete")
			}
			var keep []*beta.DocumentSchema_EntityType
			for _, et := range sc.GetEntityTypes() {
				if kind != "extractor" && len(et.GetProperties()) == 0 && gone[et.GetName()] {
					continue
				}
				var props []*beta.DocumentSchema_EntityType_Property
				for _, pp := range et.GetProperties() {
					if !gone[pp.GetName()] {
						props = append(props, pp)
					}
				}
				et.Properties = props
				keep = append(keep, et)
			}
			sc.EntityTypes = keep
		}
		_, err = c.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: ds + "/datasetSchema", DocumentSchema: sc}})
		return err
	})
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (p docAIProcessorsProvider) trainVersion(ctx context.Context, project string, pr *documentaipb.Processor, v map[string]string) (*console.Listing, bool, error) {
	req := &documentaipb.TrainProcessorVersionRequest{Parent: pr.GetName(),
		ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: strings.TrimSpace(v["name"])}}
	if b := strings.TrimSpace(v["base"]); b != "" {
		req.BaseProcessorVersion = pr.GetName() + "/processorVersions/" + b
	}
	var pv *documentaipb.ProcessorVersion
	err := p.d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		op, err := c.TrainProcessorVersion(ctx, req)
		if err != nil {
			return err
		}
		r, err := op.Wait(ctx)
		if err != nil {
			return err
		}
		pv, err = c.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: r.GetProcessorVersion()})
		return err
	})
	if err != nil {
		return nil, true, err
	}
	m := pv.GetLatestEvaluation().GetAggregateMetrics()
	model := "Counting model (no local generation model is installed)"
	if pv.GetModelType() == documentaipb.ProcessorVersion_MODEL_TYPE_GENERATIVE {
		model = "Generative (the local model)"
	}
	out := &console.Listing{Columns: []string{"Name", "State", "Model", "F1 score", "Precision", "Recall"}, NameColumn: "Version ID",
		Items: []console.Resource{{Name: lastSegment(pv.GetName()), Fields: map[string]string{"Name": pv.GetDisplayName(),
			"State": stateWord(pv.GetState().String()), "Model": model, "F1 score": pct(m.GetF1Score()), "Precision": pct(m.GetPrecision()),
			"Recall": pct(m.GetRecall())}}}, Total: 1,
		Note: "The version is undeployed: deploy it in Deploy & use, then set it as the default."}
	if ev := pv.GetLatestEvaluation().GetEvaluation(); ev != "" {
		out.Items[0].Link = docAIEvaluationHref(p.d.project(project), lastSegment(pr.GetName()), lastSegment(pv.GetName()), lastSegment(ev), "")
	} else {
		out.Note = "The version is undeployed, and not evaluated: the dataset has no labeled test documents."
	}
	return out, true, nil
}

func (p docAIProcessorsProvider) evaluateVersion(ctx context.Context, project string, pr *documentaipb.Processor, v map[string]string) (*console.Listing, bool, error) {
	id := strings.TrimSpace(v["version"])
	if id == "" {
		return nil, true, errors.New("choose a version")
	}
	req := &documentaipb.EvaluateProcessorVersionRequest{ProcessorVersion: pr.GetName() + "/processorVersions/" + id}
	if docs := strings.TrimSpace(v["documents"]); docs != "" {
		req.EvaluationDocuments = &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsPrefix{
			GcsPrefix: &documentaipb.GcsPrefix{GcsUriPrefix: docs}}}
	}
	var ev string
	err := p.d.withClient(ctx, func(c *documentai.DocumentProcessorClient) error {
		op, err := c.EvaluateProcessorVersion(ctx, req)
		if err != nil {
			return err
		}
		r, err := op.Wait(ctx)
		ev = r.GetEvaluation()
		return err
	})
	if err != nil {
		return nil, true, err
	}
	return &console.Listing{Open: docAIEvaluationHref(p.d.project(project), lastSegment(pr.GetName()), id, lastSegment(ev), "")}, true, nil
}

// customDetail answers a custom processor's deeper pages: a document's
// labeling page and an evaluation's.
func (p docAIProcessorsProvider) customDetail(ctx context.Context, project string, path []string) (console.Detail, bool) {
	switch {
	case len(path) == 3 && path[1] == docAIDocumentSegment:
		return p.documentDetail(ctx, project, path[0], path[2]), true
	case (len(path) == 4 || len(path) == 5) && path[1] == docAIEvaluationSegment:
		return p.evaluationDetail(ctx, project, path[0], path[2:]), true
	}
	return console.Detail{}, false
}

// customPathActions are the actions of a custom processor's deeper pages.
func (p docAIProcessorsProvider) customPathActions(ctx context.Context, project string, path []string) []console.Action {
	d, ok := p.customDetail(ctx, project, path)
	if !ok {
		return nil
	}
	var out []console.Action
	for _, s := range d.Sections {
		out = append(out, s.Actions...)
	}
	return out
}

// customPathAct runs a deeper page's action.
func (p docAIProcessorsProvider) customPathAct(ctx context.Context, project string, path []string, action string, v map[string]string) (*console.Listing, bool, error) {
	if len(path) < 3 {
		return nil, false, nil
	}
	pr, err := p.d.processor(ctx, project, path[0])
	if err != nil {
		return nil, true, err
	}
	switch {
	case len(path) == 3 && path[1] == docAIDocumentSegment:
		return nil, true, p.labelAct(ctx, pr, path[2], action, v)
	case (len(path) == 4 || len(path) == 5) && path[1] == docAIEvaluationSegment && action == "set-threshold":
		return &console.Listing{Open: docAIEvaluationHref(p.d.project(project), path[0], path[2], path[3], strings.TrimSpace(v["threshold"]))}, true, nil
	}
	return nil, false, nil
}

// docIDs are the IDs of the checked documents, named one per line by
// their display names (or IDs).
func (p docAIProcessorsProvider) docIDs(ctx context.Context, pr *documentaipb.Processor, checked string) ([]string, error) {
	ds, err := p.d.dataset(ctx, pr, "")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range strings.Split(checked, "\n") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		for _, m := range ds.docs {
			if id := m.GetDocumentId().GetGcsManagedDocId().GetCwDocId(); m.GetDisplayName() == n || id == n {
				out = append(out, id)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("check the documents")
	}
	return out, nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
