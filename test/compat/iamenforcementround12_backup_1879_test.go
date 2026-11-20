//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	sbo "cloud.google.com/go/storagebatchoperations/apiv1"
	"cloud.google.com/go/storagebatchoperations/apiv1/storagebatchoperationspb"
	si "cloud.google.com/go/storageinsights/apiv1"
	"cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	sts "cloud.google.com/go/storagetransfer/apiv1"
	"cloud.google.com/go/storagetransfer/apiv1/storagetransferpb"
	"golang.org/x/oauth2"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// wantPastHTTP fails if err is a 403: the call passed its IAM check.
func wantPastHTTP(t *testing.T, what string, err error) {
	t.Helper()
	var ge *googleapi.Error
	if errors.As(err, &ge) && (ge.Code == http.StatusForbidden || ge.Code == http.StatusUnauthorized) {
		t.Fatalf("%s = %v; want it past its IAM check", what, err)
	}
}

// listErr is the first page's error of an iterator, nil for none or an
// empty list.
func listErr(err error) error {
	if errors.Is(err, iterator.Done) {
		return nil
	}
	return err
}

// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupVaults.list, backupdr.projects.locations.backupVaults.fetchUsable, backupdr.projects.locations.backupVaults.testIamPermissions, backupdr.projects.locations.backupPlans.list, backupdr.projects.locations.operations.get
//
// TestIAMEnforcementBackupDR (#1879), with the official backupdr/v1 REST
// client and a minted token: without a role, a vault's get
// (backupdr.backupVaults.get) and the location's backupVaults.list and
// backupPlans.list are refused, fetchUsable leaves the vault out and its
// testIamPermissions answers none; Backup and DR Viewer on the project
// lets them through and answers backupdr.backupVaults.get, not
// backupdr.backupVaults.delete, which is still refused. The instance's
// own account is never refused.
func TestIAMEnforcementBackupDR(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvBackupDR)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iobdr12-")
	loc := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iobdr12")
	owner := backupDRService(t, h)
	as, err := backupdr.NewService(ctx, restAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	op, err := owner.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{
		BackupMinimumEnforcedRetentionDuration: "86400s"}).BackupVaultId("v12").Context(ctx).Do()
	waitBackupDROp(t, owner, op, err)
	vault := loc + "/backupVaults/v12"
	t.Cleanup(func() {
		if op, err := owner.Projects.Locations.BackupVaults.Delete(vault).Force(true).Do(); err == nil {
			waitBackupDROp(t, owner, op, nil)
		}
	})
	if _, err := owner.Projects.Locations.BackupVaults.Get(vault).Context(ctx).Do(); err != nil {
		t.Fatalf("the instance's own account: get = %v", err)
	}
	get := func() error { _, err := as.Projects.Locations.BackupVaults.Get(vault).Context(ctx).Do(); return err }
	list := func() error { _, err := as.Projects.Locations.BackupVaults.List(loc).Context(ctx).Do(); return err }
	plans := func() error { _, err := as.Projects.Locations.BackupPlans.List(loc).Context(ctx).Do(); return err }
	usable := func() bool {
		r, err := as.Projects.Locations.BackupVaults.FetchUsable(loc).Context(ctx).Do()
		if err != nil {
			t.Fatalf("fetchUsable: %v", err)
		}
		for _, v := range r.BackupVaults {
			if v.Name == vault {
				return true
			}
		}
		return false
	}
	held := func() []string {
		r, err := as.Projects.Locations.BackupVaults.TestIamPermissions(vault, &backupdr.TestIamPermissionsRequest{
			Permissions: []string{"backupdr.backupVaults.get", "backupdr.backupVaults.delete"}}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("testIamPermissions: %v", err)
		}
		return r.Permissions
	}
	wantHTTPDenied(t, "get without a role", get(), newerDenied("backupdr.backupVaults.get", vault))
	wantHTTPDenied(t, "backupVaults.list without a role", list(), newerDenied("backupdr.backupVaults.list", loc))
	wantHTTPDenied(t, "backupPlans.list without a role", plans(), newerDenied("backupdr.backupPlans.list", loc))
	if usable() {
		t.Error("fetchUsable without a role lists the vault")
	}
	if got := held(); len(got) != 0 {
		t.Errorf("testIamPermissions without a role = %v; want none", got)
	}

	p.grant("roles/backupdr.viewer")
	t.Cleanup(func() { p.revoke("roles/backupdr.viewer") })
	waitGranted(t, "get as a Backup and DR Viewer", get)
	for what, call := range map[string]func() error{"backupVaults.list": list, "backupPlans.list": plans} {
		if err := call(); err != nil {
			t.Errorf("%s as a Backup and DR Viewer: %v", what, err)
		}
	}
	if !usable() {
		t.Error("fetchUsable as a Backup and DR Viewer leaves the vault out")
	}
	if got := held(); strings.Join(got, ",") != "backupdr.backupVaults.get" {
		t.Errorf("testIamPermissions as a Backup and DR Viewer = %v; want backupdr.backupVaults.get", got)
	}
	_, err = as.Projects.Locations.BackupVaults.Delete(vault).Context(ctx).Do()
	wantHTTPDenied(t, "delete as a Backup and DR Viewer", err, newerDenied("backupdr.backupVaults.delete", vault))
}

// covers: storagebatchoperations.projects.locations.jobs.list, storagebatchoperations.projects.locations.jobs.get, storagebatchoperations.projects.locations.jobs.create, storageinsights.projects.locations.reportConfigs.list, storagetransfer.projects.agentPools.list, storagetransfer.googleServiceAccounts.get
//
// TestIAMEnforcementStorageCompanions (#1879), with the official Storage
// Batch Operations, Storage Insights and Storage Transfer clients over
// REST and a minted token: without a role, ListJobs
// (storagebatchoperations.jobs.list), GetJob, ListReportConfigs
// (storageinsights.reportConfigs.list), ListAgentPools
// (storagetransfer.agentpools.list) and GetGoogleServiceAccount
// (storagetransfer.projects.getServiceAccount) are refused; each API's
// Viewer role lets them through (GetJob of no job is then NOT_FOUND), but
// not CreateJob (storagebatchoperations.jobs.create). The instance's own
// account is never refused.
func TestIAMEnforcementStorageCompanions(t *testing.T) {
	h := New(t)
	batchAddr := h.Endpoint(EnvStorageBatch)
	insightsAddr := h.Endpoint(EnvStorageInsights)
	transferAddr := h.Endpoint(EnvStorageTransfer)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iosj12-")
	p := newIAMPrincipal(t, h, project, "iosj12")
	opts := func(addr, token string) []option.ClientOption {
		if token == "" {
			return []option.ClientOption{option.WithEndpoint("http://" + stripScheme(addr)), option.WithoutAuthentication()}
		}
		return []option.ClientOption{option.WithEndpoint("http://" + stripScheme(addr)),
			option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}})}
	}
	batch := func(token string) *sbo.Client {
		c, err := sbo.NewRESTClient(ctx, opts(batchAddr, token)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	insights := func(token string) *si.Client {
		c, err := si.NewRESTClient(ctx, opts(insightsAddr, token)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	transfer := func(token string) *sts.Client {
		c, err := sts.NewRESTClient(ctx, opts(transferAddr, token)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	parent := "projects/" + project + "/locations/global"
	insightsParent := "projects/" + project + "/locations/us-central1"
	job := parent + "/jobs/none12"
	calls := func(b *sbo.Client, i *si.Client, x *sts.Client) map[string]func() error {
		return map[string]func() error{
			"ListJobs": func() error {
				_, err := b.ListJobs(ctx, &storagebatchoperationspb.ListJobsRequest{Parent: parent}).Next()
				return listErr(err)
			},
			"ListReportConfigs": func() error {
				_, err := i.ListReportConfigs(ctx, &storageinsightspb.ListReportConfigsRequest{Parent: insightsParent}).Next()
				return listErr(err)
			},
			"ListAgentPools": func() error {
				_, err := x.ListAgentPools(ctx, &storagetransferpb.ListAgentPoolsRequest{ProjectId: project}).Next()
				return listErr(err)
			},
			"GetGoogleServiceAccount": func() error {
				_, err := x.GetGoogleServiceAccount(ctx, &storagetransferpb.GetGoogleServiceAccountRequest{ProjectId: project})
				return err
			},
		}
	}
	for what, call := range calls(batch(""), insights(""), transfer("")) {
		if err := call(); err != nil {
			t.Errorf("the instance's own account: %s = %v", what, err)
		}
	}
	b, i, x := batch(p.token), insights(p.token), transfer(p.token)
	as := calls(b, i, x)
	getJob := func() error { _, err := b.GetJob(ctx, &storagebatchoperationspb.GetJobRequest{Name: job}); return err }
	wantHTTPDenied(t, "ListJobs without a role", as["ListJobs"](), newerDenied("storagebatchoperations.jobs.list", parent))
	wantHTTPDenied(t, "GetJob without a role", getJob(), newerDenied("storagebatchoperations.jobs.get", job))
	wantHTTPDenied(t, "ListReportConfigs without a role", as["ListReportConfigs"](),
		newerDenied("storageinsights.reportConfigs.list", insightsParent))
	wantHTTPDenied(t, "ListAgentPools without a role", as["ListAgentPools"](),
		newerDenied("storagetransfer.agentpools.list", "projects/"+project))
	wantHTTPDenied(t, "GetGoogleServiceAccount without a role", as["GetGoogleServiceAccount"](),
		newerDenied("storagetransfer.projects.getServiceAccount", "projects/"+project))

	for _, role := range []string{"roles/storagebatchoperations.viewer", "roles/storageinsights.viewer", "roles/storagetransfer.viewer"} {
		p.grant(role)
		t.Cleanup(func() { p.revoke(role) })
	}
	waitGranted(t, "ListJobs as a viewer", as["ListJobs"])
	for what, call := range as {
		if err := call(); err != nil {
			t.Errorf("%s as a viewer: %v", what, err)
		}
	}
	wantPastHTTP(t, "GetJob as a viewer", getJob())
	_, err := b.CreateJob(ctx, &storagebatchoperationspb.CreateJobRequest{Parent: parent, JobId: "denied12",
		Job: &storagebatchoperationspb.Job{}})
	wantHTTPDenied(t, "CreateJob as a viewer", err, newerDenied("storagebatchoperations.jobs.create", parent))
}
