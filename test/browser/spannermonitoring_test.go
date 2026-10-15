//go:build browser

package browser

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// envSpanner is the Spanner emulator's host endpoint, as the compat harness
// names it.
const envSpanner = "CLOUDBURROW_TEST_SPANNER"

// TestSpannerOnMonitoringChartsItsCalls: /monitoring named Spanner "not
// measured", because its port is a raw tunnel nothing read (#1267). The
// call front in its pod records its calls (#1288), so an instance listing
// with the official client is counted and /monitoring draws Spanner's
// request, error and latency charts beside its pod's container CPU and
// memory, and no longer lists it as not measured.
func TestSpannerOnMonitoringChartsItsCalls(t *testing.T) {
	needService(t, "spanner")
	addr := strings.TrimSpace(os.Getenv(envSpanner))
	if addr == "" {
		t.Skipf("%s is not set: the calls are made through the emulator's tunnel", envSpanner)
	}
	p := open(t)
	project := uniqueProject(t)
	ctx := context.Background()
	c, err := instance.NewInstanceAdminClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	it := c.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: "projects/" + project})
	if _, err := it.Next(); err != nil && err != iterator.Done {
		t.Fatalf("ListInstances with the official client: %v", err)
	}

	const charted = `(() => {
		const titles = [...document.querySelectorAll("#request-metrics .chart-card h3")].map((h) => h.textContent);
		return ["spanner: requests", "spanner: errors", "spanner: latency",
		        "spanner: container CPU", "spanner: container memory"].every((t) => titles.includes(t));
	})()`
	deadline := time.Now().Add(90 * time.Second)
	for {
		p.navigate("/monitoring")
		p.waitFor(`document.querySelector("#request-metrics") !== null`)
		var ok bool
		p.eval(charted, &ok)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			var titles []string
			p.eval(`[...document.querySelectorAll("#request-metrics .chart-card h3")].map((h) => h.textContent)`, &titles)
			t.Fatalf("/monitoring charts %v; want Spanner's requests, errors, latency and container charts", titles)
		}
		time.Sleep(3 * time.Second)
	}
	var unmeasured []string
	p.eval(`[...document.querySelectorAll("#request-metrics .unmeasured li")].map((li) => li.textContent)`, &unmeasured)
	for _, u := range unmeasured {
		if strings.HasPrefix(u, "spanner:") {
			t.Errorf("Spanner is still listed as not measured: %q", u)
		}
	}
}
