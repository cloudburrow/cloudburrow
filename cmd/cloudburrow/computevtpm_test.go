package main

import (
	"reflect"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

// The vTPM container stops on SIGTERM, not on the guest image's
// SIGRTMIN+3, which swtpm ignores: otherwise every stop of a Shielded VM
// waits out the pod's grace period.
func TestVTPMContainerStopsOnSIGTERM(t *testing.T) {
	spec := map[string]any{}
	vtpmPod(compute.VMSpec{Key: "vm-1", VTPM: true}, "img", spec, nil)
	inits, _ := spec["initContainers"].([]any)
	if len(inits) != 1 {
		t.Fatalf("init containers: %v", inits)
	}
	c := inits[0].(map[string]any)
	if c["name"] != vtpmContainer {
		t.Fatalf("container %v", c["name"])
	}
	lc, _ := c["lifecycle"].(map[string]any)
	pre, _ := lc["preStop"].(map[string]any)
	ex, _ := pre["exec"].(map[string]any)
	if !reflect.DeepEqual(ex["command"], []string{"/bin/sh", "-c", "kill -TERM 1"}) {
		t.Fatalf("preStop: %v", lc)
	}
}
