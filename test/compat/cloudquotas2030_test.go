//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	cloudquotas "cloud.google.com/go/cloudquotas/apiv1"
	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const saQuota = "SERVICE-ACCOUNT-COUNT-per-project"

// cloudQuotasClients are the official cloudquotas/apiv1 clients over gRPC
// and JSON on the Resource Manager port (#2030).
func cloudQuotasClients(t *testing.T, h *Harness) (map[string]*cloudquotas.Client, map[string]*cloudquotas.QuotaAdjusterSettingsManagerClient) {
	t.Helper()
	ep := h.Endpoint(EnvResourceManager)
	g, err := cloudquotas.NewClient(h.Context(), rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := cloudquotas.NewRESTClient(h.Context(), option.WithEndpoint("http://"+ep), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	ag, err := cloudquotas.NewQuotaAdjusterSettingsManagerClient(h.Context(), rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := cloudquotas.NewQuotaAdjusterSettingsManagerRESTClient(h.Context(), option.WithEndpoint("http://"+ep), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close(); _ = ag.Close(); _ = ar.Close() })
	return map[string]*cloudquotas.Client{"grpc": g, "rest": r}, map[string]*cloudquotas.QuotaAdjusterSettingsManagerClient{"grpc": ag, "rest": ar}
}

// quotaCode is err's canonical code, from a gRPC status or from a JSON
// error's "status".
func quotaCode(err error) codes.Code {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		var body struct {
			Error struct{ Status string } `json:"error"`
		}
		_ = json.Unmarshal([]byte(ge.Body), &body)
		for c := codes.OK; c <= codes.Unauthenticated; c++ {
			if strings.EqualFold(strings.ReplaceAll(body.Error.Status, "_", ""), c.String()) {
				return c
			}
		}
		return codes.Unknown
	}
	if s, ok := status.FromError(err); ok {
		return s.Code()
	}
	return codes.Unknown
}

// covers: google.api.cloudquotas.v1.CloudQuotas/ListQuotaInfos, google.api.cloudquotas.v1.CloudQuotas/GetQuotaInfo, google.api.cloudquotas.v1.CloudQuotas/CreateQuotaPreference, google.api.cloudquotas.v1.CloudQuotas/GetQuotaPreference, google.api.cloudquotas.v1.CloudQuotas/ListQuotaPreferences, google.api.cloudquotas.v1.CloudQuotas/UpdateQuotaPreference, google.api.cloudquotas.v1.QuotaAdjusterSettingsManager/GetQuotaAdjusterSettings, google.api.cloudquotas.v1.QuotaAdjusterSettingsManager/UpdateQuotaAdjusterSettings
//
// TestCloudQuotasLifecycle (#2030), with cloud.google.com/go/cloudquotas
// over gRPC and JSON: the served APIs' quota infos with Google's defaults,
// a preference created, read, listed, filtered and updated (granted at
// once), the safety checks, a fixed limit refused, and the quota
// adjuster's settings.
func TestCloudQuotasLifecycle(t *testing.T) {
	h := New(t)
	clients, adjusters := cloudQuotasClients(t, h)
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			ctx := h.Context()
			project := h.Project()
			parent := "projects/" + project + "/locations/global"
			var ids []string
			it := c.ListQuotaInfos(ctx, &pb.ListQuotaInfosRequest{Parent: parent + "/services/iam.googleapis.com"})
			for {
				qi, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, qi.GetQuotaId())
			}
			if len(ids) != 3 {
				t.Fatalf("iam.googleapis.com quota infos = %v", ids)
			}
			qi, err := c.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: parent + "/services/iam.googleapis.com/quotaInfos/" + saQuota})
			if err != nil {
				t.Fatal(err)
			}
			if qi.GetQuotaDisplayName() != "Service Account Count" || qi.GetDimensionsInfos()[0].GetDetails().GetValue() != 100 ||
				qi.GetContainerType() != pb.QuotaInfo_PROJECT || !strings.HasPrefix(qi.GetName(), "projects/") {
				t.Errorf("GetQuotaInfo = %v", qi)
			}
			if _, err := c.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: parent + "/services/iam.googleapis.com/quotaInfos/nope"}); quotaCode(err) != codes.NotFound {
				t.Errorf("unknown quota = %v", err)
			}
			id := "sa-" + transport
			pref := &pb.QuotaPreference{Service: "iam.googleapis.com", QuotaId: saQuota, QuotaConfig: &pb.QuotaConfig{PreferredValue: 50},
				Justification: "compat", ContactEmail: "dev@example.com"}
			if _, err := c.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: id, QuotaPreference: pref}); quotaCode(err) != codes.FailedPrecondition {
				t.Fatalf("a 50%% decrease without ignore_safety_checks = %v", err)
			}
			p, err := c.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: id, QuotaPreference: pref,
				IgnoreSafetyChecks: []pb.QuotaSafetyCheck{pb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH}})
			if err != nil {
				t.Fatal(err)
			}
			if p.GetReconciling() || p.GetQuotaConfig().GetGrantedValue().GetValue() != 50 || !strings.HasSuffix(p.GetName(), "/quotaPreferences/"+id) || p.GetEtag() == "" {
				t.Errorf("created = %v", p)
			}
			got, err := c.GetQuotaPreference(ctx, &pb.GetQuotaPreferenceRequest{Name: parent + "/quotaPreferences/" + id})
			if err != nil || got.GetJustification() != "compat" {
				t.Fatalf("get = %v, %v", got, err)
			}
			qi, _ = c.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: parent + "/services/iam.googleapis.com/quotaInfos/" + saQuota})
			if v := qi.GetDimensionsInfos()[0].GetDetails().GetValue(); v != 50 {
				t.Errorf("QuotaInfo value after the preference = %d, want 50", v)
			}
			up, err := c.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"quota_config.preferred_value"}},
				QuotaPreference: &pb.QuotaPreference{Name: parent + "/quotaPreferences/" + id, QuotaConfig: &pb.QuotaConfig{PreferredValue: 100}}})
			if err != nil || up.GetQuotaConfig().GetGrantedValue().GetValue() != 100 {
				t.Fatalf("update = %v, %v", up, err)
			}
			// allow_missing creates; a regional preference applies to its region.
			reg, err := c.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{AllowMissing: true, QuotaPreference: &pb.QuotaPreference{
				Name: parent + "/quotaPreferences/ca-" + transport, Service: "privateca.googleapis.com", QuotaId: "CAS-per-project-region",
				Dimensions: map[string]string{"region": "us-east1"}, QuotaConfig: &pb.QuotaConfig{PreferredValue: 1500}}})
			if err != nil || reg.GetDimensions()["region"] != "us-east1" {
				t.Fatalf("allow_missing = %v, %v", reg, err)
			}
			n := 0
			pit := c.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: parent, Filter: "service=privateca.googleapis.com"})
			for {
				_, err := pit.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				n++
			}
			if n < 1 {
				t.Errorf("filtered list found %d", n)
			}
			if _, err := c.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreference: &pb.QuotaPreference{
				Service: "workflows.googleapis.com", QuotaId: "WORKFLOWS-per-project", QuotaConfig: &pb.QuotaConfig{PreferredValue: 20000}}}); quotaCode(err) != codes.InvalidArgument {
				t.Errorf("a fixed limit = %v", err)
			}
			a := adjusters[transport]
			set, err := a.UpdateQuotaAdjusterSettings(ctx, &pb.UpdateQuotaAdjusterSettingsRequest{QuotaAdjusterSettings: &pb.QuotaAdjusterSettings{
				Name: parent + "/quotaAdjusterSettings", Enablement: pb.QuotaAdjusterSettings_ENABLED}})
			if err != nil {
				t.Fatal(err)
			}
			if s, err := a.GetQuotaAdjusterSettings(ctx, &pb.GetQuotaAdjusterSettingsRequest{Name: parent + "/quotaAdjusterSettings"}); err != nil ||
				s.GetEnablement() != pb.QuotaAdjusterSettings_ENABLED || s.GetEtag() != set.GetEtag() {
				t.Errorf("adjuster settings = %v, %v", s, err)
			}
		})
	}
}

// TestCloudQuotasPreferenceIsEnforced (#2030): a granted Service Account
// Count preference is the limit IAM's CreateServiceAccount enforces, and a
// decrease below the project's accounts is refused by the safety check.
func TestCloudQuotasPreferenceIsEnforced(t *testing.T) {
	h := New(t)
	clients, _ := cloudQuotasClients(t, h)
	c := clients["grpc"]
	ctx := h.Context()
	project := iamProject(t, h, "quota-")
	iam, err := admin.NewIamClient(ctx, rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iam.Close() })
	create := func(i int) error {
		_, err := iam.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project,
			AccountId: fmt.Sprintf("quota-%d-%d", i, time.Now().UnixNano()%1e6)})
		return err
	}
	for i := 0; i < 2; i++ {
		if err := create(i); err != nil {
			t.Fatal(err)
		}
	}
	live := 0
	sit := iam.ListServiceAccounts(ctx, &adminpb.ListServiceAccountsRequest{Name: "projects/" + project})
	for {
		_, err := sit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		live++
	}
	parent := "projects/" + project + "/locations/global"
	all := []pb.QuotaSafetyCheck{pb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH}
	below := &pb.QuotaPreference{Service: "iam.googleapis.com", QuotaId: saQuota, QuotaConfig: &pb.QuotaConfig{PreferredValue: int64(live - 1)}}
	if _, err := c.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "sa-limit", QuotaPreference: below,
		IgnoreSafetyChecks: all}); quotaCode(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "QUOTA_DECREASE_BELOW_USAGE") {
		t.Fatalf("a decrease below %d accounts = %v", live, err)
	}
	if _, err := c.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "sa-limit",
		QuotaPreference:    &pb.QuotaPreference{Service: "iam.googleapis.com", QuotaId: saQuota, QuotaConfig: &pb.QuotaConfig{PreferredValue: int64(live + 1)}},
		IgnoreSafetyChecks: all}); err != nil {
		t.Fatal(err)
	}
	if err := create(10); err != nil {
		t.Fatalf("the account the preference allows: %v", err)
	}
	err = create(11)
	if quotaCode(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), fmt.Sprintf("(%d)", live+1)) {
		t.Fatalf("an account past the preference = %v, want RESOURCE_EXHAUSTED naming %d", err, live+1)
	}
	// Raised back, the next account is allowed.
	if _, err := c.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{QuotaPreference: &pb.QuotaPreference{Name: parent + "/quotaPreferences/sa-limit",
		QuotaConfig: &pb.QuotaConfig{PreferredValue: 100}}}); err != nil {
		t.Fatal(err)
	}
	if err := create(12); err != nil {
		t.Errorf("after raising the preference: %v", err)
	}
}

// TestGcloudQuotas (#2030): gcloud quotas info list and describe, and
// quotas preferences create, describe, list and update, against the
// cloudquotas override on the Resource Manager port.
func TestGcloudQuotas(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	g := newGcloudSession(t, h)
	project := h.Project()
	if out := g.must("quotas", "info", "list", "--service=iam.googleapis.com", "--project="+project, "--format=value(quotaId)"); !strings.Contains(out, saQuota) {
		t.Errorf("quotas info list:\n%s", out)
	}
	if out := g.must("quotas", "info", "describe", saQuota, "--service=iam.googleapis.com", "--project="+project, "--format=value(quotaDisplayName)"); strings.TrimSpace(out) != "Service Account Count" {
		t.Errorf("quotas info describe: %q", out)
	}
	g.must("quotas", "preferences", "create", "--preference-id=gcloud-ca", "--service=privateca.googleapis.com", "--quota-id=CAS-per-project-region",
		"--preferred-value=1200", "--dimensions=region=us-central1", "--project="+project, "--justification=compat")
	if out := g.must("quotas", "preferences", "describe", "gcloud-ca", "--project="+project, "--format=value(quotaConfig.grantedValue)"); strings.TrimSpace(out) != "1200" {
		t.Errorf("preferences describe grantedValue = %q", out)
	}
	if out := g.must("quotas", "preferences", "list", "--project="+project, "--format=value(name)"); !strings.Contains(out, "/quotaPreferences/gcloud-ca") {
		t.Errorf("preferences list:\n%s", out)
	}
	g.must("quotas", "preferences", "update", "gcloud-ca", "--service=privateca.googleapis.com", "--quota-id=CAS-per-project-region",
		"--preferred-value=1300", "--dimensions=region=us-central1", "--project="+project)
	if out := g.must("quotas", "preferences", "describe", "gcloud-ca", "--project="+project, "--format=value(quotaConfig.preferredValue)"); strings.TrimSpace(out) != "1300" {
		t.Errorf("after update preferredValue = %q", out)
	}
}

// TestTerraformCloudQuotasPreference (#2030): google_cloud_quotas_quota_preference
// applies, plans clean, updates in place, and the API reads it back.
func TestTerraformCloudQuotasPreference(t *testing.T) {
	h := New(t)
	clients, _ := cloudQuotasClients(t, h)
	c := clients["rest"]
	m := newTFModule(t)
	project := h.Project()
	module := func(v int) string {
		return fmt.Sprintf(`
resource "google_cloud_quotas_quota_preference" "p" {
  parent        = "projects/%s"
  name          = "tf-ca"
  service       = "privateca.googleapis.com"
  quota_id      = "CAS-per-project-region"
  dimensions    = { region = "us-west1" }
  justification = "compat"
  quota_config {
    preferred_value = %d
  }
}
`, project, v)
	}
	m.write(module(1100))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()
	m.write(module(1400))
	m.apply()
	p, err := c.GetQuotaPreference(h.Context(), &pb.GetQuotaPreferenceRequest{Name: "projects/" + project + "/locations/global/quotaPreferences/tf-ca"})
	if err != nil || p.GetQuotaConfig().GetGrantedValue().GetValue() != 1400 {
		t.Fatalf("after the update: %v, %v", p, err)
	}
	m.planClean()
}
