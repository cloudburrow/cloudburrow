//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// whoAmIScript is a step that asks the metadata server, at
// GCE_METADATA_HOST as every Google client library does, for its account's
// email and an identity token, and prints the email and the token's claims
// segment (not its signature).
const whoAmIScript = `H="http://$GCE_METADATA_HOST/computeMetadata/v1/instance/service-accounts/default"
echo "email:$(wget -q -O- --header 'Metadata-Flavor: Google' "$H/email")"
echo "claims:$(wget -q -O- --header 'Metadata-Flavor: Google' "$H/identity?audience=https://build.example&format=full" | cut -d. -f2)"
`

// buildIdentity is the email and the identity token's email claim a
// whoAmIScript step printed in a build's log.
func buildIdentity(t *testing.T, logText string) (email, claim string) {
	t.Helper()
	for _, l := range strings.Split(logText, "\n") {
		if _, rest, ok := strings.Cut(l, "email:"); ok && email == "" {
			email = strings.TrimSpace(rest)
		}
		if _, rest, ok := strings.Cut(l, "claims:"); ok && claim == "" {
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(rest), "="))
			if err != nil {
				t.Errorf("the identity token's claims %q: %v", rest, err)
				continue
			}
			var c struct {
				Email string `json:"email"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Errorf("the identity token's claims: %v", err)
			}
			claim = c.Email
		}
	}
	return email, claim
}

// buildServiceAccount makes an IAM service account in project through the
// official IAM client and returns it.
func buildServiceAccount(t *testing.T, h *Harness, project, prefix string) *adminpb.ServiceAccount {
	t.Helper()
	c := iamAdmin(t, h)
	id := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e8)
	sa, err := c.CreateServiceAccount(h.Context(), &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: id})
	if err != nil {
		t.Fatalf("CreateServiceAccount %s: %v", id, err)
	}
	t.Cleanup(func() {
		_ = c.DeleteServiceAccount(context.Background(), &adminpb.DeleteServiceAccountRequest{Name: sa.GetName()})
	})
	return sa
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/RunBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestCloudBuildServiceAccount (#1854), with the official Cloud Build
// client over gRPC on the Run port and the official IAM client, on an
// instance started with --cloudbuild-host-steps:
//   - a build with serviceAccount projects/P/serviceAccounts/EMAIL and its
//     own logsBucket runs its steps as that account: the metadata server
//     at GCE_METADATA_HOST names it, and an identity token's email claim is
//     its; a build with none runs as the default Cloud Build account,
//     GetDefaultServiceAccount's;
//   - a user-specified account with neither logsBucket nor
//     CLOUD_LOGGING_ONLY / NONE logging is refused INVALID_ARGUMENT with
//     Google's message; so are a name not of the form
//     projects/P/serviceAccounts/ACCOUNT and an account IAM does not have;
//     the account's unique ID names it as its email does;
//   - a trigger's serviceAccount is the account its builds run as, the
//     build config's own ignored, as Google documents; a malformed one is
//     refused INVALID_ARGUMENT at CreateBuildTrigger.
func TestCloudBuildServiceAccount(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	// A project of the instance's registry, which IAM keeps accounts in.
	project := iamProject(t, h, "cbsa-")
	sa := buildServiceAccount(t, h, project, "cbsa")
	other := buildServiceAccount(t, h, project, "cbsb")
	bucket := uploadSource(ctx, t, h, "unused.tar.gz", sourceArchive(t, map[string][]byte{"x": []byte("x")}))
	step := []*cbpb.BuildStep{{Id: "who", Name: stepImage(), Script: whoAmIScript}}

	// Google's requirements of a user-specified account.
	for _, c := range []struct {
		what string
		b    *cbpb.Build
		code codes.Code
		msg  string
	}{
		{"no logs bucket", &cbpb.Build{ServiceAccount: sa.GetName(), Steps: step}, codes.InvalidArgument,
			"if 'build.service_account' is specified, the build must either (a) specify 'build.logs_bucket'"},
		{"GCS_ONLY to the default bucket", &cbpb.Build{ServiceAccount: sa.GetName(), Steps: step,
			Options: &cbpb.BuildOptions{Logging: cbpb.BuildOptions_GCS_ONLY}}, codes.InvalidArgument, "CLOUD_LOGGING_ONLY / NONE"},
		{"a bare email", &cbpb.Build{ServiceAccount: sa.GetEmail(), Steps: step, LogsBucket: "gs://" + bucket + "/logs"},
			codes.InvalidArgument, "projects/{PROJECT_ID}/serviceAccounts/{ACCOUNT}"},
		{"no such account", &cbpb.Build{ServiceAccount: "projects/" + project + "/serviceAccounts/nobody-x@" + project + ".iam.gserviceaccount.com",
			Steps: step, LogsBucket: "gs://" + bucket + "/logs"}, codes.InvalidArgument, "does not exist"},
	} {
		_, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: project, Build: c.b})
		if status.Code(err) != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: CreateBuild = %v; want %v with %q", c.what, err, c.code, c.msg)
		}
	}

	run := func(b *cbpb.Build) *cbpb.Build {
		t.Helper()
		op, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: project, Build: b})
		skipWithoutHostSteps(t, err)
		if err != nil {
			t.Fatalf("CreateBuild: %v", err)
		}
		done, err := op.Wait(ctx)
		if err != nil || done.GetStatus() != cbpb.Build_SUCCESS {
			t.Fatalf("the build = %v, %v", done, err)
		}
		return done
	}
	for _, name := range []string{sa.GetName(), "projects/" + project + "/serviceAccounts/" + sa.GetUniqueId()} {
		b := run(&cbpb.Build{ServiceAccount: name, LogsBucket: "gs://" + bucket + "/logs", Steps: step})
		if b.GetServiceAccount() != name {
			t.Errorf("the build's serviceAccount = %q; want %q", b.GetServiceAccount(), name)
		}
		email, claim := buildIdentity(t, buildLogText(ctx, t, h, bucket, "logs/", b.GetId()))
		if email != sa.GetEmail() || claim != sa.GetEmail() {
			t.Errorf("%s: the step ran as %q (identity token %q); want %q", name, email, claim, sa.GetEmail())
		}
	}
	def, err := cb.GetDefaultServiceAccount(ctx, &cbpb.GetDefaultServiceAccountRequest{Name: "projects/" + project + "/locations/global/defaultServiceAccount"})
	if err != nil {
		t.Fatalf("GetDefaultServiceAccount: %v", err)
	}
	b := run(&cbpb.Build{LogsBucket: "gs://" + bucket + "/logs", Steps: step})
	if email, _ := buildIdentity(t, buildLogText(ctx, t, h, bucket, "logs/", b.GetId())); "projects/"+project+"/serviceAccounts/"+email != def.GetServiceAccountEmail() {
		t.Errorf("a build with no account ran as %q; want %s", email, def.GetServiceAccountEmail())
	}

	// A trigger's account, over its build config's.
	parent := "projects/" + project + "/locations/global"
	if _, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: project, Trigger: &cbpb.BuildTrigger{
		Name: "cbsa-bad-" + fmt.Sprint(time.Now().UnixNano()%1e6), ServiceAccount: sa.GetEmail(),
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Steps: step}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a trigger's bare email = %v; want INVALID_ARGUMENT", err)
	}
	trig, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: project, Trigger: &cbpb.BuildTrigger{
		Name: "cbsa-" + fmt.Sprint(time.Now().UnixNano()%1e6), ServiceAccount: sa.GetName(),
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{ServiceAccount: other.GetName(),
			LogsBucket: "gs://" + bucket + "/logs", Steps: step}}}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger: %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: project, TriggerId: trig.GetId()})
	})
	if trig.GetServiceAccount() != sa.GetName() {
		t.Errorf("the trigger's serviceAccount = %q", trig.GetServiceAccount())
	}
	op, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: project, TriggerId: trig.GetId()})
	skipWithoutHostSteps(t, err)
	if err != nil {
		t.Fatalf("RunBuildTrigger: %v", err)
	}
	tb, err := op.Wait(ctx)
	if err != nil || tb.GetStatus() != cbpb.Build_SUCCESS || tb.GetServiceAccount() != sa.GetName() {
		t.Fatalf("the triggered build = %v, %v", tb, err)
	}
	if email, claim := buildIdentity(t, buildLogText(ctx, t, h, bucket, "logs/", tb.GetId())); email != sa.GetEmail() || claim != sa.GetEmail() {
		t.Errorf("the triggered build ran as %q (identity token %q); want the trigger's %q", email, claim, sa.GetEmail())
	}
}

// TestGcloudBuildsServiceAccount (#1854): `gcloud builds triggers create
// manual --service-account` records the account and `gcloud builds
// triggers run` runs the build as it, through the configuration `cloudburrow
// gcloud-setup` writes, egress blocked; `gcloud builds submit` of a config
// naming serviceAccount without a logs option is refused with Google's
// message.
func TestGcloudBuildsServiceAccount(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	g := newGcloudSession(t, h)
	cb := cloudBuildClient(ctx, t, h)
	sa := buildServiceAccount(t, h, g.project, "gcbsa")
	bucket := uploadSource(ctx, t, h, "unused.tar.gz", sourceArchive(t, map[string][]byte{"x": []byte("x")}))
	dir := t.TempDir()
	script, _ := json.Marshal(whoAmIScript)
	inline := filepath.Join(dir, "build.yaml")
	if err := os.WriteFile(inline, []byte("steps:\n- name: "+stepImage()+"\n  script: "+string(script)+"\nlogsBucket: gs://"+bucket+"/logs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "gsa-" + fmt.Sprint(time.Now().UnixNano()%1e8)
	g.must("builds", "triggers", "create", "manual", "--name="+name, "--region=us-central1", "--inline-config="+inline,
		"--service-account="+sa.GetName())
	if got := lastLine(g.must("builds", "triggers", "describe", name, "--region=us-central1", "--format=value(serviceAccount)")); got != sa.GetName() {
		t.Errorf("the trigger's serviceAccount = %q; want %q", got, sa.GetName())
	}
	id := lastLine(g.must("builds", "triggers", "describe", name, "--region=us-central1", "--format=value(id)"))
	t.Cleanup(func() { _, _ = g.run(nil, "builds", "triggers", "delete", name, "--region=us-central1", "--quiet") })
	out, err := g.run(nil, "builds", "triggers", "run", name, "--region=us-central1")
	if err != nil && strings.Contains(out, hostStepsFlag) {
		t.Skip("the instance runs arbitrary build steps only with " + hostStepsFlag + " (#1382, ADR-0004); start it with the flag")
	}
	if err != nil {
		t.Fatalf("gcloud builds triggers run: %v\n%s", err, out)
	}
	b := triggeredBuild(ctx, t, cb, "projects/"+g.project+"/locations/us-central1", id, 1)
	if b.GetStatus() != cbpb.Build_SUCCESS || b.GetServiceAccount() != sa.GetName() {
		t.Fatalf("the triggered build = %v", b)
	}
	email, claim := buildIdentity(t, buildLogText(ctx, t, h, bucket, "logs/", b.GetId()))
	t.Logf("gcloud's triggered build %s ran as %s", b.GetId(), email)
	if email != sa.GetEmail() || claim != sa.GetEmail() {
		t.Errorf("the build ran as %q (identity token %q); want %q", email, claim, sa.GetEmail())
	}

	bare := filepath.Join(dir, "bare.yaml")
	if err := os.WriteFile(bare, []byte("steps:\n- name: "+stepImage()+"\n  args: ['true']\nserviceAccount: "+sa.GetName()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = g.run(nil, "builds", "submit", "--no-source", "--config="+bare, "--async")
	if err == nil || !strings.Contains(out, "build.service_account") {
		t.Errorf("gcloud builds submit of an account without a logs option = %v:\n%s", err, out)
	}
}
