package iameval

import (
	"slices"
	"strings"
	"testing"

	_ "cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	_ "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	_ "cloud.google.com/go/documentai/apiv1/documentaipb"
	_ "cloud.google.com/go/documentai/apiv1beta3/documentaipb"
	_ "cloud.google.com/go/speech/apiv1/speechpb"
	_ "cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestComputeFieldChecks (#1879): an insert's fields ask for the
// permissions their REST reference names, on the resources they name.
func TestComputeFieldChecks(t *testing.T) {
	public := func(p string) bool { return p == "debian-cloud" }
	body := `{"name":"vm","tags":{"items":["web"]},"labels":{"a":"b"},"deletionProtection":true,
		"metadata":{"items":[{"key":"k","value":"v"}]},
		"networkInterfaces":[{"network":"global/networks/net","subnetwork":"https://compute.googleapis.com/compute/v1/projects/host/regions/us-central1/subnetworks/sub","accessConfigs":[{"type":"ONE_TO_ONE_NAT"}]}],
		"disks":[{"boot":true,"initializeParams":{"sourceImage":"projects/debian-cloud/global/images/family/debian-12","labels":{"x":"y"}}},
		         {"source":"zones/us-central1-a/disks/data","mode":"READ_ONLY"},
		         {"initializeParams":{"sourceImage":"global/images/mine","sourceSnapshot":"snap","diskName":"d2"}}],
		"serviceAccounts":[{"email":"default"},{"email":"run@other.iam.gserviceaccount.com"}]}`
	got := ComputeFieldChecks("POST", "/compute/v1/projects/p/zones/us-central1-a/instances", []byte(body), public)
	want := []FieldCheck{
		{Permission: "compute.networks.use", Resource: "projects/p/global/networks/net"},
		{Permission: "compute.networks.useExternalIp", Resource: "projects/p/global/networks/net"},
		{Permission: "compute.subnetworks.use", Resource: "projects/host/regions/us-central1/subnetworks/sub"},
		{Permission: "compute.subnetworks.useExternalIp", Resource: "projects/host/regions/us-central1/subnetworks/sub"},
		{Permission: "compute.disks.setLabels", Resource: "projects/p/zones/us-central1-a/disks/vm"},
		{Permission: "compute.disks.useReadOnly", Resource: "projects/p/zones/us-central1-a/disks/data"},
		{Permission: "compute.images.useReadOnly", Resource: "projects/p/global/images/mine"},
		{Permission: "compute.snapshots.useReadOnly", Resource: "projects/p/global/snapshots/snap"},
		{Permission: "compute.disks.create", Resource: "projects/p"},
		{Permission: "compute.instances.setTags", Resource: "projects/p/zones/us-central1-a/instances/vm"},
		{Permission: "compute.instances.setMetadata", Resource: "projects/p/zones/us-central1-a/instances/vm"},
		{Permission: "compute.instances.setLabels", Resource: "projects/p/zones/us-central1-a/instances/vm"},
		{Permission: "compute.instances.setDeletionProtection", Resource: "projects/p/zones/us-central1-a/instances/vm"},
		{Permission: "compute.instances.setServiceAccount", Resource: "projects/p/zones/us-central1-a/instances/vm"},
		{Permission: ActAs, Resource: "p-compute@developer.gserviceaccount.com", ActAs: true, Project: "p"},
		{Permission: ActAs, Resource: "run@other.iam.gserviceaccount.com", ActAs: true, Project: "p"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("instances.insert:\n got %+v\nwant %+v", got, want)
	}
	names := iamroles.Names()
	for _, c := range want {
		if _, ok := slices.BinarySearch(names, c.Permission); !ok {
			t.Errorf("%s is not in Google's permission index", c.Permission)
		}
	}
	for _, c := range []struct {
		path, body string
		want       []FieldCheck
	}{
		{"/compute/v1/projects/p/zones/z/instances/vm/attachDisk", `{"source":"projects/p/zones/z/disks/d"}`,
			[]FieldCheck{{Permission: "compute.disks.use", Resource: "projects/p/zones/z/disks/d"}}},
		{"/compute/v1/projects/p/zones/z/disks", `{"name":"d","sourceImage":"projects/debian-cloud/global/images/debian-12-x","sourceDisk":"other"}`,
			[]FieldCheck{{Permission: "compute.disks.useReadOnly", Resource: "projects/p/zones/z/disks/other"}}},
		{"/compute/v1/projects/p/regions/r/subnetworks", `{"name":"s","network":"global/networks/n"}`,
			[]FieldCheck{{Permission: "compute.networks.updatePolicy", Resource: "projects/p/global/networks/n"}}},
		{"/compute/v1/projects/p/global/firewalls", `{"name":"f","network":"projects/p/global/networks/n"}`,
			[]FieldCheck{{Permission: "compute.networks.updatePolicy", Resource: "projects/p/global/networks/n"}}},
		{"/compute/v1/projects/p/global/routes", `{"name":"f","network":"n"}`,
			[]FieldCheck{{Permission: "compute.networks.updatePolicy", Resource: "projects/p/global/networks/n"}}},
		{"/compute/v1/projects/p/regions/r/addresses", `{"name":"a","subnetwork":"s","labels":{"k":"v"}}`,
			[]FieldCheck{{Permission: "compute.subnetworks.use", Resource: "projects/p/regions/r/subnetworks/s"},
				{Permission: "compute.addresses.setLabels", Resource: "projects/p/regions/r/addresses/a"}}},
		{"/compute/v1/projects/p/zones/z/instances", `{"name":"vm"}`, nil},
	} {
		if got := ComputeFieldChecks("POST", c.path, []byte(c.body), public); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.path, got, c.want)
		}
	}
	for _, p := range []string{APIKeysLookup, ServiceUsageOperationsGet} {
		if _, ok := slices.BinarySearch(names, p); !ok {
			t.Errorf("%s is not in Google's permission index", p)
		}
	}
}

// TestRoundEightAITables (#1879): every row of the AI tables names a
// served method's string field and a permission in Google's index, and
// the JSON tables match the paths they are for.
func TestRoundEightAITables(t *testing.T) {
	names := iamroles.Names()
	known := func(p string) bool { _, ok := slices.BinarySearch(names, p); return ok }
	for _, svc := range []*Service{VertexAI, DocumentAI, Speech} {
		for full, m := range svc.Methods {
			service, method, _ := strings.Cut(strings.TrimPrefix(full, "/"), "/")
			if Services[service] != svc {
				t.Errorf("%s: service %s is not registered to its table", full, service)
			}
			d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
			if err != nil {
				t.Errorf("%s: no service %s", full, service)
				continue
			}
			md := d.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(method))
			if md == nil {
				t.Errorf("%s: no such method", full)
				continue
			}
			paths := []string{m.Field}
			for _, a := range m.Also {
				paths = append(paths, a.Field)
				if !known(a.Permission) {
					t.Errorf("%s: %s is not in Google's permission index", full, a.Permission)
				}
			}
			for _, p := range paths {
				if !hasPath(md.Input(), strings.Split(p, ".")) {
					t.Errorf("%s: request has no string field %s", full, p)
				}
			}
			for _, r := range []string{"projects/p/locations/l/processors/x", "projects/p/locations/l/processors/x/processorVersions/v"} {
				if p := m.PermissionOf(r); !known(p) {
					t.Errorf("%s: %q is not in Google's permission index", full, p)
				}
			}
		}
	}
	for _, table := range [][]JSONMethod{AIJSON, SpeechJSON, TranslationJSON} {
		for _, m := range table {
			if !known(m.Permission) {
				t.Errorf("%s: %s is not in Google's permission index", m.Pattern, m.Permission)
			}
		}
	}
	for _, c := range []struct {
		table                []JSONMethod
		path, perm, resource string
	}{
		{AIJSON, "/v1beta1/projects/p/locations/us-central1/publishers/google/models/gemini:generateContent", "aiplatform.endpoints.predict",
			"projects/p/locations/us-central1/publishers/google/models/gemini"},
		{AIJSON, "/v1/projects/p/locations/l/endpoints/e:predict", "aiplatform.endpoints.predict", "projects/p/locations/l/endpoints/e"},
		{AIJSON, "/v1/projects/p/locations/l/endpoints/e:explain", "aiplatform.endpoints.explain", "projects/p/locations/l/endpoints/e"},
		{AIJSON, "/v1beta1/projects/p/locations/l/endpoints/e/chat/completions", "aiplatform.endpoints.predict", "projects/p/locations/l/endpoints/e"},
		{SpeechJSON, "/v2/projects/p/locations/global/recognizers/_:recognize", "speech.recognizers.recognize", "projects/p/locations/global/recognizers/_"},
		{TranslationJSON, "/v3/projects/p/locations/global:translateText", "cloudtranslate.generalModels.predict", "projects/p/locations/global"},
		{TranslationJSON, "/v3/projects/p/locations/global:detectLanguage", "cloudtranslate.languageDetectionModels.predict", "projects/p/locations/global"},
	} {
		perm, res, ok := MatchJSON(c.table, "POST", c.path)
		if !ok || perm != c.perm || res != c.resource {
			t.Errorf("%s = %s on %s (%v); want %s on %s", c.path, perm, res, ok, c.perm, c.resource)
		}
	}
	if p := DocumentAI.Methods["/google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument"]; p.PermissionOf("projects/p/locations/l/processors/x/processorVersions/v") != "documentai.processorVersions.processOnline" {
		t.Error("ProcessDocument on a processor version is not documentai.processorVersions.processOnline")
	}
}
