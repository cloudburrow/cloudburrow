package spannerfront

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

type customSource map[string][]orgpolicy.CustomConstraint

func (customSource) Effective(context.Context, string, string) (orgpolicy.Effective, error) {
	return orgpolicy.Effective{}, nil
}

func (s customSource) Customs(_ context.Context, _, resourceType, _ string) ([]orgpolicy.CustomConstraint, error) {
	return s[resourceType], nil
}

func enforcedCustom(name, condition, action, desc string) orgpolicy.CustomConstraint {
	return orgpolicy.CustomConstraint{Name: name, Condition: condition, ActionType: action, Description: desc,
		Effective: orgpolicy.AsResponse(orgpolicy.Effective{Spec: &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
			{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}})}
}

// TestCustomConstraints (#1895): the page's example (DENY
// resource.name.contains('denied-database-name')) refuses CreateDatabase
// PERMISSION_DENIED with the page's message; a constraint on
// spanner.googleapis.com/InstanceConfig refuses a create and an update
// FAILED_PRECONDITION "Operation denied by custom org policies: [...]".
func TestCustomConstraints(t *testing.T) {
	orgpolicy.Use(customSource{
		orgpolicy.TypeSpannerDatabase:       {enforcedCustom("custom.spannerDatabase", "resource.name.contains('denied-database-name')", "DENY", "No denied names.")},
		orgpolicy.TypeSpannerInstanceConfig: {enforcedCustom("custom.cfgName", "resource.displayName.startsWith('ok')", "ALLOW", "Names start with ok.")},
	})
	defer orgpolicy.Use(nil)
	ctx := context.Background()
	s := &Instances{configs: map[string]*instancepb.InstanceConfig{}, now: time.Now, ops: map[string]*longrunningpb.Operation{}}
	_, err := databases{s: s}.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: "projects/p/instances/i",
		CreateStatement: "CREATE DATABASE `denied-database-name100`"})
	if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || st.Message() != databaseDenied {
		t.Errorf("CreateDatabase of a denied name = %v", err)
	}
	base := managedConfig("projects/p", "nam3")
	create := func(display string) error {
		_, err := s.CreateInstanceConfig(ctx, &instancepb.CreateInstanceConfigRequest{Parent: "projects/p", InstanceConfigId: "custom-" + display,
			InstanceConfig: &instancepb.InstanceConfig{DisplayName: display, BaseConfig: base.GetName(), Replicas: base.GetReplicas()}})
		return err
	}
	const want = `Operation denied by custom org policies: ["customConstraints/custom.cfgName":"Names start with ok."]`
	if err := create("badname"); status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != want {
		t.Errorf("a configuration the constraint refuses = %v", err)
	}
	if err := create("okname"); err != nil {
		t.Fatalf("a configuration the constraint allows: %v", err)
	}
	_, err = s.UpdateInstanceConfig(ctx, &instancepb.UpdateInstanceConfigRequest{InstanceConfig: &instancepb.InstanceConfig{
		Name: "projects/p/instanceConfigs/custom-okname", DisplayName: "renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("renaming it against the constraint = %v", err)
	}
	if got := s.configs["projects/p/instanceConfigs/custom-okname"].GetDisplayName(); got != "okname" {
		t.Errorf("the refused update changed the display name to %q", got)
	}
}
