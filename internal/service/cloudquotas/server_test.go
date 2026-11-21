package cloudquotas

import (
	"context"
	"strings"
	"testing"

	pb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/quotalimit"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const parent = "projects/p1/locations/global"

func TestQuotaInfosAndPreferences(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory(), nil, nil)
	infos, err := s.ListQuotaInfos(ctx, &pb.ListQuotaInfosRequest{Parent: parent + "/services/iam.googleapis.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(infos.GetQuotaInfos()) != 3 {
		t.Fatalf("iam quotas = %d, want 3", len(infos.GetQuotaInfos()))
	}
	qi, err := s.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: parent + "/services/iam.googleapis.com/quotaInfos/SERVICE-ACCOUNT-COUNT-per-project"})
	if err != nil || qi.GetDimensionsInfos()[0].GetDetails().GetValue() != 100 || qi.GetContainerType() != pb.QuotaInfo_PROJECT {
		t.Fatalf("GetQuotaInfo = %v, %v", qi, err)
	}
	// A decrease of more than 10% is refused without its safety check.
	in := &pb.QuotaPreference{Service: "iam.googleapis.com", QuotaId: "SERVICE-ACCOUNT-COUNT-per-project", QuotaConfig: &pb.QuotaConfig{PreferredValue: 5}}
	if _, err := s.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "sa", QuotaPreference: in}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("large decrease = %v, want FAILED_PRECONDITION", err)
	}
	quotalimit.RegisterUsage("iam.googleapis.com", "SERVICE-ACCOUNT-COUNT-per-project", func(string, map[string]string) (int64, error) { return 7, nil })
	t.Cleanup(func() { quotalimit.RegisterUsage("iam.googleapis.com", "SERVICE-ACCOUNT-COUNT-per-project", nil) })
	if _, err := s.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "sa", QuotaPreference: in,
		IgnoreSafetyChecks: []pb.QuotaSafetyCheck{pb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("decrease below usage = %v, want FAILED_PRECONDITION", err)
	}
	p, err := s.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "sa", QuotaPreference: in,
		IgnoreSafetyChecks: []pb.QuotaSafetyCheck{pb.QuotaSafetyCheck_QUOTA_DECREASE_PERCENTAGE_TOO_HIGH, pb.QuotaSafetyCheck_QUOTA_DECREASE_BELOW_USAGE}})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetReconciling() || p.GetQuotaConfig().GetGrantedValue().GetValue() != 5 || p.GetEtag() == "" {
		t.Errorf("created = %v", p)
	}
	if v := quotalimit.Limit("p1", "iam.googleapis.com", "SERVICE-ACCOUNT-COUNT-per-project", nil, 100); v != 100 {
		t.Errorf("Limit without a source = %d", v)
	}
	quotalimit.Use(s)
	t.Cleanup(func() { quotalimit.Use(nil) })
	if v := quotalimit.Limit("p1", "iam.googleapis.com", "SERVICE-ACCOUNT-COUNT-per-project", nil, 100); v != 5 {
		t.Errorf("Limit = %d, want the granted 5", v)
	}
	qi, _ = s.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: parent + "/services/iam.googleapis.com/quotaInfos/SERVICE-ACCOUNT-COUNT-per-project"})
	if qi.GetDimensionsInfos()[0].GetDetails().GetValue() != 5 {
		t.Errorf("QuotaInfo after the preference = %v", qi)
	}
	if _, err := s.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "sa", QuotaPreference: in}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate = %v", err)
	}
	// An increase is granted; update needs a known etag or none.
	up, err := s.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"quota_config.preferred_value"}},
		QuotaPreference: &pb.QuotaPreference{Name: parent + "/quotaPreferences/sa", QuotaConfig: &pb.QuotaConfig{PreferredValue: 200}}})
	if err != nil || up.GetQuotaConfig().GetGrantedValue().GetValue() != 200 {
		t.Fatalf("update = %v, %v", up, err)
	}
	if _, err := s.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{QuotaPreference: &pb.QuotaPreference{Name: parent + "/quotaPreferences/sa",
		Etag: "stale", QuotaConfig: &pb.QuotaConfig{PreferredValue: 210}}}); status.Code(err) != codes.Aborted {
		t.Errorf("stale etag = %v", err)
	}
	// A regional preference, created by update with allow_missing.
	if _, err := s.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{AllowMissing: true, QuotaPreference: &pb.QuotaPreference{
		Name: parent + "/quotaPreferences/ca-east", Service: "privateca.googleapis.com", QuotaId: "CAS-per-project-region",
		Dimensions: map[string]string{"region": "us-east1"}, QuotaConfig: &pb.QuotaConfig{PreferredValue: 2000}}}); err != nil {
		t.Fatal(err)
	}
	if v := quotalimit.Limit("p1", "privateca.googleapis.com", "CAS-per-project-region", map[string]string{"region": "us-east1"}, 1000); v != 2000 {
		t.Errorf("regional limit = %d", v)
	}
	if v := quotalimit.Limit("p1", "privateca.googleapis.com", "CAS-per-project-region", map[string]string{"region": "us-west1"}, 1000); v != 1000 {
		t.Errorf("another region's limit = %d", v)
	}
	list, err := s.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: parent, Filter: "service=privateca.googleapis.com"})
	if err != nil || len(list.GetQuotaPreferences()) != 1 {
		t.Fatalf("filtered list = %v, %v", list, err)
	}
	all, _ := s.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: parent, PageSize: 1})
	if len(all.GetQuotaPreferences()) != 1 || all.GetNextPageToken() == "" {
		t.Fatalf("first page = %v", all)
	}
	next, err := s.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: parent, PageSize: 1, PageToken: all.GetNextPageToken()})
	if err != nil || len(next.GetQuotaPreferences()) != 1 || next.GetQuotaPreferences()[0].GetName() == all.GetQuotaPreferences()[0].GetName() {
		t.Fatalf("second page = %v, %v", next, err)
	}
	// A fixed limit and an unknown quota are refused.
	for _, q := range []*pb.QuotaPreference{
		{Service: "workflows.googleapis.com", QuotaId: "WORKFLOWS-per-project", QuotaConfig: &pb.QuotaConfig{PreferredValue: 20000}},
		{Service: "iam.googleapis.com", QuotaId: "nope", QuotaConfig: &pb.QuotaConfig{PreferredValue: 1}},
	} {
		if _, err := s.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreference: q}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v, want INVALID_ARGUMENT", q.GetQuotaId(), err)
		}
	}
	if _, err := s.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: "projects/p1/locations/us-east1"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("non-global location = %v", err)
	}
	if _, err := s.ListQuotaPreferences(ctx, &pb.ListQuotaPreferencesRequest{Parent: "folders/1/locations/global"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("folder = %v", err)
	}
	adj := s.Adjuster()
	set, err := adj.UpdateQuotaAdjusterSettings(ctx, &pb.UpdateQuotaAdjusterSettingsRequest{QuotaAdjusterSettings: &pb.QuotaAdjusterSettings{
		Name: parent + "/quotaAdjusterSettings", Enablement: pb.QuotaAdjusterSettings_ENABLED}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := adj.GetQuotaAdjusterSettings(ctx, &pb.GetQuotaAdjusterSettingsRequest{Name: parent + "/quotaAdjusterSettings"})
	if err != nil || got.GetEnablement() != pb.QuotaAdjusterSettings_ENABLED || got.GetEtag() != set.GetEtag() {
		t.Errorf("adjuster = %v, %v", got, err)
	}
	if err := s.ResetProject("p1"); err != nil {
		t.Fatal(err)
	}
	if v := quotalimit.Limit("p1", "iam.googleapis.com", "SERVICE-ACCOUNT-COUNT-per-project", nil, 100); v != 100 {
		t.Errorf("after reset = %d", v)
	}
}

// TestProvidedQuotasAndNumberedProjects (#2090): a provider's quotas are
// QuotaInfos at its per-project value, a preference for one is granted and
// is quotalimit's value for the project by ID or by number, and the
// preference's name, which names a project that was never created by its
// number, reads and updates it.
func TestProvidedQuotasAndNumberedProjects(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory(), nil, nil)
	s.AddQuotas(func(project string) []Quota {
		v := int64(1200)
		if project == "low" {
			v = 600
		}
		return []Quota{{Service: "parametermanager.googleapis.com", QuotaID: "ReadRequestsPerMinutePerProject",
			Metric: "parametermanager.googleapis.com/read_requests", MetricUnit: "1/min/{project}", Default: v, RefreshInterval: "minute", Enforced: true}}
	})
	name := "/locations/global/services/parametermanager.googleapis.com/quotaInfos/ReadRequestsPerMinutePerProject"
	for project, want := range map[string]int64{"p1": 1200, "low": 600} {
		qi, err := s.GetQuotaInfo(ctx, &pb.GetQuotaInfoRequest{Name: "projects/" + project + name})
		if err != nil || qi.GetDimensionsInfos()[0].GetDetails().GetValue() != want || qi.GetRefreshInterval() != "minute" {
			t.Fatalf("%s: GetQuotaInfo = %v, %v", project, qi, err)
		}
	}
	p, err := s.CreateQuotaPreference(ctx, &pb.CreateQuotaPreferenceRequest{Parent: parent, QuotaPreferenceId: "reads",
		QuotaPreference: &pb.QuotaPreference{Service: "parametermanager.googleapis.com", QuotaId: "ReadRequestsPerMinutePerProject",
			Justification: "unit", QuotaConfig: &pb.QuotaConfig{PreferredValue: 1100}}})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Value("p1", "parametermanager.googleapis.com", "ReadRequestsPerMinutePerProject", nil); !ok || v != 1100 {
		t.Errorf("Value(p1) = %d, %v", v, ok)
	}
	number := strings.TrimPrefix(strings.Split(p.GetName(), "/locations/")[0], "projects/")
	if v, ok := s.Value(number, "parametermanager.googleapis.com", "ReadRequestsPerMinutePerProject", nil); !ok || v != 1100 {
		t.Errorf("Value(%s) = %d, %v", number, v, ok)
	}
	p.QuotaConfig = &pb.QuotaConfig{PreferredValue: 1050}
	if _, err := s.UpdateQuotaPreference(ctx, &pb.UpdateQuotaPreferenceRequest{QuotaPreference: p,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"quota_config.preferred_value"}}}); err != nil {
		t.Fatalf("UpdateQuotaPreference by the answered name %s: %v", p.GetName(), err)
	}
	if v, _ := s.Value("p1", "parametermanager.googleapis.com", "ReadRequestsPerMinutePerProject", nil); v != 1050 {
		t.Errorf("after the update, Value = %d", v)
	}
}
