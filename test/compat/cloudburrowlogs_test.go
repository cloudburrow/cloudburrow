//go:build compat

package compat

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestLogsReadsAnInProcessServicesRequestLines.
//
// `cloudburrow logs --service kms` against the CI instance prints Cloud KMS's
// request lines from up.log and exits 0. It once looked for a kms pod, which
// does not exist, and failed while the service was logging requests (#587).
// A failed call is used because it is logged at the default level.
func TestLogsReadsAnInProcessServicesRequestLines(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	c := kmsClients(t, h)["grpc"]
	name := "projects/" + h.Project() + "/locations/global/keyRings/cloudburrow-logs-absent"
	if _, err := c.GetKeyRing(h.Context(), &kmspb.GetKeyRingRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetKeyRing of an absent key ring: %v, want NotFound", err)
	}

	out, err := exec.Command(cli, append([]string{"logs", "--service", "kms", "--tail", "5"}, flags...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("cloudburrow logs --service kms: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "kms.GetKeyRing => NOT_FOUND") {
		t.Errorf("cloudburrow logs --service kms printed no GetKeyRing line:\n%s", out)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.Contains(l, " kms.") {
			t.Errorf("a line that is not Cloud KMS's: %s", l)
		}
	}
}
