package vertexai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Document AI's custom processors (#2106): Custom Extractor
// (CUSTOM_EXTRACTION_PROCESSOR), Custom Classifier
// (CUSTOM_CLASSIFICATION_PROCESSOR) and Custom Splitter
// (CUSTOM_SPLITTING_PROCESSOR), as Google's Workbench pages document the
// workflow (https://docs.cloud.google.com/document-ai/docs/workbench/build-custom-processor,
// .../custom-classifier, .../evaluate, read 2026-11-21):
//
//   - A custom processor is created with no version: until a version is
//     trained and deployed, ProcessDocument is FAILED_PRECONDITION.
//   - Its dataset (the v1beta3 DocumentService, documentaidataset.go) holds
//     the labeled documents, each in the training or test split, and the
//     document schema.
//   - TrainProcessorVersion trains a version from the dataset's labeled
//     documents (or input_data's training_documents and test_documents,
//     Documents in JSON in Cloud Storage) with the request's
//     document_schema (else the dataset's): CloudBurrow's own local models
//     (documentaimodels.go), on this machine. base_processor_version
//     uptrains: the new version starts from the base version's counts. A
//     classifier or splitter needs at least two document types, each with
//     a training and a test document ("a minimum of one document in both
//     the training and test sets for each document type"); an extractor at
//     least one labeled training document. "An evaluation is automatically
//     run whenever you train or uptrain a processor version": against the
//     test documents, when there are any. The new version is UNDEPLOYED.
//   - DeployProcessorVersion and UndeployProcessorVersion (operations) set
//     its state; ProcessDocument with an undeployed version is
//     FAILED_PRECONDITION. SetDefaultProcessorVersion takes a deployed
//     version. DeleteProcessorVersion refuses the default version and a
//     deployed one.
//   - ProcessDocument reads the pages with the OCR processor's Tesseract
//     and adds the version's entities: the extractor's values (type,
//     mention_text, text_anchor, confidence), the classifier's classes
//     (an entity a class, its confidence the posterior, most probable
//     first), the splitter's subdocuments (type, confidence and
//     page_anchor, pages 0-based).
//   - EvaluateProcessorVersion scores a version on evaluation_documents or
//     the dataset's test split, with the evaluate page's definitions:
//     precision TP/(TP+FP), recall TP/(TP+FN), F1 their harmonic mean, at
//     confidence levels 0 to 1 in steps of 0.05, fuzzy
//     (confidence_level_metrics: case and whitespace ignored) and exact
//     (confidence_level_metrics_exact), for all entities and each type,
//     with auprc and estimated_calibration_error. The version's
//     latest_evaluation names it, with the aggregate metrics at the
//     confidence level of the best F1 ("the optimal threshold, which
//     maximizes the F1 score").
//
// UNVERIFIED (re-read 2026-11-21: the pages cited, which do not say): the
// state of a version after training, the codes and texts of the refusals,
// the confidence levels Google reports, how a splitter's and a classifier's
// entities are matched in an evaluation (here: the class, and for a
// splitter its type and pages), and that foundation_model_tuning_options
// is refused (CloudBurrow has no foundation model to tune).

// docAICustomTypes are the trainable types and their model kinds.
var docAICustomTypes = map[string]string{
	"CUSTOM_EXTRACTION_PROCESSOR":     "extractor",
	"CUSTOM_CLASSIFICATION_PROCESSOR": "classifier",
	"CUSTOM_SPLITTING_PROCESSOR":      "splitter",
}

// customProcessor is a custom processor's dataset, schema and versions.
type customProcessor struct {
	// DatasetState is the dataset's state and GcsPrefix its managed
	// storage ("" when unmanaged).
	DatasetState string                       `json:"datasetState,omitempty"`
	GcsPrefix    string                       `json:"gcsPrefix,omitempty"`
	Unmanaged    bool                         `json:"unmanaged,omitempty"`
	Schema       *documentaipb.DocumentSchema `json:"-"`
	Docs         []*datasetDoc                `json:"docs,omitempty"`
	Versions     []*customVersion             `json:"versions,omitempty"`
}

// datasetDoc is one document in a dataset.
type datasetDoc struct {
	ID          string                 `json:"id"`
	DisplayName string                 `json:"displayName"`
	Split       string                 `json:"split"`
	GcsURI      string                 `json:"gcsUri,omitempty"`
	Doc         *documentaipb.Document `json:"-"`
}

func (d *datasetDoc) labeled() bool { return len(d.Doc.GetEntities()) > 0 }

// customVersion is a trained version.
type customVersion struct {
	Proto       *documentaipb.ProcessorVersion `json:"-"`
	Model       *docModel                      `json:"model"`
	Evaluations []*documentaipb.Evaluation     `json:"-"`
}

// The persisted form: protos as protojson.
type customProcessorJSON struct {
	customProcessor
	SchemaJSON json.RawMessage     `json:"schema,omitempty"`
	DocsJSON   []json.RawMessage   `json:"docJSON,omitempty"`
	Versions   []customVersionJSON `json:"versions,omitempty"`
}

type customVersionJSON struct {
	Proto       json.RawMessage   `json:"proto"`
	Model       *docModel         `json:"model"`
	Evaluations []json.RawMessage `json:"evaluations,omitempty"`
}

func pj(m proto.Message) json.RawMessage {
	if m == nil {
		return nil
	}
	b, _ := protojson.Marshal(m)
	return b
}

var loose = protojson.UnmarshalOptions{DiscardUnknown: true}

// customFile is where the custom processors are kept.
func (d *docAIStore) customFile() string {
	if d.file == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.file), "custom.json")
}

func (d *docAIStore) saveCustom() {
	f := d.customFile()
	if f == "" {
		return
	}
	out := map[string]customProcessorJSON{}
	for name, c := range d.custom {
		j := customProcessorJSON{customProcessor: *c, SchemaJSON: pj(c.Schema)}
		j.customProcessor.Versions = nil
		for _, doc := range c.Docs {
			j.DocsJSON = append(j.DocsJSON, pj(doc.Doc))
		}
		for _, v := range c.Versions {
			vj := customVersionJSON{Proto: pj(v.Proto), Model: v.Model}
			for _, e := range v.Evaluations {
				vj.Evaluations = append(vj.Evaluations, pj(e))
			}
			j.Versions = append(j.Versions, vj)
		}
		out[name] = j
	}
	b, _ := json.Marshal(out)
	tmp := f + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, f)
	}
}

func (d *docAIStore) loadCustom() {
	d.custom = map[string]*customProcessor{}
	f := d.customFile()
	if f == "" {
		return
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	var in map[string]customProcessorJSON
	if json.Unmarshal(b, &in) != nil {
		return
	}
	for name, j := range in {
		c := j.customProcessor
		c.Versions = nil
		if len(j.SchemaJSON) > 0 {
			c.Schema = &documentaipb.DocumentSchema{}
			_ = loose.Unmarshal(j.SchemaJSON, c.Schema)
		}
		for i, doc := range c.Docs {
			doc.Doc = &documentaipb.Document{}
			if i < len(j.DocsJSON) {
				_ = loose.Unmarshal(j.DocsJSON[i], doc.Doc)
			}
		}
		for _, vj := range j.Versions {
			v := &customVersion{Proto: &documentaipb.ProcessorVersion{}, Model: vj.Model}
			_ = loose.Unmarshal(vj.Proto, v.Proto)
			for _, e := range vj.Evaluations {
				ev := &documentaipb.Evaluation{}
				_ = loose.Unmarshal(e, ev)
				v.Evaluations = append(v.Evaluations, ev)
			}
			c.Versions = append(c.Versions, v)
		}
		d.custom[name] = &c
	}
}

// customOf is processor name's custom state, made when absent; the caller
// holds s.mu.
func (d *docAIStore) customOf(name string) *customProcessor {
	if d.custom == nil {
		d.custom = map[string]*customProcessor{}
	}
	c := d.custom[name]
	if c == nil {
		c = &customProcessor{DatasetState: "UNINITIALIZED"}
		d.custom[name] = c
	}
	return c
}

func (c *customProcessor) version(name string) *customVersion {
	for _, v := range c.Versions {
		if v.Proto.GetName() == name {
			return v
		}
	}
	return nil
}

func isCustomType(t string) bool { return docAICustomTypes[t] != "" }

func cloneVersion(v *documentaipb.ProcessorVersion) *documentaipb.ProcessorVersion {
	return proto.Clone(v).(*documentaipb.ProcessorVersion)
}

func newHexID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// customVersionProto looks a custom processor's version up.
func (s *Server) customVersionProto(name string) (*documentaipb.ProcessorVersion, error) {
	pname, _, _ := strings.Cut(name, "/processorVersions/")
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.docAI().customOf(pname).version(name)
	if v == nil {
		return nil, apierror.NotFound("Processor version %s not found", name)
	}
	return cloneVersion(v.Proto), nil
}

// customReady checks that p's version (its default, or the one the
// request named) is deployed.
func (s *Server) customReady(p *documentaipb.Processor) error {
	if p.GetDefaultProcessorVersion() == "" {
		return apierror.FailedPrecondition("Processor %s has no processor version: train and deploy a version, then set it as the default or name it", p.GetName())
	}
	v, err := s.customVersionProto(p.GetDefaultProcessorVersion())
	if err != nil {
		return err
	}
	if v.GetState() != documentaipb.ProcessorVersion_DEPLOYED {
		return apierror.FailedPrecondition("Processor version %s is not deployed: its state is %s", v.GetName(), v.GetState())
	}
	return nil
}

// applyCustom adds p's version's entities to the OCR'd doc.
func (s *Server) applyCustom(p *documentaipb.Processor, doc *documentaipb.Document) error {
	pname := p.GetName()
	s.mu.Lock()
	v := s.docAI().customOf(pname).version(p.GetDefaultProcessorVersion())
	var m *docModel
	var schema *documentaipb.DocumentSchema
	if v != nil {
		m, schema = v.Model, proto.Clone(v.Proto.GetDocumentSchema()).(*documentaipb.DocumentSchema)
	}
	s.mu.Unlock()
	if m == nil {
		return apierror.NotFound("Processor version %s not found", p.GetDefaultProcessorVersion())
	}
	doc.Entities = append(doc.Entities, m.predictEntities(doc, schema)...)
	return nil
}

// --- versions -------------------------------------------------------------

func (d *documentAIService) customVersionOp(name string, change func(c *customProcessor, v *customVersion, p *documentaipb.Processor) error,
	meta func(*documentaipb.CommonOperationMetadata) any, resp any) (*longrunningpb.Operation, error) {
	pname, _, _ := strings.Cut(name, "/processorVersions/")
	return d.processorOp(pname, func(st *docAIStore, p *documentaipb.Processor) error {
		c := st.customOf(pname)
		v := c.version(name)
		if v == nil {
			return apierror.NotFound("Processor version %s not found", name)
		}
		if err := change(c, v, p); err != nil {
			return err
		}
		st.saveCustom()
		return nil
	}, meta, resp)
}

func (d *documentAIService) deployCustom(name string, deploy bool) (*longrunningpb.Operation, error) {
	return d.customVersionOp(name, func(_ *customProcessor, v *customVersion, p *documentaipb.Processor) error {
		if !deploy && p.GetDefaultProcessorVersion() == name {
			return apierror.FailedPrecondition("Cannot undeploy processor version %s: it is the processor's default version", name)
		}
		if deploy {
			v.Proto.State = documentaipb.ProcessorVersion_DEPLOYED
		} else {
			v.Proto.State = documentaipb.ProcessorVersion_UNDEPLOYED
		}
		return nil
	}, func(c *documentaipb.CommonOperationMetadata) any {
		c.Resource = name
		if deploy {
			return &documentaipb.DeployProcessorVersionMetadata{CommonMetadata: c}
		}
		return &documentaipb.UndeployProcessorVersionMetadata{CommonMetadata: c}
	}, func() any {
		if deploy {
			return &documentaipb.DeployProcessorVersionResponse{}
		}
		return &documentaipb.UndeployProcessorVersionResponse{}
	}())
}

func (d *documentAIService) deleteCustom(name string) (*longrunningpb.Operation, error) {
	return d.customVersionOp(name, func(c *customProcessor, v *customVersion, p *documentaipb.Processor) error {
		if p.GetDefaultProcessorVersion() == name {
			return apierror.FailedPrecondition("Cannot delete processor version %s: it is the processor's default version", name)
		}
		if v.Proto.GetState() == documentaipb.ProcessorVersion_DEPLOYED {
			return apierror.FailedPrecondition("Cannot delete processor version %s: it is deployed; undeploy it first", name)
		}
		for i, x := range c.Versions {
			if x == v {
				c.Versions = append(c.Versions[:i], c.Versions[i+1:]...)
				break
			}
		}
		return nil
	}, func(c *documentaipb.CommonOperationMetadata) any {
		c.Resource = name
		return &documentaipb.DeleteProcessorVersionMetadata{CommonMetadata: c}
	}, &emptypb.Empty{})
}

// --- training -------------------------------------------------------------

// trainingSet is a training run's documents.
type trainingSet struct {
	train, test []*documentaipb.Document
}

func (s *Server) inputDocuments(ctx context.Context, cfg *documentaipb.BatchDocumentsInputConfig) ([]*documentaipb.Document, error) {
	inputs, objs, err := s.listInputs(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var out []*documentaipb.Document
	for _, in := range inputs {
		bucket, object, _ := splitGCS(in.uri, false)
		b, _, err := objs.Read(ctx, bucket, object, docAIMaxBytes+1)
		if err != nil {
			return nil, err
		}
		doc := &documentaipb.Document{}
		if err := loose.Unmarshal(b, doc); err != nil {
			return nil, apierror.InvalidArgument("%s is not a Document in JSON: training documents are labeled Documents: %v", in.uri, err)
		}
		out = append(out, doc)
	}
	return out, nil
}

// listInputs is a BatchDocumentsInputConfig's objects.
func (s *Server) listInputs(ctx context.Context, cfg *documentaipb.BatchDocumentsInputConfig) ([]batchInput, Objects, error) {
	s.mu.RLock()
	objs := s.objects
	s.mu.RUnlock()
	if objs == nil {
		return nil, nil, apierror.FailedPrecondition("Documents in Cloud Storage need Cloud Storage, which this instance does not serve")
	}
	var inputs []batchInput
	switch {
	case cfg.GetGcsDocuments() != nil:
		for _, g := range cfg.GetGcsDocuments().GetDocuments() {
			if _, _, ok := splitGCS(g.GetGcsUri(), false); !ok {
				return nil, nil, apierror.InvalidArgument("Invalid gcs_uri %q", g.GetGcsUri())
			}
			inputs = append(inputs, batchInput{g.GetGcsUri(), g.GetMimeType()})
		}
	case cfg.GetGcsPrefix() != nil:
		bucket, prefix, ok := splitGCS(cfg.GetGcsPrefix().GetGcsUriPrefix(), true)
		if !ok {
			return nil, nil, apierror.InvalidArgument("Invalid gcs_uri_prefix %q", cfg.GetGcsPrefix().GetGcsUriPrefix())
		}
		lister, ok := objs.(ObjectLister)
		if !ok {
			return nil, nil, apierror.FailedPrecondition("gcs_prefix needs Cloud Storage listing, which this instance does not serve")
		}
		names, err := lister.List(ctx, bucket, prefix)
		if err != nil {
			return nil, nil, err
		}
		for _, n := range names {
			if !strings.HasSuffix(n, "/") {
				inputs = append(inputs, batchInput{uri: "gs://" + bucket + "/" + n})
			}
		}
	default:
		return nil, nil, apierror.InvalidArgument("Set gcs_documents or gcs_prefix")
	}
	if len(inputs) == 0 {
		return nil, nil, apierror.InvalidArgument("The input names no documents")
	}
	return inputs, objs, nil
}

// checkTrainingSet applies the documented minimums.
func checkTrainingSet(kind string, schema *documentaipb.DocumentSchema, set trainingSet) error {
	if kind == "extractor" {
		n := 0
		for _, d := range set.train {
			if len(d.GetEntities()) > 0 {
				n++
			}
		}
		if n == 0 {
			return apierror.InvalidArgument("The training set has no labeled documents: label at least one training document")
		}
		if len(extractorTypes(schema)) == 0 {
			return apierror.InvalidArgument("The document schema has no entity types to extract")
		}
		return nil
	}
	classes := classifierTypes(schema)
	if len(classes) < 2 {
		return apierror.InvalidArgument("A %s needs at least two document types in its schema; it has %d", kind, len(classes))
	}
	count := func(docs []*documentaipb.Document) map[string]int {
		n := map[string]int{}
		for _, d := range docs {
			if kind == "classifier" {
				n[docLabel(d)]++
				continue
			}
			for _, e := range d.GetEntities() {
				n[e.GetType()]++
			}
		}
		return n
	}
	tr, te := count(set.train), count(set.test)
	for _, c := range classes {
		if tr[c] == 0 || te[c] == 0 {
			return apierror.InvalidArgument("Document type %q needs a minimum of one document in both the training and test sets; "+
				"it has %d training and %d test documents", c, tr[c], te[c])
		}
	}
	return nil
}

func (d *documentAIService) trainCustom(ctx context.Context, p *documentaipb.Processor, req *documentaipb.TrainProcessorVersionRequest) (*longrunningpb.Operation, error) {
	kind := docAICustomTypes[p.GetType()]
	if req.GetFoundationModelTuningOptions() != nil {
		return nil, apierror.Unimplemented("Not implemented here: foundation_model_tuning_options: CloudBurrow trains its own local models " +
			"(documentaimodels.go) and has no copy of Google's foundation model to tune")
	}
	if req.GetCustomDocumentExtractionOptions() != nil && kind != "extractor" {
		return nil, apierror.InvalidArgument("custom_document_extraction_options is for a Custom Extractor; this processor is %s", p.GetType())
	}
	if strings.TrimSpace(req.GetProcessorVersion().GetDisplayName()) == "" {
		return nil, apierror.InvalidArgument("processor_version.display_name is required")
	}
	pname := p.GetName()
	s := d.s
	s.mu.Lock()
	c := s.docAI().customOf(pname)
	schema := req.GetDocumentSchema()
	if schema == nil && c.Schema != nil {
		schema = proto.Clone(c.Schema).(*documentaipb.DocumentSchema)
	}
	var set trainingSet
	if req.GetInputData() == nil {
		for _, doc := range c.Docs {
			if !doc.labeled() {
				continue
			}
			switch doc.Split {
			case "DATASET_SPLIT_TRAIN":
				set.train = append(set.train, proto.Clone(doc.Doc).(*documentaipb.Document))
			case "DATASET_SPLIT_TEST":
				set.test = append(set.test, proto.Clone(doc.Doc).(*documentaipb.Document))
			}
		}
	}
	var base *docModel
	if b := req.GetBaseProcessorVersion(); b != "" {
		v := c.version(b)
		if v == nil {
			s.mu.Unlock()
			return nil, apierror.NotFound("Processor version %s not found", b)
		}
		base = v.Model
	}
	s.mu.Unlock()
	if schema == nil || len(schema.GetEntityTypes()) == 0 {
		return nil, apierror.InvalidArgument("document_schema is required: set it on the request or on the processor's dataset")
	}
	if in := req.GetInputData(); in != nil {
		var err error
		if set.train, err = s.inputDocuments(ctx, in.GetTrainingDocuments()); err != nil {
			return nil, err
		}
		if in.GetTestDocuments() != nil {
			if set.test, err = s.inputDocuments(ctx, in.GetTestDocuments()); err != nil {
				return nil, err
			}
		}
	}
	if err := checkTrainingSet(kind, schema, set); err != nil {
		return nil, err
	}
	m := &docModel{Kind: kind}
	switch kind {
	case "classifier":
		trainClassifier(m, set.train)
	case "splitter":
		trainSplitter(m, set.train)
	default:
		trainExtractor(m, set.train)
	}
	mergeModel(m, base)
	now := timestamppb.Now()
	name := pname + "/processorVersions/" + newHexID()
	pv := &documentaipb.ProcessorVersion{Name: name, DisplayName: req.GetProcessorVersion().GetDisplayName(), DocumentSchema: schema,
		State: documentaipb.ProcessorVersion_UNDEPLOYED, CreateTime: now, ModelType: documentaipb.ProcessorVersion_MODEL_TYPE_CUSTOM}
	if p.GetKmsKeyName() != "" {
		pv = s.withKey(pv, p)
	}
	v := &customVersion{Proto: pv, Model: m}
	pr, l, _, _ := parseChild("name", "processors", pname)
	opName := s.newOp("projects/" + pr + "/locations/" + l)
	var evalRef *documentaipb.EvaluationReference
	if len(set.test) > 0 {
		ev := evaluateModel(m, schema, set.test, name+"/evaluations/"+newHexID())
		v.Evaluations = append(v.Evaluations, ev)
		evalRef = evaluationRef(ev, opName)
		pv.LatestEvaluation = evalRef
	}
	s.mu.Lock()
	c = s.docAI().customOf(pname)
	c.Versions = append(c.Versions, v)
	s.docAI().saveCustom()
	s.mu.Unlock()
	valid := &documentaipb.TrainProcessorVersionMetadata_DatasetValidation{}
	_ = s.ops.SetMetadata(opName, &documentaipb.TrainProcessorVersionMetadata{
		CommonMetadata: &documentaipb.CommonOperationMetadata{State: documentaipb.CommonOperationMetadata_SUCCEEDED,
			CreateTime: now, UpdateTime: timestamppb.Now(), Resource: name},
		TrainingDatasetValidation: valid, TestDatasetValidation: proto.Clone(valid).(*documentaipb.TrainProcessorVersionMetadata_DatasetValidation)})
	_ = s.ops.Succeed(opName, &documentaipb.TrainProcessorVersionResponse{ProcessorVersion: name})
	got, _ := s.ops.Get(opName)
	return opProto(got)
}

// --- evaluation -----------------------------------------------------------

// evalItem is one entity to match: its type, its key (the value, or for
// a splitter its pages) and its confidence.
type evalItem struct {
	typ, key string
	conf     float64
}

func normFuzzy(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

func entityItems(kind string, ents []*documentaipb.Document_Entity, text string, truth bool, exact bool) []evalItem {
	var out []evalItem
	runes := []rune(text)
	for _, e := range ents {
		conf := float64(e.GetConfidence())
		if truth {
			conf = 1
		}
		key := ""
		switch kind {
		case "splitter":
			var pages []string
			for _, r := range e.GetPageAnchor().GetPageRefs() {
				pages = append(pages, fmt.Sprint(r.GetPage()))
			}
			key = strings.Join(pages, ",")
		case "extractor":
			v := e.GetMentionText()
			if v == "" {
				if st, en, ok := mentionSpan(e); ok && st >= 0 && en <= len(runes) && st < en {
					v = string(runes[st:en])
				}
			}
			if exact {
				key = strings.TrimSpace(v)
			} else {
				key = normFuzzy(v)
			}
		}
		out = append(out, evalItem{e.GetType(), key, conf})
	}
	if kind == "classifier" && len(out) > 0 {
		// A document's class is its most probable one.
		best := out[0]
		for _, it := range out[1:] {
			if it.conf > best.conf {
				best = it
			}
		}
		out = []evalItem{best}
	}
	return out
}

// evalLevels are the confidence levels reported.
func evalLevels() []float64 {
	var out []float64
	for i := 0; i <= 20; i++ {
		out = append(out, float64(i)/20)
	}
	return out
}

type evalDoc struct {
	truth, pred []evalItem
}

func countMetrics(docs []evalDoc, typ string, level float64) *documentaipb.Evaluation_Metrics {
	m := &documentaipb.Evaluation_Metrics{TotalDocumentsCount: int32(len(docs))}
	for _, d := range docs {
		gt := map[string]int{}
		ng := 0
		for _, it := range d.truth {
			if typ == "" || it.typ == typ {
				gt[it.typ+"\x00"+it.key]++
				ng++
			}
		}
		np, tp := 0, 0
		for _, it := range d.pred {
			if (typ != "" && it.typ != typ) || it.conf < level {
				continue
			}
			np++
			k := it.typ + "\x00" + it.key
			if gt[k] > 0 {
				gt[k]--
				tp++
			}
		}
		m.TruePositivesCount += int32(tp)
		m.FalsePositivesCount += int32(np - tp)
		m.FalseNegativesCount += int32(ng - tp)
		m.PredictedOccurrencesCount += int32(np)
		m.GroundTruthOccurrencesCount += int32(ng)
		if np > 0 {
			m.PredictedDocumentCount++
		}
		if ng > 0 {
			m.GroundTruthDocumentCount++
		}
	}
	if d := m.TruePositivesCount + m.FalsePositivesCount; d > 0 {
		m.Precision = float32(m.TruePositivesCount) / float32(d)
	}
	if d := m.TruePositivesCount + m.FalseNegativesCount; d > 0 {
		m.Recall = float32(m.TruePositivesCount) / float32(d)
	}
	if m.Precision+m.Recall > 0 {
		m.F1Score = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
	}
	return m
}

func multiMetrics(docs []evalDoc, typ string) []*documentaipb.Evaluation_ConfidenceLevelMetrics {
	var out []*documentaipb.Evaluation_ConfidenceLevelMetrics
	for _, l := range evalLevels() {
		out = append(out, &documentaipb.Evaluation_ConfidenceLevelMetrics{ConfidenceLevel: float32(l), Metrics: countMetrics(docs, typ, l)})
	}
	return out
}

// auprc is the area under the precision-recall points, by recall.
func auprc(levels []*documentaipb.Evaluation_ConfidenceLevelMetrics) float32 {
	type pt struct{ r, p float64 }
	var pts []pt
	for _, l := range levels {
		pts = append(pts, pt{float64(l.GetMetrics().GetRecall()), float64(l.GetMetrics().GetPrecision())})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].r < pts[j].r })
	area, lastR, lastP := 0.0, 0.0, 1.0
	if len(pts) > 0 {
		lastP = pts[0].p
	}
	for _, x := range pts {
		area += (x.r - lastR) * (x.p + lastP) / 2
		lastR, lastP = x.r, x.p
	}
	return float32(area)
}

// calibration is the expected calibration error of the predictions over
// ten confidence bins: the mean, weighted by each bin's size, of |accuracy
// - mean confidence|.
func calibration(docs []evalDoc, typ string) float32 {
	type bin struct{ n, ok, conf float64 }
	bins := make([]bin, 10)
	total := 0.0
	for _, d := range docs {
		gt := map[string]int{}
		for _, it := range d.truth {
			gt[it.typ+"\x00"+it.key]++
		}
		for _, it := range d.pred {
			if typ != "" && it.typ != typ {
				continue
			}
			i := min(int(it.conf*10), 9)
			bins[i].n++
			bins[i].conf += it.conf
			if k := it.typ + "\x00" + it.key; gt[k] > 0 {
				gt[k]--
				bins[i].ok++
			}
			total++
		}
	}
	if total == 0 {
		return 0
	}
	e := 0.0
	for _, b := range bins {
		if b.n > 0 {
			e += b.n / total * math.Abs(b.ok/b.n-b.conf/b.n)
		}
	}
	return float32(e)
}

func multiConfidence(fuzzy, exact []evalDoc, typ string) *documentaipb.Evaluation_MultiConfidenceMetrics {
	f, x := multiMetrics(fuzzy, typ), multiMetrics(exact, typ)
	return &documentaipb.Evaluation_MultiConfidenceMetrics{ConfidenceLevelMetrics: f, ConfidenceLevelMetricsExact: x,
		Auprc: auprc(f), AuprcExact: auprc(x), EstimatedCalibrationError: calibration(fuzzy, typ),
		EstimatedCalibrationErrorExact: calibration(exact, typ), MetricsType: documentaipb.Evaluation_MultiConfidenceMetrics_AGGREGATE}
}

// evaluateModel scores m on the labeled test documents.
func evaluateModel(m *docModel, schema *documentaipb.DocumentSchema, test []*documentaipb.Document, name string) *documentaipb.Evaluation {
	var fuzzy, exact []evalDoc
	types := map[string]bool{}
	invalid := int32(0)
	for _, d := range test {
		truth := d.GetEntities()
		if len(truth) == 0 {
			invalid++
			continue
		}
		in := proto.Clone(d).(*documentaipb.Document)
		in.Entities = nil
		pred := m.predictEntities(in, schema)
		fuzzy = append(fuzzy, evalDoc{entityItems(m.Kind, truth, d.GetText(), true, false), entityItems(m.Kind, pred, d.GetText(), false, false)})
		exact = append(exact, evalDoc{entityItems(m.Kind, truth, d.GetText(), true, true), entityItems(m.Kind, pred, d.GetText(), false, true)})
		for _, e := range truth {
			types[e.GetType()] = true
		}
	}
	ev := &documentaipb.Evaluation{Name: name, CreateTime: timestamppb.Now(),
		DocumentCounters: &documentaipb.Evaluation_Counters{InputDocumentsCount: int32(len(test)), InvalidDocumentsCount: invalid,
			EvaluatedDocumentsCount: int32(len(fuzzy))},
		AllEntitiesMetrics: multiConfidence(fuzzy, exact, ""), EntityMetrics: map[string]*documentaipb.Evaluation_MultiConfidenceMetrics{}}
	for t := range types {
		ev.EntityMetrics[t] = multiConfidence(fuzzy, exact, t)
	}
	return ev
}

// bestLevel is the metrics at the confidence level of the best F1.
func bestLevel(levels []*documentaipb.Evaluation_ConfidenceLevelMetrics) *documentaipb.Evaluation_Metrics {
	var best *documentaipb.Evaluation_Metrics
	for _, l := range levels {
		if best == nil || l.GetMetrics().GetF1Score() > best.GetF1Score() {
			best = l.GetMetrics()
		}
	}
	return best
}

func evaluationRef(ev *documentaipb.Evaluation, op string) *documentaipb.EvaluationReference {
	return &documentaipb.EvaluationReference{Operation: op, Evaluation: ev.GetName(),
		AggregateMetrics:      bestLevel(ev.GetAllEntitiesMetrics().GetConfidenceLevelMetrics()),
		AggregateMetricsExact: bestLevel(ev.GetAllEntitiesMetrics().GetConfidenceLevelMetricsExact())}
}

func (d *documentAIService) evaluateCustom(ctx context.Context, name string, docs *documentaipb.BatchDocumentsInputConfig) (*longrunningpb.Operation, error) {
	s := d.s
	pname, _, _ := strings.Cut(name, "/processorVersions/")
	var test []*documentaipb.Document
	if docs != nil {
		var err error
		if test, err = s.inputDocuments(ctx, docs); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	c := s.docAI().customOf(pname)
	v := c.version(name)
	if v == nil {
		s.mu.Unlock()
		return nil, apierror.NotFound("Processor version %s not found", name)
	}
	m, schema := v.Model, proto.Clone(v.Proto.GetDocumentSchema()).(*documentaipb.DocumentSchema)
	if docs == nil {
		for _, doc := range c.Docs {
			if doc.Split == "DATASET_SPLIT_TEST" && doc.labeled() {
				test = append(test, proto.Clone(doc.Doc).(*documentaipb.Document))
			}
		}
	}
	s.mu.Unlock()
	if len(test) == 0 {
		return nil, apierror.InvalidArgument("There are no labeled test documents to evaluate %s against: label documents in the dataset's test split or set evaluation_documents", name)
	}
	pr, l, _, _ := parseChild("name", "processors", pname)
	opName := s.newOp("projects/" + pr + "/locations/" + l)
	ev := evaluateModel(m, schema, test, name+"/evaluations/"+newHexID())
	if v.Proto.GetKmsKeyName() != "" {
		ev.KmsKeyName, ev.KmsKeyVersionName = v.Proto.GetKmsKeyName(), v.Proto.GetKmsKeyVersionName()
	}
	s.mu.Lock()
	if v := s.docAI().customOf(pname).version(name); v != nil {
		v.Evaluations = append(v.Evaluations, ev)
		v.Proto.LatestEvaluation = evaluationRef(ev, opName)
	}
	s.docAI().saveCustom()
	s.mu.Unlock()
	now := timestamppb.Now()
	_ = s.ops.SetMetadata(opName, &documentaipb.EvaluateProcessorVersionMetadata{CommonMetadata: &documentaipb.CommonOperationMetadata{
		State: documentaipb.CommonOperationMetadata_SUCCEEDED, CreateTime: now, UpdateTime: now, Resource: name}})
	_ = s.ops.Succeed(opName, &documentaipb.EvaluateProcessorVersionResponse{Evaluation: ev.GetName()})
	got, _ := s.ops.Get(opName)
	return opProto(got)
}

func (s *Server) customEvaluations(version string) ([]*documentaipb.Evaluation, error) {
	pname, _, _ := strings.Cut(version, "/processorVersions/")
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.docAI().customOf(pname).version(version)
	if v == nil {
		return nil, apierror.NotFound("Processor version %s not found", version)
	}
	var out []*documentaipb.Evaluation
	for i := len(v.Evaluations) - 1; i >= 0; i-- {
		out = append(out, proto.Clone(v.Evaluations[i]).(*documentaipb.Evaluation))
	}
	return out, nil
}
