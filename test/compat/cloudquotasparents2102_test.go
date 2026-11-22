//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
)

const wfPoolQuota = "WORKFORCE-POOL-COUNT-per-organization"

// covers: google.api.cloudquotas.v1.CloudQuotas/ListQuotaInfos, google.api.cloudquotas.v1.CloudQuotas/GetQuotaInfo, google.api.cloudquotas.v1.CloudQuotas/ListQuotaPreferences, google.api.cloudquotas.v1.CloudQuotas/GetQuotaPreference, google.api.cloudquotas.v1.QuotaAdjusterSettingsManager/GetQuotaAdjusterSettings, google.api.cloudquotas.v1.QuotaAdjusterSettingsManager/UpdateQuotaAdjusterSettings
//
// TestCloudQuotasFolderAndOrganizationParents (#2102), with
// cloud.google.com/go/cloudquotas over gRPC and JSON: under
// organizations/{n}/locations/global the organization's quotas (IAM's
// workforce pool count, its request rates, the custom role limit), each
// containerType ORGANIZATION, and no project quota; under a folder none;
// an organization quota is not found under a project, and its preference
// is refused there and under a folder; an unknown folder is denied; a
// container lists only its own preferences; and the quota adjuster's
// enablement is inherited by a folder's project from the nearest ancestor
// that sets it (inherited, inheritedFrom), "default" when none does.
func TestCloudQuotasFolderAndOrganizationParents(t *testing.T) {
	h := New(t)
	clients, adjusters := cloudQuotasClients(t, h)
	ctx := h.Context()
	fc, err := resourcemanager.NewFoldersClient(ctx, rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	org := localOrgRoles + "/locations/global"
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			// Each transport has its own folders and project, as the
			// adjuster settings below are stored on them.
			tag := transport[:1]
			folder := quotaFolder(t, h, fc, localOrgRoles, "quota"+tag+"-")
			child := quotaFolder(t, h, fc, folder, "quotc"+tag+"-")
			project := orgPolicyProject(t, h, pc, child, "quotp"+tag+"-")
			ids := map[string]bool{}
			it := c.ListQuotaInfos(ctx, &pb.ListQuotaInfosRequest{Parent: org + "/services/iam.googleapis.com"})
			for {
				qi, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if qi.GetContainerType() != pb.QuotaInfo_ORGANIZATION || !strings.HasPrefix(qi.GetName(), org+"/services/iam.googleapis.com/quotaInfos/") {
					t.Errorf("organization quota info %v", qi)
				}
				ids[qi.GetQuotaId()] = true
			}
			if !ids[wfPoolQuota] || !ids["CUSTOM-ROLES-per-organization"] || ids[saQuota] {
				t.Errorf("organization quotas = %v", ids)
			}
			qi, err := c.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: org + "/services/iam.googleapis.com/quotaInfos/CUSTOM-ROLES-per-organization"})
			if err != nil || !qi.GetIsFixed() || qi.GetDimensionsInfos()[0].GetDetails().GetValue() != 300 {
				t.Errorf("custom roles limit = %v, %v", qi, err)
			}
			fit := c.ListQuotaInfos(ctx, &pb.ListQuotaInfosRequest{Parent: folder + "/locations/global/services/iam.googleapis.com"})
			if qi, err := fit.Next(); err != iterator.Done {
				t.Errorf("folder quota infos = %v, %v, want none", qi, err)
			}
			if _, err := c.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: "projects/" + project + "/locations/global/services/iam.googleapis.com/quotaInfos/" + wfPoolQuota}); quotaCode(err) != codes.NotFound {
				t.Errorf("organization quota under a project = %v, want NOT_FOUND", err)
			}
			for _, at := range []string{"projects/" + project, folder} {
				_, err := c.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: at + "/locations/global", QuotaPreference: &pb.QuotaPreference{
					Service: "iam.googleapis.com", QuotaId: wfPoolQuota, QuotaConfig: &pb.QuotaConfig{PreferredValue: 110}}})
				if quotaCode(err) != codes.InvalidArgument {
					t.Errorf("organization quota preference under %s = %v, want INVALID_ARGUMENT", at, err)
				}
			}
			if _, err := c.GetQuotaPreference(ctx, &pb.GetQuotaPreferenceRequest{Name: "folders/987654321987/locations/global/quotaPreferences/x"}); quotaCode(err) != codes.PermissionDenied {
				t.Errorf("unknown folder = %v, want PERMISSION_DENIED", err)
			}
			pit := c.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: folder + "/locations/global"})
			if p, err := pit.Next(); err != iterator.Done {
				t.Errorf("folder preferences = %v, %v, want none", p, err)
			}

			a := adjusters[transport]
			projSettings := "projects/" + project + "/locations/global/quotaAdjusterSettings"
			s, err := a.GetQuotaAdjusterSettings(ctx, &pb.GetQuotaAdjusterSettingsRequest{Name: projSettings})
			if err != nil || !s.GetInherited() || (s.GetInheritedFrom() != "default" && s.GetInheritedFrom() != localOrgRoles) {
				t.Errorf("unset project adjuster = %v, %v", s, err)
			}
			set, err := a.UpdateQuotaAdjusterSettings(ctx, &pb.UpdateQuotaAdjusterSettingsRequest{QuotaAdjusterSettings: &pb.QuotaAdjusterSettings{
				Name: folder + "/locations/global/quotaAdjusterSettings", Enablement: pb.QuotaAdjusterSettings_ENABLED}})
			if err != nil || set.GetName() != folder+"/locations/global/quotaAdjusterSettings" {
				t.Fatalf("folder adjuster = %v, %v", set, err)
			}
			s, err = a.GetQuotaAdjusterSettings(ctx, &pb.GetQuotaAdjusterSettingsRequest{Name: projSettings})
			if err != nil || s.GetEnablement() != pb.QuotaAdjusterSettings_ENABLED || !s.GetInherited() || s.GetInheritedFrom() != folder {
				t.Errorf("project under the folder = %v, %v, want ENABLED from %s", s, err, folder)
			}
			if _, err := a.UpdateQuotaAdjusterSettings(ctx, &pb.UpdateQuotaAdjusterSettingsRequest{QuotaAdjusterSettings: &pb.QuotaAdjusterSettings{
				Name: child + "/locations/global/quotaAdjusterSettings", Enablement: pb.QuotaAdjusterSettings_DISABLED}}); err != nil {
				t.Fatal(err)
			}
			s, err = a.GetQuotaAdjusterSettings(ctx, &pb.GetQuotaAdjusterSettingsRequest{Name: projSettings})
			if err != nil || s.GetEnablement() != pb.QuotaAdjusterSettings_DISABLED || s.GetInheritedFrom() != child {
				t.Errorf("nearest ancestor = %v, %v, want DISABLED from %s", s, err, child)
			}
			s, err = a.GetQuotaAdjusterSettings(ctx, &pb.GetQuotaAdjusterSettingsRequest{Name: child + "/locations/global/quotaAdjusterSettings"})
			if err != nil || s.GetInherited() || s.GetInheritedFrom() != "" || s.GetEnablement() != pb.QuotaAdjusterSettings_DISABLED {
				t.Errorf("set on the child folder = %v, %v", s, err)
			}
			if _, err := a.UpdateQuotaAdjusterSettings(ctx, &pb.UpdateQuotaAdjusterSettingsRequest{QuotaAdjusterSettings: &pb.QuotaAdjusterSettings{
				Name: projSettings, Enablement: pb.QuotaAdjusterSettings_ENABLED}}); err != nil {
				t.Fatal(err)
			}
			s, err = a.GetQuotaAdjusterSettings(ctx, &pb.GetQuotaAdjusterSettingsRequest{Name: projSettings})
			if err != nil || s.GetInherited() || s.GetEnablement() != pb.QuotaAdjusterSettings_ENABLED {
				t.Errorf("set on the project = %v, %v", s, err)
			}
		})
	}
}

// covers: google.api.cloudquotas.v1.CloudQuotas/CreateQuotaPreference, google.api.cloudquotas.v1.CloudQuotas/UpdateQuotaPreference
//
// TestCloudQuotasOrganizationPreferenceIsEnforced (#2102): a preference
// under organizations/{n} for IAM's "Number of workforce identity pools"
// is the limit the organization's CreateWorkforcePool enforces
// (RESOURCE_EXHAUSTED past it, through google.golang.org/api/iam/v1); a
// decrease below the organization's pools is refused by the safety check;
// the organization's preference is listed under the organization only,
// not under its projects. The preference goes back to 100 at the end.
func TestCloudQuotasOrganizationPreferenceIsEnforced(t *testing.T) {
	h := New(t)
	clients, _ := cloudQuotasClients(t, h)
	c := clients["grpc"]
	ctx := h.Context()
	org := localOrgRoles + "/locations/global"
	svc := iamREST(t, h)
	pools := svc.Locations.WorkforcePools
	live := 0
	if err := pools.List("locations/global").Parent(localOrgRoles).Pages(ctx, func(r *iamv1.ListWorkforcePoolsResponse) error {
		live += len(r.WorkforcePools)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// One preference per quota and dimensions (Google's reference): reuse
	// the organization's, if an earlier run or the console made it.
	name := org + "/quotaPreferences/compat-workforce-pools"
	eit := c.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: org, Filter: "quotaId=" + wfPoolQuota})
	if got, err := eit.Next(); err == nil {
		name = got.GetName()
	}
	set := func(v int64, ignore ...pb.QuotaSafetyCheck) (*pb.QuotaPreference, error) {
		return c.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{AllowMissing: true, IgnoreSafetyChecks: ignore,
			QuotaPreference: &pb.QuotaPreference{Name: name, Service: "iam.googleapis.com", QuotaId: wfPoolQuota,
				QuotaConfig: &pb.QuotaConfig{PreferredValue: v}}})
	}
	t.Cleanup(func() {
		_, _ = c.UpdateQuotaPreference(context.Background(), &pb.UpdateQuotaPreferenceRequest{AllowMissing: true,
			QuotaPreference: &pb.QuotaPreference{Name: name, Service: "iam.googleapis.com", QuotaId: wfPoolQuota, QuotaConfig: &pb.QuotaConfig{PreferredValue: 100}}})
	})
	all := []pb.QuotaSafetyCheck{pb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH}
	if live > 0 {
		if _, err := set(int64(live-1), all...); quotaCode(err) != codes.FailedPrecondition {
			t.Errorf("a decrease below the organization's %d pools = %v, want FAILED_PRECONDITION", live, err)
		}
	}
	p, err := set(int64(live+1), all...)
	if err != nil || p.GetQuotaConfig().GetGrantedValue().GetValue() != int64(live+1) || p.GetName() != name {
		t.Fatalf("organization preference = %v, %v", p, err)
	}
	create := func(prefix string) error {
		id := wfPoolID(h, prefix)
		_, err := pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles}).WorkforcePoolId(id).Context(ctx).Do()
		if err == nil {
			t.Cleanup(func() {
				_, _ = pools.Delete("locations/global/workforcePools/" + id).Context(context.Background()).Do()
			})
		}
		return err
	}
	if err := create("wfq1-"); err != nil {
		t.Fatalf("a pool within the limit: %v", err)
	}
	if err := create("wfq2-"); err == nil {
		t.Fatal("a pool past the organization's limit was created")
	} else {
		wantWIFError(t, "a pool past the limit", err, 429)
	}
	if got, err := c.GetQuotaPreference(ctx, &pb.GetQuotaPreferenceRequest{Name: name}); err != nil || got.GetQuotaId() != wfPoolQuota {
		t.Errorf("GetQuotaPreference = %v, %v", got, err)
	}
	pit := c.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: "projects/" + h.Project() + "/locations/global", Filter: "quotaId=" + wfPoolQuota})
	if got, err := pit.Next(); err != iterator.Done {
		t.Errorf("a project lists the organization's preference: %v, %v", got, err)
	}
}

// quotaFolder is a folder under parent, deleted at the end.
func quotaFolder(t *testing.T, h *Harness, fc *resourcemanager.FoldersClient, parent, prefix string) string {
	t.Helper()
	ctx := h.Context()
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: parent, DisplayName: folderLabel(h, prefix)}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	f, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	t.Cleanup(func() { _, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}) })
	return f.GetName()
}
