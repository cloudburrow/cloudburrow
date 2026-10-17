package logging

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestSettingsAndLinksOverREST (#1292): the official client's REST
// transport changes and reads the settings, and creates a link through its
// operation; the settings path Google binds ({name=*/*}/settings) carries
// projects/{p} alone, which is accepted.
func TestSettingsAndLinksOverREST(t *testing.T) {
	ctx := context.Background()
	a := NewAdmin(store.NewMemory())
	a.UseLinkedDatasets(newFakeLinked())
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { a.Register(g) }))
	defer srv.Close()
	c, err := vlogging.NewConfigRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resp, err := http.Get(srv.URL + "/v2/projects/proj-1/settings")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"projects/proj-1/settings"`) {
		t.Fatalf("GET settings = %d %s", resp.StatusCode, body)
	}
	// The JSON API's loggingServiceAccountId, which the proto has no field
	// for, is the Log Router account (#1304).
	var js struct{ KmsServiceAccountId, LoggingServiceAccountId string }
	if err := json.Unmarshal(body, &js); err != nil || js.LoggingServiceAccountId == "" || js.LoggingServiceAccountId != js.KmsServiceAccountId {
		t.Errorf("GET settings names the accounts %+v (%v); want loggingServiceAccountId, kmsServiceAccountId's", js, err)
	}
	if _, err := c.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: "projects/proj-1",
		Settings: &loggingpb.Settings{StorageLocation: "us-east1"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"storage_location"}}}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if st, err := c.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: "projects/proj-1"}); err != nil || st.GetStorageLocation() != "us-east1" {
		t.Fatalf("GetSettings = %v, %v", st, err)
	}
	bucket := "projects/proj-1/locations/global/buckets/b"
	if _, err := c.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/proj-1/locations/global", BucketId: "b",
		Bucket: &loggingpb.LogBucket{AnalyticsEnabled: true}}); err != nil {
		t.Fatal(err)
	}
	op, err := c.CreateLink(ctx, &loggingpb.CreateLinkRequest{Parent: bucket, LinkId: "l"})
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if l, err := op.Wait(ctx); err != nil || l.GetName() != bucket+"/links/l" {
		t.Fatalf("CreateLink.Wait = %v, %v", l, err)
	}
}
