package iameval

import (
	"context"
	"slices"
	"strings"
	"testing"

	_ "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	_ "google.golang.org/genproto/googleapis/devtools/containeranalysis/v1"
	gpb "google.golang.org/genproto/googleapis/grafeas/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestRound12SourceTables (#1879): every row of the Secure Source
// Manager, Developer Connect and Container Analysis tables names a method
// its service has, string request fields it has and permissions Google's
// index names; every method of those services has a row, but those whose
// pages need no permission and Developer Connect's unserved ones.
func TestRound12SourceTables(t *testing.T) {
	names := iamroles.Names()
	inIndex := func(full, p string) {
		if _, ok := slices.BinarySearch(names, p); !ok {
			t.Errorf("%s: %s is not in Google's permission index", full, p)
		}
	}
	unchecked := map[string]bool{SSMTestIamPermissionsRepo: true}
	for _, m := range []string{"CreateAccountConnector", "DeleteAccountConnector", "GetAccountConnector", "ListAccountConnectors",
		"UpdateAccountConnector", "DeleteSelf", "DeleteUser", "FetchAccessToken", "FetchSelf", "FinishOAuth", "ListUsers",
		"StartOAuth", "FetchGitHubInstallations"} {
		unchecked["/google.cloud.developerconnect.v1.DeveloperConnect/"+m] = true // not served (UNIMPLEMENTED)
	}
	tables := []*Service{SecureSourceManager, SecureSourceManagerJSON, DeveloperConnect, ContainerAnalysis}
	for name, svc := range Services {
		if svc != SecureSourceManager && svc != DeveloperConnect && svc != ContainerAnalysis {
			continue
		}
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			t.Errorf("no service %s", name)
			continue
		}
		ms := d.(protoreflect.ServiceDescriptor).Methods()
		for i := 0; i < ms.Len(); i++ {
			full := "/" + name + "/" + string(ms.Get(i).Name())
			if _, ok := svc.Methods[full]; !ok && !unchecked[full] && !strings.HasSuffix(full, "/TestIamPermissions") {
				t.Errorf("%s has no row", full)
			}
		}
	}
	for _, svc := range tables {
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
			paths := []string{m.Field}
			for _, a := range m.Also {
				paths = append(paths, a.Field)
				inIndex(full, a.Permission)
			}
			for _, p := range paths {
				if !hasPath(md.Input(), strings.Split(p, ".")) {
					t.Errorf("%s: request has no string field %s", full, p)
				}
			}
			switch {
			case m.Permission != "":
				inIndex(full, m.Permission)
			case m.PermissionFor != nil:
				for _, r := range []string{"projects/p/notes/n", "projects/p/occurrences/o"} {
					inIndex(full, m.PermissionFor(r))
				}
			default:
				t.Errorf("%s checks nothing", full)
			}
		}
	}
	for _, j := range DeveloperConnectOpsJSON {
		inIndex(j.Pattern.String(), j.Permission)
	}
	for _, p := range RunPortListOperations {
		inIndex("RunPortListOperations", p)
	}
}

// TestRound12SourceEnforcer: a repository's own policy grants its issues,
// an instance's does not reach its repositories; an occurrence's create
// needs attachOccurrence on its note; TestIamPermissionsRepo answers what
// the caller holds; the owner passes.
func TestRound12SourceEnforcer(t *testing.T) {
	const user = "u@p.iam.gserviceaccount.com"
	member := []string{"serviceAccount:" + user}
	repo, inst := "projects/p/locations/l/repositories/r", "projects/p/locations/l/instances/i"
	own := map[string]*iampolicy.Stored{
		repo:                    pol(iampolicy.Binding{Role: "roles/securesourcemanager.repoReader", Members: member}),
		inst:                    pol(iampolicy.Binding{Role: "roles/securesourcemanager.instanceManager", Members: member}),
		"projects/prov/notes/n": pol(iampolicy.Binding{Role: "roles/containeranalysis.notes.attacher", Members: member}),
	}
	e := &Enforcer{Eval: &Evaluator{Source: fakeSource{policies: map[string]*iampolicy.Stored{
		"projects/p": pol(iampolicy.Binding{Role: "roles/containeranalysis.occurrences.editor", Members: member}),
	}}}, On: func() bool { return true },
		CallerOf: func(b string) Caller {
			if b == "owner" {
				return Caller{Email: "owner@p.iam.gserviceaccount.com", Owner: true}
			}
			return Caller{Email: user}
		},
		Policies: map[string]PolicyGetter{
			SecureSourceManager.Name: func(_ context.Context, r string) *iampolicy.Stored { return own[r] },
			ContainerAnalysis.Name:   func(_ context.Context, r string) *iampolicy.Stored { return own[r] },
		},
	}
	ctx := incomingBearer("user")
	ok := func(context.Context, any) (any, error) { return nil, nil }
	ssm := e.Interceptor(SecureSourceManager)
	at := func(m string) *grpc.UnaryServerInfo {
		return &grpc.UnaryServerInfo{FullMethod: "/google.cloud.securesourcemanager.v1.SecureSourceManager/" + m}
	}
	if _, err := ssm(ctx, &ssmpb.GetIssueRequest{Name: repo + "/issues/1"}, at("GetIssue"), ok); err != nil {
		t.Errorf("a repoReader's GetIssue: %v", err)
	}
	if _, err := ssm(ctx, &ssmpb.CreateIssueRequest{Parent: repo}, at("CreateIssue"), ok); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a repoReader's CreateIssue: %v", err)
	}
	other := "projects/p/locations/l/repositories/other"
	if _, err := ssm(ctx, &ssmpb.GetRepositoryRequest{Name: other}, at("GetRepository"), ok); status.Code(err) != codes.PermissionDenied {
		t.Errorf("an instance manager's GetRepository of a repository not its: %v", err)
	}
	if _, err := ssm(ctx, &ssmpb.GetInstanceRequest{Name: inst}, at("GetInstance"), ok); err != nil {
		t.Errorf("an instance manager's GetInstance: %v", err)
	}
	if _, err := ssm(incomingBearer("owner"), &ssmpb.DeleteRepositoryRequest{Name: other}, at("DeleteRepository"), ok); err != nil {
		t.Errorf("owner: %v", err)
	}
	held := func(context.Context, any) (any, error) {
		return &iampb.TestIamPermissionsResponse{Permissions: []string{"securesourcemanager.repositories.get", "securesourcemanager.repositories.delete"}}, nil
	}
	resp, err := e.TestPermissionsGRPC(ctx, SecureSourceManager, &iampb.TestIamPermissionsRequest{Resource: repo}, held)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.(*iampb.TestIamPermissionsResponse).GetPermissions(); !slices.Equal(got, []string{"securesourcemanager.repositories.get"}) {
		t.Errorf("TestIamPermissionsRepo = %v", got)
	}

	ca := e.Interceptor(ContainerAnalysis)
	create := &grpc.UnaryServerInfo{FullMethod: "/grafeas.v1.Grafeas/CreateOccurrence"}
	if _, err := ca(ctx, &gpb.CreateOccurrenceRequest{Parent: "projects/p", Occurrence: &gpb.Occurrence{NoteName: "projects/prov/notes/n"}}, create, ok); err != nil {
		t.Errorf("an occurrence on an attachable note: %v", err)
	}
	_, err = ca(ctx, &gpb.CreateOccurrenceRequest{Parent: "projects/p", Occurrence: &gpb.Occurrence{NoteName: "projects/prov/notes/m"}}, create, ok)
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "containeranalysis.notes.attachOccurrence") {
		t.Errorf("an occurrence on another note: %v", err)
	}
	batch := &grpc.UnaryServerInfo{FullMethod: "/grafeas.v1.Grafeas/BatchCreateOccurrences"}
	if _, err := ca(ctx, &gpb.BatchCreateOccurrencesRequest{Parent: "projects/p", Occurrences: []*gpb.Occurrence{
		{NoteName: "projects/prov/notes/n"}, {NoteName: "projects/prov/notes/m"}}}, batch, ok); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a batch with an unattachable note: %v", err)
	}
	getPol := &grpc.UnaryServerInfo{FullMethod: "/google.devtools.containeranalysis.v1.ContainerAnalysis/GetIamPolicy"}
	_, err = ca(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/prov/notes/n"}, getPol, ok)
	if !strings.Contains(status.Convert(err).Message(), "containeranalysis.notes.getIamPolicy") {
		t.Errorf("a note's GetIamPolicy: %v", err)
	}
	if svc, get, list, del := RunPortOperation("type.googleapis.com/google.cloud.developerconnect.v1.OperationMetadata"); svc != DeveloperConnect ||
		get != "developerconnect.operations.get" || list != "developerconnect.operations.list" || del != "developerconnect.operations.delete" {
		t.Errorf("Developer Connect's operation = %v %s %s %s", svc, get, list, del)
	}
	if svc, _, _, _ := RunPortOperation("type.googleapis.com/google.cloud.securesourcemanager.v1.OperationMetadata"); svc != nil {
		t.Error("Secure Source Manager's operations are checked; their pages name no permission")
	}
}
