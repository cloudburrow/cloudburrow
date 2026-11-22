//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// documentAIClients are the official Document AI clients, gRPC and REST, on
// the instance's local AI endpoint (#1799), never Google's.
func documentAIClients(t *testing.T, h *Harness) (*documentai.DocumentProcessorClient, *documentai.DocumentProcessorClient) {
	t.Helper()
	addr := h.Endpoint(EnvLocalAI)
	if addr == "" {
		t.Fatal("no local AI endpoint")
	}
	g, err := documentai.NewDocumentProcessorClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("documentai.NewDocumentProcessorClient: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	r, err := documentai.NewDocumentProcessorRESTClient(h.Context(), option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("documentai.NewDocumentProcessorRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return g, r
}

// createDocumentProcessor makes a processor of typ in the us location, and
// deletes it when the test ends.
func createDocumentProcessor(t *testing.T, h *Harness, c *documentai.DocumentProcessorClient, project, typ string) *documentaipb.Processor {
	t.Helper()
	p, err := c.CreateProcessor(h.Context(), &documentaipb.CreateProcessorRequest{Parent: "projects/" + project + "/locations/us",
		Processor: &documentaipb.Processor{Type: typ, DisplayName: strings.ToLower(typ) + "-" + h.Project()}})
	if err != nil {
		t.Fatalf("CreateProcessor %s: %v", typ, err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteProcessor(h.Context(), &documentaipb.DeleteProcessorRequest{Name: p.GetName()}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	return p
}

// TestDocumentAIProcessors (#1799): Document AI v1's DocumentProcessorService
// on the local AI endpoint, through the official
// cloud.google.com/go/documentai client over gRPC and REST: the served
// processor types (the custom ones since #2106); a processor created, read, listed, disabled (a
// FAILED_PRECONDITION to process with), enabled and deleted (operations
// waited on); an unserved type UNIMPLEMENTED; the OCR processor reading the
// scanned page with local Tesseract into Document.text, pages, lines and
// tokens whose anchors slice the text; the layout parser's blocks and
// chunks over REST.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/FetchProcessorTypes, google.cloud.documentai.v1.DocumentProcessorService/ListProcessorTypes, google.cloud.documentai.v1.DocumentProcessorService/GetProcessorType, google.cloud.documentai.v1.DocumentProcessorService/CreateProcessor, google.cloud.documentai.v1.DocumentProcessorService/GetProcessor, google.cloud.documentai.v1.DocumentProcessorService/ListProcessors, google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument, google.cloud.documentai.v1.DocumentProcessorService/DisableProcessor, google.cloud.documentai.v1.DocumentProcessorService/EnableProcessor, google.cloud.documentai.v1.DocumentProcessorService/DeleteProcessor
func TestDocumentAIProcessors(t *testing.T) {
	h := New(t)
	g, r := documentAIClients(t, h)
	ctx := h.Context()
	project := h.Project()
	parent := "projects/" + project + "/locations/us"
	scan, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}

	types, err := g.FetchProcessorTypes(ctx, &documentaipb.FetchProcessorTypesRequest{Parent: parent})
	if err != nil || len(types.GetProcessorTypes()) != 5 {
		t.Fatalf("FetchProcessorTypes: %v %v", types, err)
	}
	var listed []string
	it := r.ListProcessorTypes(ctx, &documentaipb.ListProcessorTypesRequest{Parent: parent})
	for {
		pt, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListProcessorTypes: %v", err)
		}
		listed = append(listed, pt.GetType())
	}
	if strings.Join(listed, ",") != "CUSTOM_CLASSIFICATION_PROCESSOR,CUSTOM_EXTRACTION_PROCESSOR,CUSTOM_SPLITTING_PROCESSOR,LAYOUT_PARSER_PROCESSOR,OCR_PROCESSOR" {
		t.Errorf("ListProcessorTypes: %v", listed)
	}
	if pt, err := g.GetProcessorType(ctx, &documentaipb.GetProcessorTypeRequest{Name: parent + "/processorTypes/OCR_PROCESSOR"}); err != nil || !pt.GetAllowCreation() {
		t.Errorf("GetProcessorType: %v %v", pt, err)
	}
	if _, err := g.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: parent,
		Processor: &documentaipb.Processor{Type: "FORM_PARSER_PROCESSOR", DisplayName: "form"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("FORM_PARSER_PROCESSOR: %v", err)
	}

	ocrP := createDocumentProcessor(t, h, g, project, "OCR_PROCESSOR")
	if got, err := r.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: ocrP.GetName()}); err != nil ||
		got.GetState() != documentaipb.Processor_ENABLED || got.GetType() != "OCR_PROCESSOR" {
		t.Errorf("GetProcessor over REST: %v %v", got, err)
	}
	found := false
	pit := g.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: parent})
	for {
		p, err := pit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListProcessors: %v", err)
		}
		found = found || p.GetName() == ocrP.GetName()
	}
	if !found {
		t.Errorf("ListProcessors does not list %s", ocrP.GetName())
	}

	raw := &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: scan, MimeType: "image/png"}}
	res, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: ocrP.GetName(), Source: raw})
	if err != nil {
		t.Fatalf("ProcessDocument (OCR): %v", err)
	}
	doc := res.GetDocument()
	if !strings.Contains(doc.GetText(), "INVOICE No. 2026-0417") || len(doc.GetPages()) != 1 || len(doc.Pages[0].GetTokens()) < 10 ||
		len(doc.Pages[0].GetLines()) == 0 || doc.Pages[0].GetDimension().GetWidth() <= 0 {
		t.Fatalf("OCR document: text %q, %d pages", doc.GetText(), len(doc.GetPages()))
	}
	seg := doc.Pages[0].Lines[0].GetLayout().GetTextAnchor().GetTextSegments()[0]
	if line := string([]rune(doc.GetText())[seg.GetStartIndex():seg.GetEndIndex()]); line != "INVOICE No. 2026-0417\n" {
		t.Errorf("first line's anchor slices %q", line)
	}

	op, err := g.DisableProcessor(ctx, &documentaipb.DisableProcessorRequest{Name: ocrP.GetName()})
	if err != nil {
		t.Fatalf("DisableProcessor: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("DisableProcessor wait: %v", err)
	}
	if _, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: ocrP.GetName(), Source: raw}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("process with a disabled processor: %v", err)
	}
	eop, err := g.EnableProcessor(ctx, &documentaipb.EnableProcessorRequest{Name: ocrP.GetName()})
	if err != nil {
		t.Fatalf("EnableProcessor: %v", err)
	}
	if _, err := eop.Wait(ctx); err != nil {
		t.Fatalf("EnableProcessor wait: %v", err)
	}

	layout := createDocumentProcessor(t, h, r, project, "LAYOUT_PARSER_PROCESSOR")
	res, err = r.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: layout.GetName(), Source: raw,
		ProcessOptions: &documentaipb.ProcessOptions{LayoutConfig: &documentaipb.ProcessOptions_LayoutConfig{
			ChunkingConfig: &documentaipb.ProcessOptions_LayoutConfig_ChunkingConfig{ChunkSize: 30}}}})
	if err != nil {
		t.Fatalf("ProcessDocument (layout parser, REST): %v", err)
	}
	blocks, chunks := res.GetDocument().GetDocumentLayout().GetBlocks(), res.GetDocument().GetChunkedDocument().GetChunks()
	if len(blocks) < 2 || len(chunks) < 2 || blocks[0].GetTextBlock().GetType() != "paragraph" || chunks[0].GetChunkId() != "c1" ||
		!strings.Contains(chunks[0].GetContent(), "INVOICE No. 2026-0417") {
		t.Errorf("layout document: %d blocks, %d chunks: %v", len(blocks), len(chunks), res.GetDocument())
	}

	dop, err := g.DeleteProcessor(ctx, &documentaipb.DeleteProcessorRequest{Name: ocrP.GetName()})
	if err != nil {
		t.Fatalf("DeleteProcessor: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteProcessor wait: %v", err)
	}
	if _, err := g.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: ocrP.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("deleted processor: %v", err)
	}
}

// TestBigQueryMLProcessDocument (#1799): ML.PROCESS_DOCUMENT through the
// official cloud.google.com/go/bigquery, storage, bigquery/connection/apiv1
// and documentai clients: a layout parser created on the local Document AI
// API, a CLOUD_AI_DOCUMENT_V1 remote model naming it by its ID, and the
// object table's scanned page processed into ml_process_document_result's
// chunkedDocument and documentLayout (local Tesseract, never Google), with
// process_options' chunk size; an object that is not an image or PDF is its
// row's error; a query over the object table selects the rows processed.
// CREATE MODEL fails without DOCUMENT_PROCESSOR, with a processor that does
// not exist, and with the OCR processor, which BigQuery does not support.
//
// covers: bigquery.jobs.query
func TestBigQueryMLProcessDocument(t *testing.T) {
	h := New(t)
	g, _ := documentAIClients(t, h)
	c, ds, id := visionObjectTable(t, h)
	run := func(sql string) error { return mlRun(h, c, sql) }
	conn := c.Project() + ".us." + id
	layout := createDocumentProcessor(t, h, g, c.Project(), "LAYOUT_PARSER_PROCESSOR")
	ocrP := createDocumentProcessor(t, h, g, c.Project(), "OCR_PROCESSOR")
	pid := layout.GetName()[strings.LastIndex(layout.GetName(), "/")+1:]

	for opts, want := range map[string]string{
		"": "DOCUMENT_PROCESSOR",
		", DOCUMENT_PROCESSOR = 'ffffffffffffffff'":                     "doesn't exist",
		", DOCUMENT_PROCESSOR = '" + ocrP.GetName() + "'":               "not supported",
		", DOCUMENT_PROCESSOR = 'projects/1/locations/eu/processors/x'": "different region",
	} {
		err := run(fmt.Sprintf("CREATE MODEL `%s.refused` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_DOCUMENT_V1'%s)", ds, conn, opts))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CREATE MODEL with %q: %v, want %q", opts, err, want)
		}
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.docs` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_DOCUMENT_V1', DOCUMENT_PROCESSOR = '%s')",
		ds, conn, pid)); err != nil {
		t.Fatalf("CREATE MODEL CLOUD_AI_DOCUMENT_V1: %v", err)
	}
	md, err := generatedBigQuery(t, h).Models.Get(c.Project(), ds, "docs").Context(h.Context()).Do()
	if err != nil || md.RemoteModelInfo == nil || md.RemoteModelInfo.RemoteServiceType != "CLOUD_AI_DOCUMENT_V1" {
		t.Errorf("models.get: %v %+v", err, md)
	}

	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT uri, content_type, ml_process_document_status AS status, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_process_document_result, '$.chunkedDocument.chunks')) AS chunks, "+
		"JSON_VALUE(ml_process_document_result, '$.chunkedDocument.chunks[0].content') AS content, "+
		"JSON_VALUE(ml_process_document_result, '$.chunkedDocument.chunks[0].chunkId') AS chunk_id, "+
		"JSON_VALUE(ml_process_document_result, '$.documentLayout.blocks[0].textBlock.type') AS block_type "+
		"FROM ML.PROCESS_DOCUMENT(MODEL `%s.docs`, TABLE `%s.images`, "+
		"PROCESS_OPTIONS => (JSON '{\"layout_config\": {\"chunking_config\": {\"chunk_size\": 30, \"include_ancestor_headings\": true}}}')) ORDER BY uri", ds, ds))
	if len(rows) != 2 {
		t.Fatalf("rows: %v", rows)
	}
	if s, _ := rows[0]["status"].(string); !strings.HasSuffix(fmt.Sprint(rows[0]["uri"]), "/img/broken.png") || !strings.Contains(s, "INVALID_ARGUMENT") || rows[0]["content"] != nil {
		t.Errorf("broken object row: %v", rows[0])
	}
	n, _ := rows[1]["chunks"].(int64)
	if content, _ := rows[1]["content"].(string); rows[1]["status"] != "" || n < 2 || rows[1]["chunk_id"] != "c1" || rows[1]["block_type"] != "paragraph" ||
		!strings.Contains(content, "INVOICE No. 2026-0417") || rows[1]["content_type"] != "image/png" {
		t.Errorf("scanned page row: %v", rows[1])
	}

	// A query over the object table: only its rows are processed.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT uri, ml_process_document_status AS status, "+
		"JSON_VALUE(ml_process_document_result, '$.chunkedDocument.chunks[0].chunkId') AS chunk_id "+
		"FROM ML.PROCESS_DOCUMENT(MODEL `%s.docs`, (SELECT uri, content_type FROM `%s.images` WHERE uri LIKE '%%scan%%'))", ds, ds))
	if len(rows) != 1 || rows[0]["status"] != "" || rows[0]["chunk_id"] != "c1" {
		t.Errorf("over a query: %v", rows)
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.PROCESS_DOCUMENT(MODEL `%s.docs`, (SELECT a.uri, a.content_type FROM `%s.images` a JOIN `%s.images` b USING (uri)))", ds, ds, ds)); err == nil {
		t.Error("a query with a JOIN was accepted")
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.ANNOTATE_IMAGE(MODEL `%s.docs`, TABLE `%s.images`, STRUCT(['text_detection'] AS vision_features))", ds, ds)); err == nil ||
		!strings.Contains(err.Error(), "ML.PROCESS_DOCUMENT") {
		t.Errorf("ML.ANNOTATE_IMAGE of a Document AI model: %v", err)
	}
}

// TestDocumentAIProcessorVersions (#1930): a processor's pretrained versions
// through the official client, gRPC and REST: listed and read, "stable"
// naming the newest; SetDefaultProcessorVersion (an operation waited on)
// changes the default ProcessDocument reads with; Deploy, Undeploy and
// Delete of a pretrained version are FAILED_PRECONDITION, as
// https://docs.cloud.google.com/document-ai/docs/manage-processor-versions
// says they cannot be changed; TrainProcessorVersion of an OCR processor,
// which is not trainable, is INVALID_ARGUMENT.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/ListProcessorVersions, google.cloud.documentai.v1.DocumentProcessorService/GetProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/SetDefaultProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/DeployProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/UndeployProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/DeleteProcessorVersion, google.cloud.documentai.v1.DocumentProcessorService/TrainProcessorVersion
func TestDocumentAIProcessorVersions(t *testing.T) {
	h := New(t)
	g, r := documentAIClients(t, h)
	ctx := h.Context()
	p := createDocumentProcessor(t, h, g, h.Project(), "OCR_PROCESSOR")
	for _, c := range []*documentai.DocumentProcessorClient{g, r} {
		var ids []string
		it := c.ListProcessorVersions(ctx, &documentaipb.ListProcessorVersionsRequest{Parent: p.GetName()})
		for {
			v, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListProcessorVersions: %v", err)
			}
			if !v.GetGoogleManaged() || v.GetState() != documentaipb.ProcessorVersion_DEPLOYED {
				t.Errorf("version %v", v)
			}
			ids = append(ids, v.GetName()[strings.LastIndex(v.GetName(), "/")+1:])
		}
		if strings.Join(ids, ",") != "pretrained-ocr-v2.1-2024-08-07,pretrained-ocr-v2.0-2023-06-02,pretrained-ocr-v1.2-2022-11-10" {
			t.Errorf("versions: %v", ids)
		}
	}
	if v, err := r.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetName() + "/processorVersions/stable"}); err != nil ||
		!strings.HasSuffix(v.GetName(), "/pretrained-ocr-v2.1-2024-08-07") {
		t.Errorf("stable: %v %v", v, err)
	}
	v12 := p.GetName() + "/processorVersions/pretrained-ocr-v1.2-2022-11-10"
	op, err := g.SetDefaultProcessorVersion(ctx, &documentaipb.SetDefaultProcessorVersionRequest{Processor: p.GetName(), DefaultProcessorVersion: v12})
	if err != nil {
		t.Fatalf("SetDefaultProcessorVersion: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("SetDefaultProcessorVersion's operation: %v", err)
	}
	if got, err := r.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: p.GetName()}); err != nil || got.GetDefaultProcessorVersion() != v12 {
		t.Errorf("default after SetDefaultProcessorVersion: %v %v", got.GetDefaultProcessorVersion(), err)
	}
	if _, err := g.DeployProcessorVersion(ctx, &documentaipb.DeployProcessorVersionRequest{Name: v12}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Deploy of a pretrained version: %v", err)
	}
	if _, err := g.UndeployProcessorVersion(ctx, &documentaipb.UndeployProcessorVersionRequest{Name: v12}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Undeploy of a pretrained version: %v", err)
	}
	if _, err := g.DeleteProcessorVersion(ctx, &documentaipb.DeleteProcessorVersionRequest{
		Name: p.GetName() + "/processorVersions/pretrained-ocr-v2.0-2023-06-02"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Delete of a pretrained version: %v", err)
	}
	if _, err := g.GetProcessorVersion(ctx, &documentaipb.GetProcessorVersionRequest{Name: p.GetName() + "/processorVersions/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("an unknown version: %v", err)
	}
	if _, err := g.TrainProcessorVersion(ctx, &documentaipb.TrainProcessorVersionRequest{Parent: p.GetName(),
		ProcessorVersion: &documentaipb.ProcessorVersion{DisplayName: "mine"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("TrainProcessorVersion of the OCR processor: %v", err)
	}
}

// TestDocumentAIBatchProcess (#1930): BatchProcessDocuments through the
// official client reads the scanned page from the instance's Cloud Storage
// and writes its Document, as Google lays the output out, under
// {gcs_uri}/{operation ID}/0/scan-0.json; the operation's
// BatchProcessMetadata names the input and output; a missing object is the
// document's error in the metadata, and the operation fails when every
// document does.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/BatchProcessDocuments
func TestDocumentAIBatchProcess(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvStorage) == "" {
		t.Skip("this instance serves no Cloud Storage")
	}
	g, _ := documentAIClients(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	scan, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	w := bh.Object("in/scan.png").NewWriter(ctx)
	w.ContentType = "image/png"
	if _, err := w.Write(scan); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	p := createDocumentProcessor(t, h, g, h.Project(), "OCR_PROCESSOR")
	out := &documentaipb.DocumentOutputConfig{Destination: &documentaipb.DocumentOutputConfig_GcsOutputConfig_{
		GcsOutputConfig: &documentaipb.DocumentOutputConfig_GcsOutputConfig{GcsUri: "gs://" + bh.BucketName() + "/out"}}}
	op, err := g.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), DocumentOutputConfig: out,
		InputDocuments: &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsPrefix{
			GcsPrefix: &documentaipb.GcsPrefix{GcsUriPrefix: "gs://" + bh.BucketName() + "/in/"}}}})
	if err != nil {
		t.Fatalf("BatchProcessDocuments: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("BatchProcessDocuments' operation: %v", err)
	}
	meta, err := op.Metadata()
	if err != nil || meta.GetState() != documentaipb.BatchProcessMetadata_SUCCEEDED || len(meta.GetIndividualProcessStatuses()) != 1 {
		t.Fatalf("metadata: %v %v", meta, err)
	}
	st := meta.IndividualProcessStatuses[0]
	id := op.Name()[strings.LastIndex(op.Name(), "/")+1:]
	if st.GetInputGcsSource() != "gs://"+bh.BucketName()+"/in/scan.png" || st.GetOutputGcsDestination() != "gs://"+bh.BucketName()+"/out/"+id+"/0" ||
		st.GetStatus().GetCode() != 0 {
		t.Errorf("status: %v", st)
	}
	rd, err := bh.Object("out/" + id + "/0/scan-0.json").NewReader(ctx)
	if err != nil {
		t.Fatalf("the output Document: %v", err)
	}
	b := make([]byte, 0, 1<<20)
	buf := make([]byte, 64<<10)
	for {
		n, err := rd.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	_ = rd.Close()
	var doc documentaipb.Document
	if err := protojson.Unmarshal(b, &doc); err != nil || !strings.Contains(doc.GetText(), "INVOICE") || len(doc.GetPages()) != 1 {
		t.Errorf("the output Document: %v %.200q", err, doc.GetText())
	}

	op, err = g.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), DocumentOutputConfig: out,
		InputDocuments: &documentaipb.BatchDocumentsInputConfig{Source: &documentaipb.BatchDocumentsInputConfig_GcsDocuments{
			GcsDocuments: &documentaipb.GcsDocuments{Documents: []*documentaipb.GcsDocument{
				{GcsUri: "gs://" + bh.BucketName() + "/in/missing.png", MimeType: "image/png"}}}}}})
	if err != nil {
		t.Fatalf("BatchProcessDocuments of a missing object: %v", err)
	}
	if _, err := op.Wait(ctx); status.Code(err) != codes.NotFound {
		t.Errorf("a batch whose one document is missing: %v", err)
	}
	if _, err := g.BatchProcessDocuments(ctx, &documentaipb.BatchProcessRequest{Name: p.GetName(), DocumentOutputConfig: out}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no input: %v", err)
	}
}
