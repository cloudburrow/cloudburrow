//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	redisadmin "cloud.google.com/go/redis/apiv1"
	"cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The Memorystore admin API's endpoint (#1302) and the project its
// instances are in, as the compat harness names them.
const (
	envMemorystoreAdmin   = "CLOUDBURROW_TEST_MEMORYSTORE_ADMIN"
	envMemorystoreProject = "CLOUDBURROW_TEST_MEMORYSTORE_PROJECT"
)

// TestMemorystoreRotateServerCAThroughThePage (#1463), in the emulators
// shard. An instance made with in-transit encryption, through the official
// client, shows its Server CA tab and Rotate server CA. Clicked, the
// rotation is not sent until the instance's name is typed back, as the
// action's confirmation promises: a wrong name is refused with nothing
// sent. Confirmed, GetInstance lists a second server CA beside the first,
// and the Server CA tab shows both.
func TestMemorystoreRotateServerCAThroughThePage(t *testing.T) {
	needService(t, "memorystore")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: the rotation is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx := context.Background()
	c, err := redisadmin.NewCloudRedisClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	id := fmt.Sprintf("browser-rot-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/us-central1", InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance with in-transit encryption: %v", err)
	}
	if len(before.GetServerCaCerts()) != 1 {
		t.Fatalf("a new instance lists %d server CAs, want 1", len(before.GetServerCaCerts()))
	}

	p := open(t)
	q := "?project=" + project
	p.navigate("/memorystore" + q)
	p.clickText("#view tbody a", id)
	p.waitFor(`document.querySelector("#tab-server-ca") !== null`)
	p.clickText("#view .page-actions button", "Rotate server CA")

	// Confirmed by the instance's name before anything is sent.
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	var detail string
	p.eval(`document.querySelector(".modal .confirm-detail").textContent`, &detail)
	if !strings.Contains(detail, "A new server CA is listed beside the current one") {
		t.Errorf("the Rotate server CA confirmation reads %q; want what the rotation does", detail)
	}
	p.confirmByName("wrong")
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	if sent := p.sent(http.MethodPost, "/api/actions/memorystore"); len(sent) != 0 {
		t.Fatalf("Rotate server CA confirmed with the wrong name was sent: %v", sent)
	}
	p.eval(`(() => { document.querySelector(".modal #confirm-input").value = ""; return true; })()`, nil)
	p.confirmByName(id)
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPost, "/api/actions/memorystore"); len(sent) != 1 {
		t.Fatalf("the confirmed Rotate server CA sent %d requests, want 1: %v", len(sent), sent)
	}
	after, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || len(after.GetServerCaCerts()) != 2 ||
		after.GetServerCaCerts()[1].GetSerialNumber() != before.GetServerCaCerts()[0].GetSerialNumber() {
		t.Fatalf("after Rotate server CA, GetInstance's server_ca_certs = %v, %v; want a new CA beside the first", after.GetServerCaCerts(), err)
	}

	// The Server CA tab lists both.
	p.navigate("/memorystore" + q)
	p.clickText("#view tbody a", id)
	p.waitFor(`document.querySelector("#tab-server-ca") !== null`)
	p.clickText("#tab-server-ca", "Server CA")
	var tab string
	p.eval(`[...document.querySelectorAll("#view [role=tabpanel]")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &tab)
	for _, ca := range after.GetServerCaCerts() {
		if !strings.Contains(tab, "Serial "+ca.GetSerialNumber()) {
			t.Errorf("the Server CA tab reads %q; want serial %s", tab, ca.GetSerialNumber())
		}
	}
}
