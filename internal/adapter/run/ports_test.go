package run

import (
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// A port named h2c (#1475) is Knative's h2c port, served end to end over
// HTTP/2 cleartext, and reads back named; any other name is refused.
func TestServicePortNameH2C(t *testing.T) {
	t.Parallel()
	s := svc("grpc", &runpb.Container{Image: "gcr.io/p/app:v1", Ports: []*runpb.ContainerPort{{Name: "h2c", ContainerPort: 50051}}})
	m, err := ToKnative(s, "cloudburrow", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m, "- containerPort: 50051\n              name: h2c\n") {
		t.Errorf("manifest does not name the port h2c:\n%s", m)
	}
	s.Template.Containers[0].Ports[0].Name = "grpc"
	if _, err := ToKnative(s, "cloudburrow", "test", nil); err == nil {
		t.Error("port name grpc accepted")
	}
}
