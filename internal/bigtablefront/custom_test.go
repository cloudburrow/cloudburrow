package bigtablefront

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// customSource enforces its custom constraints, of the resource types
// they name, on every project.
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

// TestCustomConstraints (#1895): the page's examples, a Cluster outside
// the US (ALLOW resource.location.contains('/locations/us-')) and a table
// without deletion protection (ALLOW resource.deletionProtection ==
// true), are refused FAILED_PRECONDITION "Operation denied by custom org
// policy: [...]", in an instance's own clusters too; allowed ones are
// created; a field the page does not list cannot be read.
func TestCustomConstraints(t *testing.T) {
	orgpolicy.Use(customSource{
		orgpolicy.TypeBigtableCluster:  {enforcedCustom("custom.usClusters", "resource.location.contains('/locations/us-')", "ALLOW", "Clusters in the US.")},
		orgpolicy.TypeBigtableTable:    {enforcedCustom("custom.protected", "resource.deletionProtection == true", "ALLOW", "Protected tables.")},
		orgpolicy.TypeBigtableInstance: {enforcedCustom("custom.nolabels", "has(resource.labels)", "DENY", "Labels are not a documented field.")},
	})
	defer orgpolicy.Use(nil)
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := dial(t, addr)
	iac, err := bigtable.NewInstanceAdminClient(ctx, "p", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	const want = `Operation denied by custom org policy: ["customConstraints/custom.usClusters":"Clusters in the US."]`
	err = iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "eu-inst", DisplayName: "EU instance",
		ClusterId: "eu-inst-c1", Zone: "europe-west1-b", NumNodes: 1})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != want {
		t.Errorf("an instance with a cluster in Europe = %v, want FAILED_PRECONDITION %s", err, want)
	}
	// has(resource.labels) cannot be true: labels are not a documented
	// field of an instance.
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: "us-inst", DisplayName: "US instance",
		ClusterId: "us-inst-c1", Zone: "us-central1-b", NumNodes: 1, Labels: map[string]string{"a": "b"}}); err != nil {
		t.Fatalf("an instance in the US: %v", err)
	}
	err = iac.CreateCluster(ctx, &bigtable.ClusterConfig{InstanceID: "us-inst", ClusterID: "us-inst-eu", Zone: "europe-west1-c", NumNodes: 1})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != want {
		t.Errorf("a cluster in Europe = %v", err)
	}
	ac, err := bigtable.NewAdminClient(ctx, "p", "us-inst", option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	err = ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "open"})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition ||
		st.Message() != `Operation denied by custom org policy: ["customConstraints/custom.protected":"Protected tables."]` {
		t.Errorf("a table without deletion protection = %v", err)
	}
	if err := ac.CreateTableFromConf(ctx, &bigtable.TableConf{TableID: "kept", DeletionProtection: bigtable.Protected}); err != nil {
		t.Errorf("a protected table: %v", err)
	}
}
