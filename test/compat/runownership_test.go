//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRunDeleteRefusesAnUnownedKnativeService (#1576): a Knative Service a
// developer made by hand in the adapter's namespace, with no
// cloudburrow.dev/owned label, is visible to the official ServicesClient
// under its name, and DeleteService refuses it with FAILED_PRECONDITION and
// leaves it in the cluster (internal/adapter/run/knative.go).
// covers: google.cloud.run.v2.Services/DeleteService
func TestRunDeleteRefusesAnUnownedKnativeService(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set: the hand-made Knative Service is applied with kubectl", envKubeconfig)
	}
	const id = "hand-made-ksvc"
	kubectl := func(stdin string, args ...string) (string, error) {
		cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", "default"}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	manifest := `apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: ` + id + `
spec:
  template:
    spec:
      containers:
        - image: ` + helloDigest + `
`
	if out, err := kubectl(manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = kubectl("", "delete", "ksvc", id, "--ignore-not-found", "--wait=false") })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := runParent(h) + "/services/" + id
	if _, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name}); err != nil {
		t.Fatalf("GetService of the hand-made service: %v", err)
	}
	_, err := c.DeleteService(ctx, &runpb.DeleteServiceRequest{Name: name})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not created by CloudBurrow") {
		t.Errorf("DeleteService of an unowned Knative Service = %v, want FAILED_PRECONDITION", err)
	}
	if out, err := kubectl("", "get", "ksvc", id, "-o", "name"); err != nil {
		t.Errorf("the hand-made Knative Service is gone after a refused delete: %v\n%s", err, out)
	}
}
