//go:build compat

package compat

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.dataform.v1.Dataform/CreateCompilationResultLongRunning, google.cloud.dataform.v1.Dataform/CancelWorkflowInvocation
//
// TestDataformLongRunningCompileAndCancel (#2105): CreateCompilationResultLongRunning
// (the official client's long-running form of CreateCompilationResult)
// compiles a workspace as an operation the client waits on, over gRPC
// and REST. An invocation whose first action is a slow BigQuery query is
// then cancelled while that action runs: the invocation is CANCELLED, the
// running action and the one depending on it are CANCELLED, and the
// action's BigQuery job was stopped (DONE with reason "stopped"), not
// left to finish.
func TestDataformLongRunningCompileAndCancel(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	bq, project := dataformBigQuery(t, h)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: Dataform core runs in a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	dataset := "dfc_" + suffix
	t.Cleanup(func() { _ = bq.Dataset(dataset).DeleteWithContents(context.Background()) })
	repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: fmt.Sprintf("projects/%s/locations/us-central1", project), RepositoryId: "c" + suffix})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
	})
	ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	dataformWrite(ctx, t, c, ws.GetName(), "workflow_settings.yaml", fmt.Sprintf(
		"defaultProject: %s\ndefaultLocation: US\ndefaultDataset: %s\ndefaultAssertionDataset: %s_assertions\ndataformCoreVersion: 3.1.6\n", project, dataset, dataset))
	dataformWrite(ctx, t, c, ws.GetName(), "definitions/slow.sqlx",
		"config { type: \"table\" }\nselect count(*) as n from unnest(generate_array(1, 3000)) a, unnest(generate_array(1, 3000)) b, unnest(generate_array(1, 100)) c\n")
	dataformWrite(ctx, t, c, ws.GetName(), "definitions/after.sqlx", "config { type: \"view\" }\nselect n from ${ref(\"slow\")}\n")

	var comp *dfpb.CompilationResult
	for transport, cl := range clients {
		op, err := cl.CreateCompilationResultLongRunning(ctx, &dfpb.CreateCompilationResultRequest{Parent: repo.GetName(),
			CompilationResult: &dfpb.CompilationResult{Source: &dfpb.CompilationResult_Workspace{Workspace: ws.GetName()}}})
		if err != nil {
			t.Fatalf("%s: CreateCompilationResultLongRunning: %v", transport, err)
		}
		res, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("%s: the compile operation: %v", transport, err)
		}
		if len(res.GetCompilationErrors()) > 0 || res.GetWorkspace() != ws.GetName() || !strings.HasPrefix(res.GetName(), repo.GetName()+"/compilationResults/") {
			t.Fatalf("%s: the compilation result = %v", transport, res)
		}
		if got, err := c.GetCompilationResult(ctx, &dfpb.GetCompilationResultRequest{Name: res.GetName()}); err != nil || got.GetName() != res.GetName() {
			t.Errorf("%s: GetCompilationResult of the operation's result = %v, %v", transport, got, err)
		}
		comp = res
	}

	inv, err := c.CreateWorkflowInvocation(ctx, &dfpb.CreateWorkflowInvocationRequest{Parent: repo.GetName(),
		WorkflowInvocation: &dfpb.WorkflowInvocation{CompilationSource: &dfpb.WorkflowInvocation_CompilationResult{CompilationResult: comp.GetName()}}})
	if err != nil {
		t.Fatalf("CreateWorkflowInvocation: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for invocationActions(ctx, t, c, inv.GetName())["slow"].GetState() != dfpb.WorkflowInvocationAction_RUNNING {
		if time.Now().After(deadline) {
			t.Fatalf("the slow action never ran: %v", invocationActions(ctx, t, c, inv.GetName())["slow"])
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // its query job is running on BigQuery
	asked := time.Now()
	if _, err := clients["rest"].CancelWorkflowInvocation(ctx, &dfpb.CancelWorkflowInvocationRequest{Name: inv.GetName()}); err != nil {
		t.Fatalf("CancelWorkflowInvocation: %v", err)
	}
	done := waitInvocation(ctx, t, c, inv.GetName())
	took := time.Since(asked)
	if done.GetState() != dfpb.WorkflowInvocation_CANCELLED {
		t.Errorf("the cancelled invocation = %v, want CANCELLED", done.GetState())
	}
	acts := invocationActions(ctx, t, c, inv.GetName())
	for _, name := range []string{"slow", "after"} {
		if acts[name].GetState() != dfpb.WorkflowInvocationAction_CANCELLED {
			t.Errorf("action %s = %v (%s), want CANCELLED", name, acts[name].GetState(), acts[name].GetFailureReason())
		}
	}
	jobID := acts["slow"].GetBigqueryAction().GetJobId()
	if jobID == "" {
		t.Fatalf("the slow action names no BigQuery job: %v", acts["slow"])
	}
	job, err := bq.JobFromIDLocation(ctx, jobID, "US")
	if err != nil {
		t.Fatalf("JobFromIDLocation %s: %v", jobID, err)
	}
	st := job.LastStatus()
	if !st.Done() || st.Err() == nil || !strings.Contains(fmt.Sprint(st.Err()), "stopped") {
		t.Errorf("the slow action's job %s: done %v, err %v; want DONE with reason stopped", jobID, st.Done(), st.Err())
	}
	if _, err := bq.Dataset(dataset).Table("slow").Metadata(ctx); err == nil {
		t.Error("the cancelled table action created its table")
	}
	t.Logf("cancelled %v after the request", took)
	if _, err := c.CancelWorkflowInvocation(ctx, &dfpb.CancelWorkflowInvocationRequest{Name: inv.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("cancelling a finished invocation: %v, want FAILED_PRECONDITION", err)
	}
}

// covers: google.cloud.dataform.v1.Dataform/GetConfig, google.cloud.dataform.v1.Dataform/UpdateConfig
//
// TestDataformCustomerManagedKey (#2105): a repository protected by a key
// of this instance's Cloud KMS, in its own region, records the key version
// in data_encryption_state; a key of another region is refused, the key
// cannot be changed, and while the key's version is DISABLED the
// repository's data (workspace files) is refused while its record still
// reads; enabled again, it works. Config's default_kms_key_name protects a
// repository created without a key.
func TestDataformCustomerManagedKey(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	kc, err := kms.NewKeyManagementClient(h.Context(), option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	mkKey := func(region string) string {
		parent := fmt.Sprintf("projects/%s/locations/%s", h.Project(), region)
		if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: "df" + suffix}); err != nil {
			t.Fatalf("CreateKeyRing: %v", err)
		}
		k, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: parent + "/keyRings/df" + suffix, CryptoKeyId: "repo",
			CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
		if err != nil {
			t.Fatalf("CreateCryptoKey: %v", err)
		}
		return k.GetName()
	}
	key, far := mkKey("us-central1"), mkKey("europe-west1")
	version := key + "/cryptoKeyVersions/1"
	setState := func(s kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: s},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", s, err)
		}
	}
	parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			if _, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "far" + transport + suffix,
				Repository: &dfpb.Repository{KmsKeyName: far}}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("a key of another region: %v, want INVALID_ARGUMENT", err)
			}
			repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "k" + transport + suffix,
				Repository: &dfpb.Repository{KmsKeyName: key}})
			if err != nil {
				t.Fatalf("CreateRepository with a key: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			if repo.GetKmsKeyName() != key || repo.GetDataEncryptionState().GetKmsKeyVersionName() != version {
				t.Errorf("the repository's key = %q, data_encryption_state %v", repo.GetKmsKeyName(), repo.GetDataEncryptionState())
			}
			if _, err := c.UpdateRepository(ctx, &dfpb.UpdateRepositoryRequest{Repository: &dfpb.Repository{Name: repo.GetName(), KmsKeyName: far},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("changing the key: %v, want INVALID_ARGUMENT", err)
			}
			ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			dataformWrite(ctx, t, c, ws.GetName(), "definitions/a.sqlx", "select 1 as a\n")
			setState(kmspb.CryptoKeyVersion_DISABLED)
			_, rerr := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws.GetName(), Path: "definitions/a.sqlx"})
			_, werr := c.WriteFile(ctx, &dfpb.WriteFileRequest{Workspace: ws.GetName(), Path: "definitions/b.sqlx", Contents: []byte("select 2\n")})
			_, gerr := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: repo.GetName()})
			setState(kmspb.CryptoKeyVersion_ENABLED)
			// FAILED_PRECONDITION; over REST it is HTTP 400, which the
			// client reads back as INVALID_ARGUMENT.
			refused := func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "Cloud KMS key cannot be used") &&
					(status.Code(err) == codes.FailedPrecondition || transport == "rest")
			}
			if !refused(rerr) || !refused(werr) {
				t.Errorf("with the key disabled: ReadFile %v, WriteFile %v; want FAILED_PRECONDITION", rerr, werr)
			}
			if gerr != nil {
				t.Errorf("GetRepository with the key disabled: %v", gerr)
			}
			if got, err := c.ReadFile(ctx, &dfpb.ReadFileRequest{Workspace: ws.GetName(), Path: "definitions/a.sqlx"}); err != nil || string(got.GetFileContents()) != "select 1 as a\n" {
				t.Errorf("ReadFile with the key enabled again = %v, %v", got, err)
			}
		})
	}
	c := clients["grpc"]
	cfgName := parent + "/config"
	if _, err := c.UpdateConfig(ctx, &dfpb.UpdateConfigRequest{Config: &dfpb.Config{Name: cfgName, DefaultKmsKeyName: far}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a default key of another region: %v, want INVALID_ARGUMENT", err)
	}
	cfg, err := clients["rest"].UpdateConfig(ctx, &dfpb.UpdateConfigRequest{Config: &dfpb.Config{Name: cfgName, DefaultKmsKeyName: key}})
	if err != nil || cfg.GetDefaultKmsKeyName() != key {
		t.Fatalf("UpdateConfig = %v, %v", cfg, err)
	}
	t.Cleanup(func() {
		_, _ = c.UpdateConfig(context.Background(), &dfpb.UpdateConfigRequest{Config: &dfpb.Config{Name: cfgName},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_kms_key_name"}}})
	})
	if got, err := c.GetConfig(ctx, &dfpb.GetConfigRequest{Name: cfgName}); err != nil || got.GetDefaultKmsKeyName() != key {
		t.Errorf("GetConfig = %v, %v", got, err)
	}
	d, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "default" + suffix})
	if err != nil {
		t.Fatalf("CreateRepository under the default key: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: d.GetName(), Force: true})
	})
	if d.GetKmsKeyName() != key || d.GetDataEncryptionState().GetKmsKeyVersionName() != version {
		t.Errorf("the repository under the default key = %q, %v", d.GetKmsKeyName(), d.GetDataEncryptionState())
	}
}

// TestDataformDeveloperConnectRemote (#2105): a repository whose
// git_remote_settings name a Developer Connect git repository link (to a
// Secure Source Manager repository of this instance) and no token secret
// pushes and fetches with the link's read-write token; a link that does
// not exist is refused.
func TestDataformDeveloperConnectRemote(t *testing.T) {
	h := New(t)
	clients := dataformClients(t, h)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	ssm, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ssm.Close() })
	dc, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	parent := runParent(h)
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
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "dcr-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository (Secure Source Manager): %v", err)
	}
	remote, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cop, err := dc.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: "df-" + suffix,
		Connection: &dcpb.Connection{ConnectionConfig: &dcpb.Connection_SecureSourceManagerInstanceConfig{
			SecureSourceManagerInstanceConfig: &dcpb.SecureSourceManagerInstanceConfig{Instance: inst.GetName()}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lop, err := dc.CreateGitRepositoryLink(ctx, &dcpb.CreateGitRepositoryLinkRequest{Parent: conn.GetName(), GitRepositoryLinkId: "link",
		GitRepositoryLink: &dcpb.GitRepositoryLink{CloneUri: remote.GetUris().GetGitHttps()}})
	if err != nil {
		t.Fatalf("CreateGitRepositoryLink: %v", err)
	}
	link, err := lop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := dc.DeleteGitRepositoryLink(ctx, &dcpb.DeleteGitRepositoryLinkRequest{Name: link.GetName()}); err == nil {
			_ = op.Wait(ctx)
		}
		if op, err := dc.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: conn.GetName()}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	dfParent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			missing := conn.GetName() + "/gitRepositoryLinks/missing"
			if _, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: dfParent, RepositoryId: "nolink" + transport + suffix,
				Repository: &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: remote.GetUris().GetGitHttps(), DefaultBranch: "main",
					GitRepositoryLink: &missing}}}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("a link that does not exist: %v, want INVALID_ARGUMENT", err)
			}
			name := link.GetName()
			repo, err := c.CreateRepository(ctx, &dfpb.CreateRepositoryRequest{Parent: dfParent, RepositoryId: "link" + transport + suffix,
				Repository: &dfpb.Repository{GitRemoteSettings: &dfpb.Repository_GitRemoteSettings{Url: remote.GetUris().GetGitHttps(), DefaultBranch: "main",
					GitRepositoryLink: &name}}})
			if err != nil {
				t.Fatalf("CreateRepository with a git repository link: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &dfpb.DeleteRepositoryRequest{Name: repo.GetName(), Force: true})
			})
			if st, err := c.ComputeRepositoryAccessTokenStatus(ctx, &dfpb.ComputeRepositoryAccessTokenStatusRequest{Name: repo.GetName()}); err != nil ||
				st.GetTokenStatus() != dfpb.ComputeRepositoryAccessTokenStatusResponse_VALID {
				t.Errorf("ComputeRepositoryAccessTokenStatus = %v, %v", st, err)
			}
			ws, err := c.CreateWorkspace(ctx, &dfpb.CreateWorkspaceRequest{Parent: repo.GetName(), WorkspaceId: "dev"})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			dataformWrite(ctx, t, c, ws.GetName(), "definitions/"+transport+".sqlx", "config { type: \"view\" }\nselect 1 as x\n")
			author := &dfpb.CommitAuthor{Name: "Dev", EmailAddress: "dev@example.com"}
			if _, err := c.CommitWorkspaceChanges(ctx, &dfpb.CommitWorkspaceChangesRequest{Name: ws.GetName(), Author: author, CommitMessage: transport}); err != nil {
				t.Fatalf("CommitWorkspaceChanges: %v", err)
			}
			if _, err := c.PushGitCommits(ctx, &dfpb.PushGitCommitsRequest{Name: ws.GetName(), RemoteBranch: "link-" + transport}); err != nil {
				t.Fatalf("PushGitCommits with the link's token: %v", err)
			}
			br, err := c.FetchRemoteBranches(ctx, &dfpb.FetchRemoteBranchesRequest{Name: repo.GetName()})
			if err != nil || !strings.Contains(strings.Join(br.GetBranches(), ","), "link-"+transport) {
				t.Errorf("FetchRemoteBranches = %v, %v", br, err)
			}
		})
	}
}
