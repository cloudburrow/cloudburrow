//go:build compat

package compat

import (
	"testing"

	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	documentaibeta "cloud.google.com/go/documentai/apiv1beta3"
	beta "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestIAMEnforcementDocumentAIDatasets (#2124): a custom processor's
// dataset methods, through the official v1beta3 DocumentService client
// over gRPC, each refused without a role with the permission its REST
// reference names on the dataset (ListDocuments
// documentai.datasets.listDocuments, GetDatasetSchema
// documentai.datasetSchemas.get, UpdateDataset documentai.datasets.update);
// as a Document AI Viewer the reads get past their checks and
// ImportDocuments (documentai.datasets.createDocuments),
// BatchDeleteDocuments (documentai.datasets.deleteDocuments) and
// UpdateDatasetSchema (documentai.datasetSchemas.update) are refused; as a
// Document AI Editor they pass.
func TestIAMEnforcementDocumentAIDatasets(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLocalAI)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieds-")
	p := newIAMPrincipal(t, h, project, "ieds")
	owner, err := documentai.NewDocumentProcessorClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	ownerDS, err := documentaibeta.NewDocumentClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownerDS.Close() })
	proc, err := owner.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: "projects/" + project + "/locations/us",
		Processor: &documentaipb.Processor{Type: "CUSTOM_CLASSIFICATION_PROCESSOR", DisplayName: "ieds"}})
	if err != nil {
		t.Fatalf("CreateProcessor: %v", err)
	}
	t.Cleanup(func() {
		if op, err := owner.DeleteProcessor(ctx, &documentaipb.DeleteProcessorRequest{Name: proc.GetName()}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	ds := proc.GetName() + "/dataset"
	unmanaged := &beta.Dataset{Name: ds, StorageSource: &beta.Dataset_UnmanagedDatasetConfig_{UnmanagedDatasetConfig: &beta.Dataset_UnmanagedDatasetConfig{}}}
	if op, err := ownerDS.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: unmanaged}); err != nil {
		t.Fatalf("UpdateDataset as the instance's account: %v", err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("UpdateDataset as the instance's account: %v", err)
	}

	c, err := documentaibeta.NewDocumentClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	list := func() error {
		_, err := c.ListDocuments(ctx, &beta.ListDocumentsRequest{Dataset: ds}).Next()
		return first(err)
	}
	schema := func() error {
		_, err := c.GetDatasetSchema(ctx, &beta.GetDatasetSchemaRequest{Name: ds + "/datasetSchema"})
		return err
	}
	update := func() error {
		_, err := c.UpdateDataset(ctx, &beta.UpdateDatasetRequest{Dataset: unmanaged})
		return err
	}
	imp := func() error {
		_, err := c.ImportDocuments(ctx, &beta.ImportDocumentsRequest{Dataset: ds, BatchDocumentsImportConfigs: []*beta.ImportDocumentsRequest_BatchDocumentsImportConfig{{
			BatchInputConfig: &beta.BatchDocumentsInputConfig{Source: &beta.BatchDocumentsInputConfig_GcsPrefix{GcsPrefix: &beta.GcsPrefix{GcsUriPrefix: "gs://ieds-none/"}}}}}})
		return err
	}
	del := func() error {
		_, err := c.BatchDeleteDocuments(ctx, &beta.BatchDeleteDocumentsRequest{Dataset: ds, DatasetDocuments: &beta.BatchDatasetDocuments{
			Criteria: &beta.BatchDatasetDocuments_Filter{Filter: "SplitType=DATASET_SPLIT_TEST"}}})
		return err
	}
	setSchema := func() error {
		_, err := c.UpdateDatasetSchema(ctx, &beta.UpdateDatasetSchemaRequest{DatasetSchema: &beta.DatasetSchema{Name: ds + "/datasetSchema",
			DocumentSchema: &beta.DocumentSchema{EntityTypes: []*beta.DocumentSchema_EntityType{{Name: "invoice"}, {Name: "receipt"}}}}})
		return err
	}
	wantDenied(t, "ListDocuments without a role", list(), newerDenied("documentai.datasets.listDocuments", ds), "documentai.datasets.listDocuments", true)
	wantDenied(t, "GetDatasetSchema without a role", schema(), newerDenied("documentai.datasetSchemas.get", ds+"/datasetSchema"), "documentai.datasetSchemas.get", true)
	wantDenied(t, "UpdateDataset without a role", update(), newerDenied("documentai.datasets.update", ds), "documentai.datasets.update", true)

	p.grant("roles/documentai.viewer")
	t.Cleanup(func() { p.revoke("roles/documentai.viewer") })
	waitGranted(t, "ListDocuments as a Document AI Viewer", list)
	if err := schema(); err != nil {
		t.Fatalf("GetDatasetSchema as a Document AI Viewer: %v", err)
	}
	wantDenied(t, "ImportDocuments as a Viewer", imp(), newerDenied("documentai.datasets.createDocuments", ds), "documentai.datasets.createDocuments", true)
	wantDenied(t, "BatchDeleteDocuments as a Viewer", del(), newerDenied("documentai.datasets.deleteDocuments", ds), "documentai.datasets.deleteDocuments", true)
	wantDenied(t, "UpdateDatasetSchema as a Viewer", setSchema(), newerDenied("documentai.datasetSchemas.update", ds+"/datasetSchema"), "documentai.datasetSchemas.update", true)

	p.grant("roles/documentai.editor")
	t.Cleanup(func() { p.revoke("roles/documentai.editor") })
	waitGranted(t, "UpdateDatasetSchema as a Document AI Editor", setSchema)
	if err := del(); err != nil {
		t.Fatalf("BatchDeleteDocuments as an Editor: %v", err)
	}
	// Past its check, the import reads a bucket that does not exist.
	if err := imp(); status.Code(err) == codes.PermissionDenied {
		t.Fatalf("ImportDocuments as an Editor: %v", err)
	}
}
