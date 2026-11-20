package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
)

// TestGKECredentials (#1879, round 13): a cluster's credentials come back
// only to a caller holding container.clusters.getCredentials (the
// instance's own account, here); anyone else gets the cluster without its
// password and client key and certificate, its CA certificate kept.
func TestGKECredentials(t *testing.T) {
	e := &iameval.Enforcer{Eval: &iameval.Evaluator{}, On: func() bool { return true },
		CallerOf: func(b string) iameval.Caller { return iameval.Caller{Email: b} }}
	cluster := &containerpb.Cluster{Name: "c", SelfLink: "https://container.googleapis.com/v1/projects/p/locations/us-central1/clusters/c",
		MasterAuth: &containerpb.MasterAuth{ClusterCaCertificate: "ca", ClientCertificate: "cert", ClientKey: "key", Password: "pw", Username: "admin"}}
	check := gkeCredentials(e)
	call := func(bearer string) *containerpb.Cluster {
		ctx := context.Background()
		if bearer != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+bearer))
		}
		out, err := check(ctx, &containerpb.GetClusterRequest{}, &grpc.UnaryServerInfo{FullMethod: "/google.container.v1.ClusterManager/GetCluster"},
			func(context.Context, any) (any, error) { return proto.Clone(cluster), nil })
		if err != nil {
			t.Fatal(err)
		}
		return out.(*containerpb.Cluster)
	}
	if a := call("").GetMasterAuth(); a.GetClientKey() != "key" || a.GetPassword() != "pw" {
		t.Errorf("the instance's account got %+v; want the credentials", a)
	}
	if a := call("viewer@p.iam.gserviceaccount.com").GetMasterAuth(); a.GetClientKey() != "" || a.GetClientCertificate() != "" ||
		a.GetPassword() != "" || a.GetClusterCaCertificate() != "ca" {
		t.Errorf("a caller without getCredentials got %+v; want only the CA certificate", a)
	}

	h := gkeCredentialsJSON(e, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"clusters": []any{map[string]any{"name": "c", "selfLink": cluster.SelfLink,
			"masterAuth": map[string]any{"clusterCaCertificate": "ca", "clientKey": "key"}}}})
	}))
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/p/locations/-/clusters", nil)
	r.Header.Set("Authorization", "Bearer viewer@p.iam.gserviceaccount.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out struct {
		Clusters []struct {
			MasterAuth map[string]string `json:"masterAuth"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Clusters) != 1 ||
		out.Clusters[0].MasterAuth["clientKey"] != "" || out.Clusters[0].MasterAuth["clusterCaCertificate"] != "ca" {
		t.Errorf("ListClusters over JSON = %s; want the client key removed", w.Body.String())
	}
}
