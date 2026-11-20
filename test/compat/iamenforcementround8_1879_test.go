//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	documentai "cloud.google.com/go/documentai/apiv1"
	"cloud.google.com/go/documentai/apiv1/documentaipb"
	speech "cloud.google.com/go/speech/apiv2"
	"cloud.google.com/go/speech/apiv2/speechpb"
	translate "google.golang.org/api/translate/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	ak "google.golang.org/api/apikeys/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"
	"google.golang.org/protobuf/proto"
)

// pastIAM fails unless err is an answer other than a 403: the request
// passed its IAM checks.
func pastIAM(t *testing.T, what string, err error) {
	t.Helper()
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == http.StatusForbidden {
		t.Fatalf("%s = %v; want past its IAM checks", what, err)
	}
}

// TestIAMEnforcementComputeFields (#1879), with the official Compute
// Engine REST client: an instances.insert naming another project's
// subnetwork and image and a service account needs, beyond
// compute.instances.create, compute.subnetworks.use on the subnetwork,
// compute.images.useReadOnly on the image and iam.serviceAccounts.actAs on
// the account, each refused in Compute's words until its role is granted
// (Compute Network User and Compute Image User on the host project,
// Service Account User); the instance's own account inserts it.
func TestIAMEnforcementComputeFields(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvCompute)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iecf8-")
	host := iamProject(t, h, "iecfh8-")
	p := newIAMPrincipal(t, h, project, "iecf8")
	onHost := projectRoles(t, h, host, "serviceAccount:"+p.email)
	vmAccount := newServiceAccount(t, h, project, "vm8")
	vms, err := computeapi.NewInstancesRESTClient(ctx, computeAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vms.Close() })
	subnet := "projects/" + host + "/regions/us-central1/subnetworks/shared"
	image := "projects/" + host + "/global/images/golden"
	insert := func(c *computeapi.InstancesClient) error {
		_, err := c.Insert(ctx, &computepb.InsertInstanceRequest{Project: project, Zone: "us-central1-a", InstanceResource: &computepb.Instance{
			Name:        proto.String("fields8"),
			MachineType: proto.String("zones/us-central1-a/machineTypes/no-such-type"),
			NetworkInterfaces: []*computepb.NetworkInterface{{
				Network:    proto.String("projects/" + project + "/global/networks/default"),
				Subnetwork: proto.String(subnet),
			}},
			Disks: []*computepb.AttachedDisk{{Boot: proto.Bool(true), AutoDelete: proto.Bool(true),
				InitializeParams: &computepb.AttachedDiskInitializeParams{SourceImage: proto.String(image)}}},
			ServiceAccounts: []*computepb.ServiceAccount{{Email: proto.String(vmAccount.GetEmail()),
				Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}}},
		}})
		return err
	}
	wantHTTPDenied(t, "instances.insert without a role", insert(vms), "Required 'compute.instances.create' permission for 'projects/"+project+"'")
	p.grant("roles/compute.instanceAdmin.v1")
	t.Cleanup(func() { p.revoke("roles/compute.instanceAdmin.v1") })
	waitGranted(t, "instances.create as a Compute Instance Admin", func() error {
		err := insert(vms)
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Message != "Required 'compute.instances.create' permission for 'projects/"+project+"'" {
			return nil
		}
		return err
	})
	wantHTTPDenied(t, "instances.insert without compute.subnetworks.use", insert(vms), "Required 'compute.subnetworks.use' permission for '"+subnet+"'")
	onHost("roles/compute.networkUser", true)
	t.Cleanup(func() { onHost("roles/compute.networkUser", false) })
	waitGranted(t, "compute.subnetworks.use as a Compute Network User", func() error {
		err := insert(vms)
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Message != "Required 'compute.subnetworks.use' permission for '"+subnet+"'" {
			return nil
		}
		return err
	})
	wantHTTPDenied(t, "instances.insert without compute.images.useReadOnly", insert(vms), "Required 'compute.images.useReadOnly' permission for '"+image+"'")
	onHost("roles/compute.imageUser", true)
	t.Cleanup(func() { onHost("roles/compute.imageUser", false) })
	saResource := "projects/" + project + "/serviceAccounts/" + vmAccount.GetEmail()
	waitGranted(t, "compute.images.useReadOnly as a Compute Image User", func() error {
		err := insert(vms)
		var ge *googleapi.Error
		if errors.As(err, &ge) && ge.Message != "Required 'compute.images.useReadOnly' permission for '"+image+"'" {
			return nil
		}
		return err
	})
	wantHTTPDenied(t, "instances.insert without actAs", insert(vms), "Required 'iam.serviceAccounts.actAs' permission for '"+saResource+"'")
	p.grant("roles/iam.serviceAccountUser")
	t.Cleanup(func() { p.revoke("roles/iam.serviceAccountUser") })
	waitHTTPGranted(t, "instances.insert as a Service Account User", func() error { return insert(vms) })
	pastIAM(t, "instances.insert with every role", insert(vms))
	owner, err := computeapi.NewInstancesRESTClient(ctx, computeAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	pastIAM(t, "instances.insert as the instance's account", insert(owner))
}

// TestIAMEnforcementAPIKeysLookupOperations (#1879), with the official API
// Keys and Service Usage clients: keys.lookupKey needs apikeys.keys.lookup
// on the key's project, and both APIs' operations.get
// serviceusage.operations.get on the operation's project; without a role
// each is refused, API Keys Viewer looks a key up, API Keys Admin and
// Service Usage Viewer read the operations, and the instance's own account
// reads them all.
func TestIAMEnforcementAPIKeysLookupOperations(t *testing.T) {
	h := New(t)
	keysEndpoint := h.Endpoint(EnvAPIKeys)
	usageEndpoint := h.Endpoint(EnvServiceUsage)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieal8-")
	p := newIAMPrincipal(t, h, project, "ieal8")
	ownerKeys, err := ak.NewService(ctx, option.WithEndpoint("http://"+stripScheme(keysEndpoint)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	ownerUsage, err := su.NewService(ctx, option.WithEndpoint("http://"+stripScheme(usageEndpoint)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	loc := "projects/" + project + "/locations/global"
	keyOp, err := ownerKeys.Projects.Locations.Keys.Create(loc, &ak.V2Key{DisplayName: "lookup8"}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("keys.create as the instance's account: %v", err)
	}
	var key ak.V2Key
	if err := json.Unmarshal(keyOp.Response, &key); err != nil || key.Name == "" {
		t.Fatalf("keys.create's response = %s (%v)", keyOp.Response, err)
	}
	ks, err := ownerKeys.Projects.Locations.Keys.GetKeyString(key.Name).Context(ctx).Do()
	if err != nil {
		t.Fatalf("keys.getKeyString as the instance's account: %v", err)
	}
	svc := "projects/" + project + "/services/pubsub.googleapis.com"
	usageOp, err := ownerUsage.Services.Enable(svc, &su.EnableServiceRequest{}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("services.enable as the instance's account: %v", err)
	}
	keys, err := ak.NewService(ctx, restAs(keysEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := su.NewService(ctx, restAs(usageEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func() error { _, err := keys.Keys.LookupKey().KeyString(ks.KeyString).Context(ctx).Do(); return err }
	keyOpGet := func() error { _, err := keys.Operations.Get(keyOp.Name).Context(ctx).Do(); return err }
	usageOpGet := func() error { _, err := usage.Operations.Get(usageOp.Name).Context(ctx).Do(); return err }
	wantHTTPDenied(t, "keys.lookupKey without a role", lookup(), newerDenied("apikeys.keys.lookup", "projects/"+project))
	wantHTTPDenied(t, "API Keys operations.get without a role", keyOpGet(), newerDenied("serviceusage.operations.get", "projects/"+project))
	wantHTTPDenied(t, "Service Usage operations.get without a role", usageOpGet(), newerDenied("serviceusage.operations.get", "projects/"+project))
	p.grant("roles/serviceusage.apiKeysViewer")
	t.Cleanup(func() { p.revoke("roles/serviceusage.apiKeysViewer") })
	waitHTTPGranted(t, "keys.lookupKey as an API Keys Viewer", lookup)
	if err := lookup(); err != nil {
		t.Fatalf("keys.lookupKey as an API Keys Viewer: %v", err)
	}
	wantHTTPDenied(t, "operations.get as an API Keys Viewer", keyOpGet(), newerDenied("serviceusage.operations.get", "projects/"+project))
	p.grant("roles/serviceusage.serviceUsageViewer")
	t.Cleanup(func() { p.revoke("roles/serviceusage.serviceUsageViewer") })
	waitHTTPGranted(t, "Service Usage operations.get as a Service Usage Viewer", usageOpGet)
	for what, call := range map[string]func() error{"API Keys operations.get": keyOpGet, "Service Usage operations.get": usageOpGet} {
		if err := call(); err != nil {
			t.Fatalf("%s as a Service Usage Viewer: %v", what, err)
		}
	}
	if _, err := ownerKeys.Keys.LookupKey().KeyString(ks.KeyString).Context(ctx).Do(); err != nil {
		t.Fatalf("keys.lookupKey as the instance's account: %v", err)
	}
	if _, err := ownerUsage.Operations.Get(usageOp.Name).Context(ctx).Do(); err != nil {
		t.Fatalf("operations.get as the instance's account: %v", err)
	}
}

// TestIAMEnforcementLocalAI (#1879), with the official Document AI,
// Speech-to-Text v2 and Vertex AI clients over gRPC and the Translation
// v3 client over JSON, on the local AI endpoint: without a role
// ListProcessors, ListRecognizers, a StreamingRecognize stream,
// GenerateContent and translateText are refused with each method's
// permission; Document AI Viewer lists processors but cannot create one,
// Cloud Speech Client lists recognizers and streams, Vertex AI User and
// Cloud Translation API User get past their checks; the instance's own
// account lists both.
func TestIAMEnforcementLocalAI(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLocalAI)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieai8-")
	p := newIAMPrincipal(t, h, project, "ieai8")
	docs, err := documentai.NewDocumentProcessorClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = docs.Close() })
	sp, err := speech.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	gen, err := aiplatform.NewPredictionClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gen.Close() })
	tr, err := translate.NewService(ctx, restAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	docParent := "projects/" + project + "/locations/us"
	speechParent := "projects/" + project + "/locations/global"
	recognizer := speechParent + "/recognizers/_"
	model := "projects/" + project + "/locations/us-central1/publishers/google/models/gemini-2.5-flash"
	listDocs := func() error {
		_, err := docs.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: docParent}).Next()
		return first(err)
	}
	listRecognizers := func() error {
		_, err := sp.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: speechParent}).Next()
		return first(err)
	}
	stream := func() error {
		s, err := sp.StreamingRecognize(ctx)
		if err != nil {
			return err
		}
		if err := s.Send(&speechpb.StreamingRecognizeRequest{Recognizer: recognizer}); err != nil {
			return err
		}
		_ = s.CloseSend()
		_, err = s.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	generate := func() error {
		_, err := gen.GenerateContent(ctx, &aiplatformpb.GenerateContentRequest{Model: model,
			Contents: []*aiplatformpb.Content{{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: "hi"}}}}}})
		return err
	}
	translateText := func() error {
		_, err := tr.Projects.Locations.TranslateText(speechParent, &translate.TranslateTextRequest{Contents: []string{"hello"},
			TargetLanguageCode: "fr", SourceLanguageCode: "en"}).Context(ctx).Do()
		return err
	}
	wantDenied(t, "ListProcessors without a role", listDocs(), newerDenied("documentai.processors.list", docParent), "documentai.processors.list", true)
	wantDenied(t, "ListRecognizers without a role", listRecognizers(), newerDenied("speech.recognizers.list", speechParent), "speech.recognizers.list", true)
	wantDenied(t, "StreamingRecognize without a role", stream(), newerDenied("speech.recognizers.recognize", recognizer), "speech.recognizers.recognize", true)
	wantDenied(t, "GenerateContent without a role", generate(), newerDenied("aiplatform.endpoints.predict", model), "aiplatform.endpoints.predict", true)
	wantHTTPDenied(t, "translateText without a role", translateText(), newerDenied("cloudtranslate.generalModels.predict", speechParent))

	p.grant("roles/documentai.viewer")
	t.Cleanup(func() { p.revoke("roles/documentai.viewer") })
	waitGranted(t, "ListProcessors as a Document AI Viewer", listDocs)
	_, err = docs.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: docParent,
		Processor: &documentaipb.Processor{Type: "OCR_PROCESSOR", DisplayName: "enforced"}})
	wantDenied(t, "CreateProcessor as a Viewer", err, newerDenied("documentai.processors.create", docParent), "documentai.processors.create", true)
	p.grant("roles/speech.client")
	t.Cleanup(func() { p.revoke("roles/speech.client") })
	waitGranted(t, "ListRecognizers as a Cloud Speech Client", listRecognizers)
	if err := stream(); status.Code(err) == codes.PermissionDenied {
		t.Fatalf("StreamingRecognize as a Cloud Speech Client: %v", err)
	}
	p.grant("roles/aiplatform.user")
	t.Cleanup(func() { p.revoke("roles/aiplatform.user") })
	waitGranted(t, "GenerateContent as a Vertex AI User", func() error {
		if err := generate(); status.Code(err) == codes.PermissionDenied {
			return err
		}
		return nil
	})
	p.grant("roles/cloudtranslate.user")
	t.Cleanup(func() { p.revoke("roles/cloudtranslate.user") })
	waitHTTPGranted(t, "translateText as a Cloud Translation API User", translateText)

	ownerDocs, err := documentai.NewDocumentProcessorClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownerDocs.Close() })
	if _, err := ownerDocs.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: docParent}).Next(); first(err) != nil {
		t.Fatalf("ListProcessors as the instance's account: %v", err)
	}
	ownerSpeech, err := speech.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownerSpeech.Close() })
	if _, err := ownerSpeech.ListRecognizers(ctx, &speechpb.ListRecognizersRequest{Parent: speechParent}).Next(); first(err) != nil {
		t.Fatalf("ListRecognizers as the instance's account: %v", err)
	}
}
