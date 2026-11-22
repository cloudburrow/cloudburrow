package iameval

import (
	"context"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	_ "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "cloud.google.com/go/longrunning/autogen/longrunningpb"
	_ "google.golang.org/genproto/googleapis/cloud/location"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestRoundSevenTables (#1879): every row of the seventh round's tables
// names a served method's string fields and a permission in Google's
// index, and the JSON tables match the paths they are for.
func TestRoundSevenTables(t *testing.T) {
	names := iamroles.Names()
	known := func(p string) bool { _, ok := slices.BinarySearch(names, p); return ok }
	for _, svc := range []*Service{PrivateCA, GKE} {
		for full, m := range svc.Methods {
			service, method, _ := strings.Cut(strings.TrimPrefix(full, "/"), "/")
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
			paths := append(strings.Split(m.Field, "|"), m.ActAs...)
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
			perm := m.Permission
			if m.PermissionFor != nil {
				for _, r := range []string{"projects/p/locations/l/caPools/c", "projects/p/locations/l/certificateTemplates/t",
					"projects/p/locations/l/caPools/c/certificateAuthorities/a/certificateRevocationLists/x"} {
					if p := m.PermissionFor(r); !known(p) {
						t.Errorf("%s: %s is not in Google's permission index", full, p)
					}
				}
				continue
			}
			if !known(perm) {
				t.Errorf("%s: %q is not in Google's permission index", full, perm)
			}
		}
	}
	if m := Run.Methods["/google.cloud.run.v2.SourceUpload/UploadSource"]; !known(m.Permission) {
		t.Errorf("UploadSource's %s is not in the index", m.Permission)
	}
	for _, p := range append(append([]string{}, RunPortListOperations...), RunPortListLocations...) {
		if !known(p) {
			t.Errorf("%s is not in the index", p)
		}
	}
	for _, typeURL := range []string{"type.googleapis.com/google.cloud.run.v2.Service", "type.googleapis.com/google.cloud.functions.v2.OperationMetadata",
		"type.googleapis.com/google.devtools.cloudbuild.v1.BuildOperationMetadata"} {
		svc, get, list, del := RunPortOperation(typeURL)
		if svc == nil {
			t.Errorf("%s: no API", typeURL)
		}
		for _, p := range []string{get, list, del} {
			if p != "" && !known(p) {
				t.Errorf("%s: %s is not in the index", typeURL, p)
			}
		}
	}
	if svc, _, _, _ := RunPortOperation("type.googleapis.com/google.devtools.artifactregistry.v1.OperationMetadata"); svc != nil {
		t.Error("Artifact Registry's operations are checked")
	}
	for name, table := range map[string][]JSONMethod{"compute": ComputeJSON, "apikeys": APIKeysJSON, "serviceusage": ServiceUsageJSON,
		"ar locations": ArtifactRegistryLocationsJSON, "upload": RunUploadSourceJSON} {
		for _, m := range table {
			if !known(m.Permission) {
				t.Errorf("%s %s: %s is not in Google's permission index", name, m.Pattern, m.Permission)
			}
		}
	}
	for _, c := range []struct {
		table                        []JSONMethod
		method, path, perm, resource string
	}{
		{ComputeJSON, "GET", "/compute/v1/projects/p/zones/z/instances", "compute.instances.list", "projects/p"},
		{ComputeJSON, "POST", "/compute/v1/projects/p/zones/z/instances", "compute.instances.create", "projects/p"},
		{ComputeJSON, "GET", "/compute/v1/projects/p/zones/z/instances/i", "compute.instances.get", "projects/p/zones/z/instances/i"},
		{ComputeJSON, "POST", "/compute/v1/projects/p/zones/z/instances/i/stop", "compute.instances.stop", "projects/p/zones/z/instances/i"},
		{ComputeJSON, "GET", "/compute/v1/projects/p/zones/z/instances/i/serialPort", "compute.instances.getSerialPortOutput", "projects/p/zones/z/instances/i"},
		{ComputeJSON, "GET", "/compute/v1/projects/p/zones/z/instances/i/getIamPolicy", "compute.instances.getIamPolicy", "projects/p/zones/z/instances/i"},
		{ComputeJSON, "POST", "/compute/v1/projects/p/regions/r/subnetworks/s/setIamPolicy", "compute.subnetworks.setIamPolicy", "projects/p/regions/r/subnetworks/s"},
		{ComputeJSON, "POST", "/compute/v1/projects/p/global/snapshots/s/setIamPolicy", "compute.snapshots.setIamPolicy", "projects/p/global/snapshots/s"},
		{ComputeJSON, "GET", "/compute/v1/projects/p/aggregated/instances", "compute.instances.list", "projects/p"},
		{ComputeJSON, "PATCH", "/compute/v1/projects/p/global/networks/n", "compute.networks.update", "projects/p/global/networks/n"},
		{ComputeJSON, "GET", "/compute/v1/projects/p/global/addresses", "compute.globalAddresses.list", "projects/p"},
		{ComputeJSON, "POST", "/compute/v1/projects/p/global/operations/o/wait", "compute.globalOperations.get", "projects/p/global/operations/o"},
		{ComputeJSON, "GET", "/compute/v1/projects/p/global/images/family/debian-12", "compute.images.getFromFamily", "projects/p/global/images/family/debian-12"},
		{ComputeJSON, "GET", "/compute/v1/projects/p", "compute.projects.get", "projects/p"},
		{APIKeysJSON, "GET", "/v2/projects/p/locations/global/keys/k/keyString", "apikeys.keys.getKeyString", "projects/p/locations/global/keys/k"},
		{APIKeysJSON, "POST", "/v2/projects/p/locations/global/keys/k:undelete", "apikeys.keys.undelete", "projects/p/locations/global/keys/k"},
		{ServiceUsageJSON, "POST", "/v1/projects/p/services/pubsub.googleapis.com:enable", "serviceusage.services.enable", "projects/p/services/pubsub.googleapis.com"},
		{ServiceUsageJSON, "GET", "/v1/projects/p/services", "serviceusage.services.list", "projects/p"},
		{ServiceUsageJSON, "POST", "/v1/projects/p/services:batchEnable", "serviceusage.services.enable", "projects/p"},
	} {
		perm, res, ok := MatchJSON(c.table, c.method, c.path)
		if !ok || perm != c.perm || res != c.resource {
			t.Errorf("%s %s = %s on %s (%v); want %s on %s", c.method, c.path, perm, res, ok, c.perm, c.resource)
		}
	}
}

// TestPrivateCATemplateUse (#1879): CreateCertificate naming a template
// also needs privateca.certificateTemplates.use on that template.
func TestPrivateCATemplateUse(t *testing.T) {
	requester := iampolicy.Binding{Role: "roles/privateca.certificateRequester", Members: []string{"serviceAccount:" + sa}}
	src := fakeSource{policies: map[string]*iampolicy.Stored{"projects/p": pol(requester)}}
	e := &Enforcer{Eval: &Evaluator{Source: src}, On: func() bool { return true }, CallerOf: func(string) Caller { return Caller{Email: sa} }}
	call := func(req *privatecapb.CreateCertificateRequest) error {
		_, err := e.Interceptor(PrivateCA)(incomingBearer("x"), req, &grpc.UnaryServerInfo{FullMethod: "/google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate"},
			func(context.Context, any) (any, error) { return "ok", nil })
		return err
	}
	const pool = "projects/p/locations/l/caPools/c"
	if err := call(&privatecapb.CreateCertificateRequest{Parent: pool}); err != nil {
		t.Fatalf("CreateCertificate as a Certificate Requester = %v", err)
	}
	tmpl := "projects/p/locations/l/certificateTemplates/t"
	err := call(&privatecapb.CreateCertificateRequest{Parent: pool, Certificate: &privatecapb.Certificate{CertificateTemplate: tmpl}})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "privateca.certificateTemplates.use") {
		t.Fatalf("CreateCertificate with a template, without its use = %v", err)
	}
	own := pol(iampolicy.Binding{Role: "roles/privateca.templateUser", Members: []string{"serviceAccount:" + sa}})
	e.Policies = map[string]PolicyGetter{PrivateCA.Name: func(_ context.Context, r string) *iampolicy.Stored {
		if r == tmpl {
			return own
		}
		return nil
	}}
	if err := call(&privatecapb.CreateCertificateRequest{Parent: pool, Certificate: &privatecapb.Certificate{CertificateTemplate: tmpl}}); err != nil {
		t.Fatalf("CreateCertificate with a template its user = %v", err)
	}
}

// TestGKELegacyFields (#1879): a request in the deprecated form is
// checked on its project.
func TestGKELegacyFields(t *testing.T) {
	if got := fieldValues((&containerpb.ListClustersRequest{ProjectId: "p", Zone: "z"}).ProtoReflect(), "parent|project_id"); got[0] != "projects/p" {
		t.Fatalf("ListClusters' resource = %v", got)
	}
	if got := fieldValues((&containerpb.ListClustersRequest{Parent: "projects/p/locations/-"}).ProtoReflect(), "parent|project_id"); got[0] != "projects/p/locations/-" {
		t.Fatalf("ListClusters' resource = %v", got)
	}
}
