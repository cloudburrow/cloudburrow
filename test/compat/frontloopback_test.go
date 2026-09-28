//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
)

// TestEmulatorsBehindAFrontRefuseOtherPods (#1114): the BigQuery emulator
// (REST 9051, Storage gRPC 9061) and the Pub/Sub emulator (8086) listen on
// their pod's loopback alone, so another pod reaches them only through
// their fronts (9050, 9060, 8085), whose checks it cannot pass by. From the
// Pub/Sub emulator's container, whose image has bash, each port is dialled
// at its pod's IP (Pub/Sub's own included: a port that its own pod cannot
// reach at the pod's IP, no other pod can): the emulators' refuse, the
// fronts' accept. Measured
// first with the emulator on 0.0.0.0: from another pod, CreateReadSession
// of a table in a project the instance does not have, sent to 9061, which
// the front refuses NOT_FOUND, crashed the emulator and every dataset was
// gone. The fronts still serve: the official client lists datasets and a
// Pub/Sub topic is listed through each.
// covers: bigquery.datasets.list, google.pubsub.v1.Publisher/ListTopics
func TestEmulatorsBehindAFrontRefuseOtherPods(t *testing.T) {
	h := New(t)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set; the ports are dialled from a pod of the cluster", envKubeconfig)
	}
	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	ctx, cancel := context.WithTimeout(h.Context(), 2*time.Minute)
	defer cancel()
	kc := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", ns}, args...)...).CombinedOutput()
		return string(out), err
	}
	podIP := func(app string) string {
		out, err := kc("get", "pod", "-l", "app="+app, "-o", "json")
		if err != nil {
			t.Fatalf("kubectl get pod -l app=%s: %v\n%s", app, err, out)
		}
		var list struct {
			Items []struct {
				Metadata struct {
					DeletionTimestamp *string `json:"deletionTimestamp"`
				} `json:"metadata"`
				Status struct {
					PodIP string `json:"podIP"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			t.Fatalf("read the %s pods: %v", app, err)
		}
		for _, it := range list.Items {
			if it.Metadata.DeletionTimestamp == nil && it.Status.PodIP != "" {
				return it.Status.PodIP
			}
		}
		t.Fatalf("no running %s pod:\n%s", app, out)
		return ""
	}
	bq, ps := podIP("bigquery"), podIP("pubsub")
	for _, p := range []struct {
		what string
		addr string
		open bool
	}{
		{"the BigQuery emulator's REST port", bq + ":9051", false},
		{"the BigQuery emulator's Storage API port", bq + ":9061", false},
		{"the Pub/Sub emulator's port", ps + ":8086", false},
		{"the BigQuery front's REST port", bq + ":9050", true},
		{"the BigQuery front's Storage API port", bq + ":9060", true},
		{"the Pub/Sub front's port", ps + ":8085", true},
	} {
		host, port, _ := strings.Cut(p.addr, ":")
		script := fmt.Sprintf("if timeout 5 bash -c '</dev/tcp/%s/%s' 2>/dev/null; then echo open; else echo refused; fi", host, port)
		out, err := kc("exec", "deploy/pubsub", "-c", "pubsub", "--", "bash", "-c", script)
		if err != nil {
			t.Fatalf("kubectl exec into the Pub/Sub emulator's container: %v\n%s", err, out)
		}
		got := strings.TrimSpace(out)
		want := map[bool]string{true: "open", false: "refused"}[p.open]
		if got != want {
			t.Errorf("%s (%s) from another pod: %s, want %s", p.what, p.addr, got, want)
		}
	}

	c, _ := bigqueryClient(t, h)
	it := c.Datasets(h.Context())
	if _, err := it.Next(); err != nil && err != iterator.Done {
		t.Errorf("datasets.list through the front: %v", err)
	}
	pc := pubsubClient(t, h)
	tit := pc.TopicAdminClient.ListTopics(h.Context(), &pubsubpb.ListTopicsRequest{Project: "projects/" + h.Project()})
	if _, err := tit.Next(); err != nil && err != iterator.Done {
		t.Errorf("ListTopics through the front: %v", err)
	}
}
