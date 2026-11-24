package main

import (
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// The stored settings' fields set the template and prefill from it, and a
// field the request did not send leaves its setting alone (#1346).
func TestRunSettingsFieldsRoundTrip(t *testing.T) {
	tmpl := &runpb.RevisionTemplate{VpcAccess: &runpb.VpcAccess{Connector: "old",
		NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{Network: "n"}}}}
	s := runSettings{&tmpl.ExecutionEnvironment, &tmpl.VpcAccess, &tmpl.EncryptionKey, &tmpl.SessionAffinity}
	if err := applyRunSettings(s, map[string]string{"executionEnvironment": "Second generation",
		"vpcConnector": "projects/p/locations/us-central1/connectors/c", "vpcEgress": "All traffic",
		"encryptionKey": " k ", "sessionAffinity": "true"}); err != nil {
		t.Fatal(err)
	}
	if tmpl.ExecutionEnvironment != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 ||
		tmpl.VpcAccess.GetConnector() != "projects/p/locations/us-central1/connectors/c" ||
		tmpl.VpcAccess.GetEgress() != runpb.VpcAccess_ALL_TRAFFIC || len(tmpl.VpcAccess.GetNetworkInterfaces()) != 0 ||
		tmpl.EncryptionKey != "k" || !tmpl.SessionAffinity {
		t.Fatalf("applied = %v", tmpl)
	}
	d := runSettingsDefaults(tmpl.ExecutionEnvironment, tmpl.VpcAccess, tmpl.EncryptionKey, tmpl.SessionAffinity)
	if d["executionEnvironment"] != "Second generation" || d["vpcEgress"] != "All traffic" ||
		d["vpcConnector"] != tmpl.VpcAccess.Connector || d["encryptionKey"] != "k" || d["sessionAffinity"] != "true" {
		t.Errorf("defaults = %v", d)
	}
	if d["vpcMode"] != "connector" {
		t.Errorf("vpcMode = %q, want connector", d["vpcMode"])
	}
	// Send traffic directly to a VPC (#2140): the connector gives way to
	// a network interface, since Cloud Run refuses the two together.
	direct := &runpb.RevisionTemplate{VpcAccess: &runpb.VpcAccess{Connector: "old"}}
	ds := runSettings{&direct.ExecutionEnvironment, &direct.VpcAccess, &direct.EncryptionKey, nil}
	if err := applyRunSettings(ds, map[string]string{"vpcMode": "direct", "vpcNetwork": "default", "vpcSubnet": "sub",
		"vpcTags": "a, b", "vpcEgress": "All traffic"}); err != nil {
		t.Fatal(err)
	}
	if v := direct.VpcAccess; v.GetConnector() != "" || len(v.GetNetworkInterfaces()) != 1 || v.GetNetworkInterfaces()[0].GetSubnetwork() != "sub" ||
		len(v.GetNetworkInterfaces()[0].GetTags()) != 2 || v.GetEgress() != runpb.VpcAccess_ALL_TRAFFIC {
		t.Errorf("direct = %v", v)
	}
	if d := runSettingsDefaults(0, direct.VpcAccess, "", false); d["vpcMode"] != "direct" || d["vpcNetwork"] != "default" || d["vpcTags"] != "a, b" {
		t.Errorf("direct defaults = %v", d)
	}
	if err := applyRunSettings(ds, map[string]string{"vpcMode": "direct"}); err == nil {
		t.Error("direct with no network was accepted")
	}
	if err := applyRunSettings(ds, map[string]string{"vpcMode": "none"}); err != nil || direct.VpcAccess != nil {
		t.Errorf("none = %v (%v)", direct.VpcAccess, err)
	}
	// Nothing sent: nothing changes.
	if err := applyRunSettings(s, map[string]string{}); err != nil || tmpl.EncryptionKey != "k" || tmpl.VpcAccess == nil {
		t.Errorf("an empty form changed %v (%v)", tmpl, err)
	}
	// Blank connector clears VPC access; Default clears the environment.
	if err := applyRunSettings(s, map[string]string{"vpcConnector": "", "executionEnvironment": "Default",
		"sessionAffinity": ""}); err != nil || tmpl.VpcAccess != nil ||
		tmpl.ExecutionEnvironment != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_UNSPECIFIED || tmpl.SessionAffinity {
		t.Errorf("cleared = %v (%v)", tmpl, err)
	}
	if err := applyRunSettings(s, map[string]string{"executionEnvironment": "gen3"}); err == nil {
		t.Error("an unknown environment was accepted")
	}
}
