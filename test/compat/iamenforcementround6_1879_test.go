//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	functionsv1 "cloud.google.com/go/functions/apiv1"
	functionsv1pb "cloud.google.com/go/functions/apiv1/functionspb"
	functions "cloud.google.com/go/functions/apiv2"
	"cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	logrest "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// restAs are REST client options for endpoint calling with token.
func restAs(endpoint, token string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint("http://" + stripScheme(endpoint) + "/"),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}})}
}

// wantPast fails unless err is nil or a code other than PERMISSION_DENIED
// and UNAUTHENTICATED: the call passed its IAM check (NOT_FOUND of a
// resource that does not exist, say).
func wantPast(t *testing.T, what string, err error) {
	t.Helper()
	wantNotDenied(t, what, err)
	if status.Code(err) == codes.Unauthenticated {
		t.Fatalf("%s = %v", what, err)
	}
}

// first is the first item of an iterator's page, nil for none.
func first(err error) error {
	if errors.Is(err, iterator.Done) {
		return nil
	}
	return err
}

// TestIAMEnforcementRunAdmin (#1879), with the official Cloud Run v2
// clients and the v1 REST client (google.golang.org/api/run/v1): without a
// role, GetService and ListJobs are PERMISSION_DENIED with Google's newer
// wording and ErrorInfo, and the v1 API's services.list 403; Cloud Run
// Viewer on the project reads and lists (a missing service NOT_FOUND) but
// cannot create; a v1 create naming a service account the caller may not
// act as is refused in Cloud Run's words until Service Account User on
// it; the instance's own account lists.
func TestIAMEnforcementRunAdmin(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iera6-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iera6")
	sc, err := run.NewServicesClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	jc, err := run.NewJobsClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jc.Close() })
	name := parent + "/services/missing"
	get := func() error { _, err := sc.GetService(ctx, &runpb.GetServiceRequest{Name: name}); return err }
	jobs := func() error {
		_, err := jc.ListJobs(ctx, &runpb.ListJobsRequest{Parent: parent}).Next()
		return first(err)
	}
	wantDenied(t, "GetService without a role", get(), newerDenied("run.services.get", name), "run.services.get", true)
	wantDenied(t, "ListJobs without a role", jobs(), newerDenied("run.jobs.list", parent), "run.jobs.list", true)
	v1, err := runv1.NewService(ctx, restAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	v1List := func() error {
		_, err := v1.Namespaces.Services.List("namespaces/" + project).Context(ctx).Do()
		return err
	}
	if err := v1List(); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "run.services.list") {
		t.Fatalf("v1 services.list without a role = %v; want 403", err)
	}

	p.grant("roles/run.viewer")
	t.Cleanup(func() { p.revoke("roles/run.viewer") })
	if err := get(); status.Code(err) != codes.NotFound {
		t.Fatalf("GetService of a missing service as a Cloud Run Viewer = %v; want NOT_FOUND", err)
	}
	waitGranted(t, "ListJobs as a Cloud Run Viewer", jobs)
	waitGranted(t, "v1 services.list as a Cloud Run Viewer", v1List)
	_, err = sc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: "enforced", Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: "127.0.0.1:1/none/never:latest"}}}}})
	wantDenied(t, "CreateService as a Cloud Run Viewer", err, newerDenied("run.services.create", parent), "run.services.create", true)

	// The v1 API's create checks run.services.create and actAs as v2's.
	target := newServiceAccount(t, h, project, "runv1")
	p.grant("roles/run.developer")
	t.Cleanup(func() { p.revoke("roles/run.developer") })
	v1Create := func() error {
		_, err := v1.Namespaces.Services.Create("namespaces/"+project, &runv1.Service{ApiVersion: "serving.knative.dev/v1", Kind: "Service",
			Metadata: &runv1.ObjectMeta{Name: "enforced-v1", Labels: map[string]string{"cloud.googleapis.com/location": "us-central1"}},
			Spec: &runv1.ServiceSpec{Template: &runv1.RevisionTemplate{Spec: &runv1.RevisionSpec{ServiceAccountName: target.GetEmail(),
				Containers: []*runv1.Container{{Image: "127.0.0.1:1/none/never:latest"}}}}}}).DryRun("all").Context(ctx).Do()
		return err
	}
	err = v1Create()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden ||
		!strings.Contains(gerr.Message, "Permission 'iam.serviceaccounts.actAs' denied on service account "+target.GetEmail()) {
		t.Fatalf("v1 services.create without actAs = %v; want 403 in Cloud Run's words", err)
	}
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	if err := v1Create(); errors.As(err, &gerr) && gerr.Code == http.StatusForbidden {
		t.Fatalf("v1 services.create with actAs = %v", err)
	}

	owner, err := run.NewServicesClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.ListServices(ctx, &runpb.ListServicesRequest{Parent: parent}).Next(); first(err) != nil {
		t.Fatalf("ListServices as the instance's account: %v", err)
	}
}

// TestIAMEnforcementFunctionsAdmin (#1879), with the official Cloud
// Functions v2 and v1 clients: without a role ListFunctions is refused on
// both; Cloud Functions Viewer lists but cannot GenerateUploadUrl
// (cloudfunctions.functions.sourceCodeSet, as its reference names it);
// Cloud Functions Developer can; the instance's own account lists.
func TestIAMEnforcementFunctionsAdmin(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iefn6-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iefn6")
	fc, err := functions.NewFunctionClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	f1, err := functionsv1.NewCloudFunctionsClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f1.Close() })
	list := func() error {
		_, err := fc.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: parent}).Next()
		return first(err)
	}
	list1 := func() error {
		_, err := f1.ListFunctions(ctx, &functionsv1pb.ListFunctionsRequest{Parent: parent}).Next()
		return first(err)
	}
	upload := func() error {
		_, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
		return err
	}
	wantDenied(t, "ListFunctions without a role", list(), newerDenied("cloudfunctions.functions.list", parent), "cloudfunctions.functions.list", true)
	wantDenied(t, "1st gen ListFunctions without a role", list1(), newerDenied("cloudfunctions.functions.list", parent), "cloudfunctions.functions.list", true)
	p.grant("roles/cloudfunctions.viewer")
	t.Cleanup(func() { p.revoke("roles/cloudfunctions.viewer") })
	waitGranted(t, "ListFunctions as a Cloud Functions Viewer", list)
	waitGranted(t, "1st gen ListFunctions as a Cloud Functions Viewer", list1)
	wantDenied(t, "GenerateUploadUrl as a Cloud Functions Viewer", upload(),
		newerDenied("cloudfunctions.functions.sourceCodeSet", parent), "cloudfunctions.functions.sourceCodeSet", true)
	p.grant("roles/cloudfunctions.developer")
	t.Cleanup(func() { p.revoke("roles/cloudfunctions.developer") })
	for i := 0; i < 10; i++ {
		if err = upload(); status.Code(err) != codes.PermissionDenied {
			break
		}
	}
	wantPast(t, "GenerateUploadUrl as a Cloud Functions Developer", err)
	owner, err := functions.NewFunctionClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.ListFunctions(ctx, &functionspb.ListFunctionsRequest{Parent: parent}).Next(); first(err) != nil {
		t.Fatalf("ListFunctions as the instance's account: %v", err)
	}
}

// TestIAMEnforcementArtifactRegistry (#1879), with the official Artifact
// Registry client: without a role ListRepositories and GetRepository are
// refused; Artifact Registry Reader on the project lists and reads but
// cannot create; Reader on one repository's own policy reads that
// repository but cannot list the project's; the instance's own account
// creates the repository and sets its policy.
func TestIAMEnforcementArtifactRegistry(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ieag6-")
	parent := "projects/" + project + "/locations/us-central1"
	owner, err := artifactregistry.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	op, err := owner.CreateRepository(ctx, &artifactregistrypb.CreateRepositoryRequest{Parent: parent, RepositoryId: "enforced",
		Repository: &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository as the instance's account: %v", err)
	}
	repo, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository as the instance's account: %v", err)
	}
	t.Cleanup(func() {
		if op, err := owner.DeleteRepository(context.Background(), &artifactregistrypb.DeleteRepositoryRequest{Name: repo.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	p := newIAMPrincipal(t, h, project, "ieag6")
	ac, err := artifactregistry.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	list := func() error {
		_, err := ac.ListRepositories(ctx, &artifactregistrypb.ListRepositoriesRequest{Parent: parent}).Next()
		return first(err)
	}
	get := func() error {
		_, err := ac.GetRepository(ctx, &artifactregistrypb.GetRepositoryRequest{Name: repo.GetName()})
		return err
	}
	wantDenied(t, "ListRepositories without a role", list(), newerDenied("artifactregistry.repositories.list", parent), "artifactregistry.repositories.list", true)
	wantDenied(t, "GetRepository without a role", get(), newerDenied("artifactregistry.repositories.get", repo.GetName()), "artifactregistry.repositories.get", true)
	p.grant("roles/artifactregistry.reader")
	waitGranted(t, "ListRepositories as a Reader", list)
	waitGranted(t, "GetRepository as a Reader", get)
	_, err = ac.CreateRepository(ctx, &artifactregistrypb.CreateRepositoryRequest{Parent: parent, RepositoryId: "denied",
		Repository: &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}})
	wantDenied(t, "CreateRepository as a Reader", err, newerDenied("artifactregistry.repositories.create", parent), "artifactregistry.repositories.create", true)
	p.revoke("roles/artifactregistry.reader")
	undo := bindOn(t, owner.GetIamPolicy, owner.SetIamPolicy, repo.GetName(),
		&iampb.Binding{Role: "roles/artifactregistry.reader", Members: []string{"serviceAccount:" + p.email}})
	t.Cleanup(undo)
	waitGranted(t, "GetRepository as a Reader on the repository", get)
	wantDenied(t, "ListRepositories as a Reader on one repository", list(), newerDenied("artifactregistry.repositories.list", parent),
		"artifactregistry.repositories.list", true)
}

// TestIAMEnforcementCloudBuild (#1879), with the official Cloud Build
// client: without a role ListBuilds (on its project_id) and
// ListWorkerPools are refused; Cloud Build Viewer lists builds but cannot
// create one or list worker pools; the instance's own account lists.
func TestIAMEnforcementCloudBuild(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iecb6-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iecb6")
	bc, err := cloudbuild.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	builds := func() error {
		_, err := bc.ListBuilds(ctx, &cloudbuildpb.ListBuildsRequest{ProjectId: project}).Next()
		return first(err)
	}
	pools := func() error {
		_, err := bc.ListWorkerPools(ctx, &cloudbuildpb.ListWorkerPoolsRequest{Parent: parent}).Next()
		return first(err)
	}
	wantDenied(t, "ListBuilds without a role", builds(), newerDenied("cloudbuild.builds.list", "projects/"+project), "cloudbuild.builds.list", true)
	wantDenied(t, "ListWorkerPools without a role", pools(), newerDenied("cloudbuild.workerpools.list", parent), "cloudbuild.workerpools.list", true)
	p.grant("roles/cloudbuild.builds.viewer")
	t.Cleanup(func() { p.revoke("roles/cloudbuild.builds.viewer") })
	waitGranted(t, "ListBuilds as a Cloud Build Viewer", builds)
	_, err = bc.CreateBuild(ctx, &cloudbuildpb.CreateBuildRequest{ProjectId: project, Build: &cloudbuildpb.Build{
		Steps: []*cloudbuildpb.BuildStep{{Name: "ubuntu", Args: []string{"true"}}}}})
	wantDenied(t, "CreateBuild as a Cloud Build Viewer", err, newerDenied("cloudbuild.builds.create", "projects/"+project), "cloudbuild.builds.create", true)
	wantDenied(t, "ListWorkerPools as a Cloud Build Viewer", pools(), newerDenied("cloudbuild.workerpools.list", parent), "cloudbuild.workerpools.list", true)
	owner, err := cloudbuild.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	if _, err := owner.ListBuilds(ctx, &cloudbuildpb.ListBuildsRequest{ProjectId: project}).Next(); first(err) != nil {
		t.Fatalf("ListBuilds as the instance's account: %v", err)
	}
}

// TestIAMEnforcementPubSubPushActAs (#1879), with the official Pub/Sub
// client: a Pub/Sub Editor cannot create a push subscription whose OIDC
// token is a service account's, nor point one at it with ModifyPushConfig,
// without iam.serviceAccounts.actAs on it ("Authenticate push
// subscriptions"), refused in Pub/Sub's words; Service Account User on the
// account lets both through; the instance's own account needs no grant.
func TestIAMEnforcementPubSubPushActAs(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvPubSub)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iepp6-")
	owner, err := pubsub.NewClient(ctx, project, grpcAs(stripScheme(endpoint), "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	topic := "projects/" + project + "/topics/pushed"
	if _, err := owner.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = owner.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topic})
	})
	target := newServiceAccount(t, h, project, "pusher")
	push := func(account string) *pubsubpb.PushConfig {
		return &pubsubpb.PushConfig{PushEndpoint: "http://127.0.0.1:1/push",
			AuthenticationMethod: &pubsubpb.PushConfig_OidcToken_{OidcToken: &pubsubpb.PushConfig_OidcToken{ServiceAccountEmail: account}}}
	}
	sub := func(c *pubsub.Client, id string) error {
		name := "projects/" + project + "/subscriptions/" + id
		_, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name, Topic: topic, PushConfig: push(target.GetEmail())})
		if err == nil {
			t.Cleanup(func() {
				_ = owner.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: name})
			})
		}
		return err
	}
	if err := sub(owner, "as-owner"); err != nil {
		t.Fatalf("CreateSubscription as the instance's account: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "iepp6", "roles/pubsub.editor")
	as, err := pubsub.NewClient(ctx, project, grpcAs(stripScheme(endpoint), p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	wantPubSubDenied(t, "CreateSubscription without actAs", sub(as, "denied"))
	plain := "projects/" + project + "/subscriptions/plain"
	if _, err := as.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: plain, Topic: topic}); err != nil {
		t.Fatalf("CreateSubscription with no push account as an editor: %v", err)
	}
	t.Cleanup(func() {
		_ = owner.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: plain})
	})
	modify := func() error {
		return as.SubscriptionAdminClient.ModifyPushConfig(ctx, &pubsubpb.ModifyPushConfigRequest{Subscription: plain, PushConfig: push(target.GetEmail())})
	}
	wantPubSubDenied(t, "ModifyPushConfig without actAs", modify())
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	waitGranted(t, "CreateSubscription with actAs", func() error { return sub(as, "granted") })
	waitGranted(t, "ModifyPushConfig with actAs", modify)
}

// TestIAMEnforcementLoggingHierarchy (#1879), with the official Cloud
// Logging clients: ListSinks on a folder is refused until Logs Viewer on
// that folder's own policy, which does not reach the organization's sinks;
// and sinks.create over JSON (google.golang.org/api/logging/v2) with a
// customWriterIdentity is 403 for a Logs Configuration Writer until
// Service Account User on that account, then answers with it as the
// sink's writer identity.
func TestIAMEnforcementLoggingHierarchy(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvLogging)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	fc, _, _ := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: "organizations/100000000001",
		DisplayName: folderLabel(h, "ielh6-")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	project := iamProject(t, h, "ielh6-")
	p := newIAMPrincipal(t, h, project, "ielh6")
	cc, err := logging.NewConfigClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	sinks := func(parent string) func() error {
		return func() error {
			_, err := cc.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: parent}).Next()
			return first(err)
		}
	}
	denied := func(perm string) string { return "Permission '" + perm + "' denied on resource (or it may not exist)." }
	wantDenied(t, "ListSinks on a folder without a role", sinks(folder.GetName())(), denied("logging.sinks.list"), "logging.sinks.list", true)
	undo := bindOn(t, fc.GetIamPolicy, fc.SetIamPolicy, folder.GetName(),
		&iampb.Binding{Role: "roles/logging.viewer", Members: []string{"serviceAccount:" + p.email}})
	t.Cleanup(undo)
	waitGranted(t, "ListSinks on a folder as its Logs Viewer", sinks(folder.GetName()))
	wantDenied(t, "ListSinks on the organization as a folder's Logs Viewer", sinks("organizations/100000000001")(),
		denied("logging.sinks.list"), "logging.sinks.list", true)

	// A sink's custom writer identity needs actAs on it.
	target := newServiceAccount(t, h, project, "writer")
	p.grant("roles/logging.configWriter")
	t.Cleanup(func() { p.revoke("roles/logging.configWriter") })
	rest, err := logrest.NewService(ctx, restAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	create := func() (*logrest.LogSink, error) {
		return rest.Projects.Sinks.Create("projects/"+project, &logrest.LogSink{Name: "custom-writer",
			Destination: "logging.googleapis.com/projects/" + h.Project() + "/locations/global/buckets/_Default"}).
			CustomWriterIdentity("serviceAccount:" + target.GetEmail()).Context(ctx).Do()
	}
	_, err = create()
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden || !strings.Contains(gerr.Message, denied("iam.serviceAccounts.actAs")) {
		t.Fatalf("sinks.create with a custom writer, without actAs = %v; want 403", err)
	}
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountUser", "serviceAccount:"+p.email)
	var sk *logrest.LogSink
	waitGranted(t, "sinks.create with a custom writer, with actAs", func() error { sk, err = create(); return err })
	t.Cleanup(func() {
		_, _ = rest.Projects.Sinks.Delete("projects/" + project + "/sinks/custom-writer").Context(context.Background()).Do()
	})
	if sk.WriterIdentity != "serviceAccount:"+target.GetEmail() {
		t.Fatalf("writerIdentity = %q; want the custom writer", sk.WriterIdentity)
	}
}
