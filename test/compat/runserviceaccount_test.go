//go:build compat

package compat

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// identityField is one key=value of an IDENTITY PROBE line.
func identityField(body, k string) string {
	m := regexp.MustCompile(`(?:^| )` + k + `=(\S*)`).FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Jobs/CreateJob, google.cloud.run.v2.Jobs/RunJob
//
// TestCloudRunWorkloadIsToldItsServiceAccount (#1388): the metadata server
// a Cloud Run workload reaches names the account it runs as, as on Cloud
// Run. A service whose template sets serviceAccount reads that account
// through cloud.google.com/go/compute/metadata, and an identity token from
// google.golang.org/api/idtoken carries it as its email claim; a service
// with none is told the project's Compute Engine default account,
// {number}-compute@developer.gserviceaccount.com; and a job's task is told
// its template's account. Each container returns what it read over HTTP,
// or prints it to its log, and GetService reports the stored account.
func TestCloudRunWorkloadIsToldItsServiceAccount(t *testing.T) {
	h := New(t)
	rc := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	loadEnvProbe(ctx, t)

	sa := "identity-probe@" + h.Project() + ".iam.gserviceaccount.com"
	compute := strconv.FormatInt(resource.ProjectNumber(h.Project()), 10) + "-compute@developer.gserviceaccount.com"
	base := ingress(t)
	for _, tc := range []struct{ id, account, want string }{
		{"compat-sa-set", sa, sa},
		{"compat-sa-default", "", compute},
	} {
		name := runParent(h) + "/services/" + tc.id
		op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: tc.id,
			Service: &runpb.Service{Template: &runpb.RevisionTemplate{ServiceAccount: tc.account,
				Containers: []*runpb.Container{{Image: envProbeImage}}}}})
		if err != nil {
			t.Fatalf("CreateService %s: %v", tc.id, err)
		}
		t.Cleanup(func() {
			delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = rc.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
		})
		svc, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("%s never became ready: %v", tc.id, err)
		}
		got, err := rc.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil || got.GetTemplate().GetServiceAccount() != tc.account {
			t.Errorf("GetService %s serviceAccount = %q (%v), want %q", tc.id, got.GetTemplate().GetServiceAccount(), err, tc.account)
		}
		code, body := httpGet(t, base, hostOf(t, svc.GetUri()), "/identity")
		if code != http.StatusOK || !strings.Contains(body, "IDENTITY PROBE: OK") {
			t.Fatalf("%s: the identity probe failed: %d %s", tc.id, code, body)
		}
		t.Logf("%s: %s", tc.id, strings.TrimSpace(body))
		if e := identityField(body, "email"); e != tc.want {
			t.Errorf("%s: the metadata server named %q, want %q", tc.id, e, tc.want)
		}
		if e := identityField(body, "id_token_email"); e != tc.want {
			t.Errorf("%s: the identity token's email = %q, want %q", tc.id, e, tc.want)
		}
		if identityField(body, "access_token") != "local" {
			t.Errorf("%s: no local access token: %s", tc.id, body)
		}
	}

	// A job's task, through the same server.
	jc, _ := runJobClients(t, h)
	jobID := "compat-sa-job"
	jobName := runParent(h) + "/jobs/" + jobID
	jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: jobID,
		Job: &runpb.Job{Template: &runpb.ExecutionTemplate{TaskCount: 1, Template: &runpb.TaskTemplate{
			ServiceAccount: sa, Retries: &runpb.TaskTemplate_MaxRetries{MaxRetries: 0},
			Timeout:    durationpb.New(3 * time.Minute),
			Containers: []*runpb.Container{{Image: envProbeImage, Args: []string{"identity"}}}}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: jobName}) })
	if _, err := jop.Wait(ctx); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	ran, err := rop.Wait(ctx)
	if err != nil || ran.GetSucceededCount() != 1 {
		t.Fatalf("the identity job did not succeed: %v %v", ran, err)
	}
	execID := ran.GetName()[strings.LastIndex(ran.GetName(), "/")+1:]
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	logs, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", "default", "logs", "job/"+execID).CombinedOutput()
	if err != nil || !strings.Contains(string(logs), "IDENTITY PROBE: OK") {
		t.Fatalf("the job's task logged %s (%v)", logs, err)
	}
	if e := identityField(string(logs), "email"); e != sa {
		t.Errorf("the job's task was told %q, want %q", e, sa)
	}
	if e := identityField(string(logs), "id_token_email"); e != sa {
		t.Errorf("the job's identity token names %q, want %q", e, sa)
	}
}
