package main

import (
	"reflect"
	"testing"
)

// TestStepMetadataArgs (#1854): a step is given its account's metadata
// host, resolved to the address cloudburrow-host publishes the metadata
// server at; no published server, or no account, gives it none.
func TestStepMetadataArgs(t *testing.T) {
	at := func(a string) func() string { return func() string { return a } }
	if got, want := stepMetadataArgs(at("192.168.65.254:9010"), "cloudburrow-sa-0123"), []string{"--add-host", "cloudburrow-sa-0123:192.168.65.254",
		"-e", "GCE_METADATA_HOST=cloudburrow-sa-0123:9010"}; !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q; want %q", got, want)
	}
	if got := stepMetadataArgs(at("host.example:9010"), "cloudburrow-sa-0123"); len(got) < 2 || got[1] != "cloudburrow-sa-0123:host-gateway" {
		t.Errorf("a named host = %q", got)
	}
	for _, got := range [][]string{stepMetadataArgs(nil, "cloudburrow-sa-0123"), stepMetadataArgs(at(""), "cloudburrow-sa-0123"),
		stepMetadataArgs(at("10.0.0.1:9010"), "")} {
		if got != nil {
			t.Errorf("args = %q; want none", got)
		}
	}
}
