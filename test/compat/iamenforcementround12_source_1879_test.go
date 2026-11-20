//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	containeranalysis "cloud.google.com/go/containeranalysis/apiv1"
	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	ssmrest "google.golang.org/api/securesourcemanager/v1"
	gpb "google.golang.org/genproto/googleapis/grafeas/v1"
)

// grantOwn adds role for member to a resource's own policy through get
// and set (a repository's, a note's), as the instance's own account.
func grantOwn(t *testing.T, ctx context.Context, resource, role, member string,
	get func(context.Context, *iampb.GetIamPolicyRequest) (*iampb.Policy, error),
	set func(context.Context, *iampb.SetIamPolicyRequest) (*iampb.Policy, error)) {
	t.Helper()
	pol, err := get(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
	if err != nil {
		t.Fatalf("GetIamPolicy %s: %v", resource, err)
	}
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{member}})
	if _, err := set(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol}); err != nil {
		t.Fatalf("SetIamPolicy %s: %v", resource, err)
	}
}

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/GetRepository, google.cloud.securesourcemanager.v1.SecureSourceManager/ListIssues, google.cloud.securesourcemanager.v1.SecureSourceManager/CreateIssue, google.cloud.securesourcemanager.v1.SecureSourceManager/GetInstance, google.cloud.securesourcemanager.v1.SecureSourceManager/GetIamPolicyRepo, google.cloud.securesourcemanager.v1.SecureSourceManager/SetIamPolicyRepo, google.cloud.securesourcemanager.v1.SecureSourceManager/TestIamPermissionsRepo
//
// TestIAMEnforcementSecureSourceManager (#1879), with the official Secure
// Source Manager client over gRPC and its REST client over JSON, on the
// Cloud Run port, with a minted token: without a role, GetRepository
// (securesourcemanager.repositories.get), GetInstance and the instance's
// getIamPolicy are refused; Repository Reader on the repository's own
// policy lets GetRepository and ListIssues through but not CreateIssue,
// and TestIamPermissionsRepo answers only what it holds; Secure Source
// Manager Viewer on the project lets GetInstance and the instance's
// getIamPolicy through. The instance's own account is never refused.
func TestIAMEnforcementSecureSourceManager(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iessm12-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iessm12")
	owner := ssmClient(t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	iop, err := owner.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i-" + suffix})
	if err != nil {
		t.Fatalf("the instance's own account: CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := owner.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := owner.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("the instance's own account: CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	as, err := securesourcemanager.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	rest, err := ssmrest.NewService(ctx, option.WithEndpoint("http://"+stripScheme(endpoint)+"/securesourcemanager/"),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}))
	if err != nil {
		t.Fatal(err)
	}
	getRepo := func() error {
		_, err := as.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo.GetName()})
		return err
	}
	listIssues := func() error {
		_, err := as.ListIssues(ctx, &ssmpb.ListIssuesRequest{Parent: repo.GetName()}).Next()
		return first(err)
	}
	createIssue := func() error {
		_, err := as.CreateIssue(ctx, &ssmpb.CreateIssueRequest{Parent: repo.GetName(), Issue: &ssmpb.Issue{Title: "t"}})
		return err
	}
	getInst := func() error {
		_, err := as.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: inst.GetName()})
		return err
	}
	instPolicy := func() error {
		_, err := rest.Projects.Locations.Instances.GetIamPolicy(inst.GetName()).Context(ctx).Do()
		return err
	}
	if _, err := owner.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo.GetName()}); err != nil {
		t.Fatalf("the instance's own account: GetRepository: %v", err)
	}
	wantDenied(t, "GetRepository without a role", getRepo(), newerDenied("securesourcemanager.repositories.get", repo.GetName()),
		"securesourcemanager.repositories.get", true)
	wantDenied(t, "CreateIssue without a role", createIssue(), newerDenied("securesourcemanager.issues.create", repo.GetName()),
		"securesourcemanager.issues.create", true)
	wantDenied(t, "GetInstance without a role", getInst(), newerDenied("securesourcemanager.instances.get", inst.GetName()),
		"securesourcemanager.instances.get", true)
	var gerr *googleapi.Error
	if err := instPolicy(); !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden ||
		!strings.Contains(gerr.Message, "securesourcemanager.instances.getIamPolicy") {
		t.Fatalf("the instance's getIamPolicy over JSON without a role = %v; want 403", err)
	}

	grantOwn(t, ctx, repo.GetName(), "roles/securesourcemanager.repoReader", "serviceAccount:"+p.email,
		func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			return owner.GetIamPolicyRepo(ctx, r)
		},
		func(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
			return owner.SetIamPolicyRepo(ctx, r)
		})
	waitGranted(t, "GetRepository as the repository's reader", getRepo)
	if err := listIssues(); err != nil {
		t.Errorf("ListIssues as the repository's reader: %v", err)
	}
	wantDenied(t, "CreateIssue as the repository's reader", createIssue(), newerDenied("securesourcemanager.issues.create", repo.GetName()),
		"securesourcemanager.issues.create", true)
	held, err := as.TestIamPermissionsRepo(ctx, &iampb.TestIamPermissionsRequest{Resource: repo.GetName(),
		Permissions: []string{"securesourcemanager.repositories.get", "securesourcemanager.issues.create"}})
	if err != nil || !slices.Equal(held.GetPermissions(), []string{"securesourcemanager.repositories.get"}) {
		t.Errorf("TestIamPermissionsRepo as the repository's reader = %v, %v; want repositories.get alone", held, err)
	}
	if err := getInst(); err == nil {
		t.Error("GetInstance as the repository's reader passed: a repository's role reached its instance")
	}

	p.grant("roles/securesourcemanager.viewer")
	waitGranted(t, "GetInstance as a Viewer", getInst)
	if err := instPolicy(); err != nil {
		t.Errorf("the instance's getIamPolicy over JSON as a Viewer: %v", err)
	}
}

// covers: google.cloud.developerconnect.v1.DeveloperConnect/CreateConnection, google.cloud.developerconnect.v1.DeveloperConnect/GetConnection, google.cloud.developerconnect.v1.DeveloperConnect/ListConnections, google.cloud.developerconnect.v1.DeveloperConnect/DeleteConnection
//
// TestIAMEnforcementDeveloperConnect (#1879), with the official Developer
// Connect client over gRPC on the Cloud Run port, with a minted token:
// without a role, GetConnection (developerconnect.connections.get),
// ListConnections and the connection's create operation
// (developerconnect.operations.get) are refused; Developer Connect Viewer
// on the project lets them through but not DeleteConnection. The
// instance's own account is never refused.
func TestIAMEnforcementDeveloperConnect(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iedc12-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iedc12")
	ssm := ssmClient(t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "dc-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	owner, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	cop, err := owner.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: "ssm-" + suffix,
		Connection: &dcpb.Connection{ConnectionConfig: &dcpb.Connection_SecureSourceManagerInstanceConfig{
			SecureSourceManagerInstanceConfig: &dcpb.SecureSourceManagerInstanceConfig{Instance: inst.GetName()}}}})
	if err != nil {
		t.Fatalf("the instance's own account: CreateConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := owner.DeleteConnection(context.Background(), &dcpb.DeleteConnectionRequest{Name: conn.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	as, err := developerconnect.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	get := func() error {
		_, err := as.GetConnection(ctx, &dcpb.GetConnectionRequest{Name: conn.GetName()})
		return err
	}
	list := func() error {
		_, err := as.ListConnections(ctx, &dcpb.ListConnectionsRequest{Parent: parent}).Next()
		return first(err)
	}
	getOp := func() error {
		_, err := as.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: cop.Name()})
		return err
	}
	del := func() error {
		_, err := as.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: conn.GetName(), ValidateOnly: true})
		return err
	}
	wantDenied(t, "GetConnection without a role", get(), newerDenied("developerconnect.connections.get", conn.GetName()),
		"developerconnect.connections.get", true)
	wantDenied(t, "ListConnections without a role", list(), newerDenied("developerconnect.connections.list", parent),
		"developerconnect.connections.list", true)
	wantDenied(t, "GetOperation without a role", getOp(), newerDenied("developerconnect.operations.get", cop.Name()),
		"developerconnect.operations.get", true)

	p.grant("roles/developerconnect.viewer")
	waitGranted(t, "GetConnection as a Viewer", get)
	if err := list(); err != nil {
		t.Errorf("ListConnections as a Viewer: %v", err)
	}
	if err := getOp(); err != nil {
		t.Errorf("GetOperation as a Viewer: %v", err)
	}
	wantDenied(t, "DeleteConnection as a Viewer", del(), newerDenied("developerconnect.connections.delete", conn.GetName()),
		"developerconnect.connections.delete", true)
	if _, err := owner.GetConnection(ctx, &dcpb.GetConnectionRequest{Name: conn.GetName()}); err != nil {
		t.Errorf("the instance's own account: GetConnection: %v", err)
	}
}

// covers: grafeas.v1.Grafeas/CreateNote, grafeas.v1.Grafeas/GetNote, grafeas.v1.Grafeas/CreateOccurrence, grafeas.v1.Grafeas/DeleteOccurrence, google.devtools.containeranalysis.v1.ContainerAnalysis/GetIamPolicy, google.devtools.containeranalysis.v1.ContainerAnalysis/SetIamPolicy, google.devtools.containeranalysis.v1.ContainerAnalysis/TestIamPermissions
//
// TestIAMEnforcementContainerAnalysis (#1879), with the official
// containeranalysis and grafeas clients over gRPC on the Cloud Run port,
// with a minted token: without a role, GetNote
// (containeranalysis.notes.get) and the note's GetIamPolicy are refused;
// Container Analysis Occurrences Editor on the project is not enough to
// attach an occurrence to the note (containeranalysis.notes.attachOccurrence),
// which Notes Attacher on the note's own policy then grants, with GetNote;
// TestIamPermissions answers what the caller holds. The instance's own
// account is never refused.
func TestIAMEnforcementContainerAnalysis(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieca12-")
	parent := "projects/" + project
	p := newIAMPrincipal(t, h, project, "ieca12")
	ownerCA, err := containeranalysis.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownerCA.Close() })
	owner := ownerCA.GetGrafeasClient()
	n, err := owner.CreateNote(ctx, &gpb.CreateNoteRequest{Parent: parent, NoteId: "attestor", Note: &gpb.Note{
		Type: &gpb.Note_Attestation{Attestation: &gpb.AttestationNote{Hint: &gpb.AttestationNote_Hint{HumanReadableName: "compat"}}}}})
	if err != nil {
		t.Fatalf("the instance's own account: CreateNote: %v", err)
	}
	t.Cleanup(func() { _ = owner.DeleteNote(context.Background(), &gpb.DeleteNoteRequest{Name: n.GetName()}) })
	asCA, err := containeranalysis.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = asCA.Close() })
	as := asCA.GetGrafeasClient()
	getNote := func() error { _, err := as.GetNote(ctx, &gpb.GetNoteRequest{Name: n.GetName()}); return err }
	occ := &gpb.Occurrence{ResourceUri: "https://us-central1-docker.pkg.dev/" + project + "/r/img@sha256:" + strings.Repeat("a", 64),
		NoteName: n.GetName(), Details: &gpb.Occurrence_Attestation{Attestation: &gpb.AttestationOccurrence{SerializedPayload: []byte("p")}}}
	var created *gpb.Occurrence
	create := func() error {
		o, err := as.CreateOccurrence(ctx, &gpb.CreateOccurrenceRequest{Parent: parent, Occurrence: occ})
		if err == nil {
			created = o
		}
		return err
	}
	wantDenied(t, "GetNote without a role", getNote(), newerDenied("containeranalysis.notes.get", n.GetName()),
		"containeranalysis.notes.get", true)
	_, err = asCA.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: n.GetName()})
	wantDenied(t, "the note's GetIamPolicy without a role", err, newerDenied("containeranalysis.notes.getIamPolicy", n.GetName()),
		"containeranalysis.notes.getIamPolicy", true)

	p.grant("roles/containeranalysis.occurrences.editor")
	wantDenied(t, "CreateOccurrence as an Occurrences Editor", create(), newerDenied("containeranalysis.notes.attachOccurrence", n.GetName()),
		"containeranalysis.notes.attachOccurrence", true)
	grantOwn(t, ctx, n.GetName(), "roles/containeranalysis.notes.attacher", "serviceAccount:"+p.email,
		func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
			return ownerCA.GetIamPolicy(ctx, r)
		},
		func(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
			return ownerCA.SetIamPolicy(ctx, r)
		})
	waitGranted(t, "CreateOccurrence as the note's attacher", create)
	t.Cleanup(func() {
		_ = owner.DeleteOccurrence(context.Background(), &gpb.DeleteOccurrenceRequest{Name: created.GetName()})
	})
	if err := getNote(); err != nil {
		t.Errorf("GetNote as the note's attacher: %v", err)
	}
	held, err := asCA.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: n.GetName(),
		Permissions: []string{"containeranalysis.notes.attachOccurrence", "containeranalysis.notes.delete"}})
	if err != nil || !slices.Equal(held.GetPermissions(), []string{"containeranalysis.notes.attachOccurrence"}) {
		t.Errorf("TestIamPermissions as the note's attacher = %v, %v; want attachOccurrence alone", held, err)
	}
	if err := as.DeleteOccurrence(ctx, &gpb.DeleteOccurrenceRequest{Name: created.GetName()}); err != nil {
		t.Errorf("DeleteOccurrence as an Occurrences Editor: %v", err)
	}
}
