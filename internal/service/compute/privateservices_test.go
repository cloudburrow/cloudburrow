package compute

import (
	"strings"
	"testing"
)

// TestWithPrivateServicesExtraPorts (#2141): an endpoint's further ports,
// a Cloud SQL instance's pooler's 6432, are sent on to the same port of
// its target beside its server's port.
func TestWithPrivateServicesExtraPorts(t *testing.T) {
	fw := withPrivateServices("table inet cloudburrow {\n}\n", []PrivateService{
		{Name: "cloudsql a", IP: "10.1.0.5", Port: 5432, TargetIP: "10.96.0.9", TargetPort: 5432, ExtraPorts: []int{6432}},
		{Name: "psc ep", IP: "10.128.0.7", Port: 3306, TargetIP: "10.96.0.10", TargetPort: 3306},
	})
	for _, want := range []string{
		"ip daddr 10.1.0.5 tcp dport 5432 dnat ip to 10.96.0.9:5432",
		"ip daddr 10.1.0.5 tcp dport 6432 dnat ip to 10.96.0.9:6432",
		"ip daddr 10.128.0.7 tcp dport 3306 dnat ip to 10.96.0.10:3306",
	} {
		if !strings.Contains(fw, want) {
			t.Errorf("no %q in:\n%s", want, fw)
		}
	}
	if strings.Contains(fw, "10.128.0.7 tcp dport 6432") {
		t.Errorf("an endpoint without a pooler serves 6432:\n%s", fw)
	}
}
