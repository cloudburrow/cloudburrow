//go:build browser

package browser

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
)

// TestFirestoreOnMonitoringChartsItsCallsAndContainer: /monitoring named Firestore "not
// measured", and charted nothing of it, because its calls went over a
// tunnel nothing read (#1267). A document written with the official client
// through the host tunnel is now counted, so /monitoring draws Firestore's
// request, error and latency charts, its pod's container CPU and memory
// beside them, and no longer lists it as not measured.
func TestFirestoreOnMonitoringChartsItsCallsAndContainer(t *testing.T) {
	needService(t, "firestore")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the calls are made through the emulator's tunnel", envFirestore)
	}
	p := open(t)
	project := uniqueProject(t)
	ctx := context.Background()
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	client, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Collection("monitored1267").Doc("d").Set(ctx, map[string]any{"n": 1}); err != nil {
		t.Fatalf("Set with the official client: %v", err)
	}

	// The request charts draw from samples taken on the server's own tick,
	// and the page draws once: it is reloaded until they are there.
	const charted = `(() => {
		const titles = [...document.querySelectorAll("#request-metrics .chart-card h3")].map((h) => h.textContent);
		return ["firestore: requests", "firestore: errors", "firestore: latency",
		        "firestore: container CPU", "firestore: container memory"].every((t) => titles.includes(t));
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
			t.Fatalf("/monitoring charts %v; want Firestore's requests, errors, latency and container charts", titles)
		}
		time.Sleep(3 * time.Second)
	}
	var unmeasured []string
	p.eval(`[...document.querySelectorAll("#request-metrics .unmeasured li")].map((li) => li.textContent)`, &unmeasured)
	for _, u := range unmeasured {
		if strings.HasPrefix(u, "firestore:") {
			t.Errorf("Firestore is still listed as not measured: %q", u)
		}
	}
}
