package main

// Cloud Run: the settings the adapter stores and returns without effect
// (#1346), on the deploy and job forms and their edit forms: the execution
// environment, VPC access (a connector and its egress), the encryption key
// and, for a service, session affinity. Each field's help says nothing acts
// on it here, as docs/compatibility.md does
// (TestRunStoredSettingsAndSubPath).

import (
	"errors"
	"fmt"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

var runExecEnvOptions = []string{"Default", "First generation", "Second generation"}

var runEgressOptions = []string{"Private ranges only", "All traffic"}

// runSettingsFields are the stored settings' fields; sessionAffinity only
// on a service's forms.
func runSettingsFields(section string, service bool) []console.Field {
	f := []console.Field{
		{Name: "executionEnvironment", Label: "Execution environment", Type: "select", Options: runExecEnvOptions,
			Default: runExecEnvOptions[0], Section: section,
			Help: "Stored and returned. Locally there is one runtime, so the choice changes nothing."},
		// Google's Networking tab (run/docs/configuring/vpc-direct-vpc and
		// vpc-connectors, read 2026-11-22): Connect to a VPC for outbound
		// traffic, sent directly (network, subnet, network tags) or through
		// a Serverless VPC Access connector, and Traffic routing (#2140).
		{Name: "vpcMode", Label: "Connect to a VPC for outbound traffic", Type: "radio", Section: section, Default: "none",
			Options: []string{"none", "direct", "connector"},
			OptionLabels: map[string]string{"none": "Don't connect to a VPC", "direct": "Send traffic directly to a VPC",
				"connector": "Use Serverless VPC Access connectors"}},
		{Name: "vpcNetwork", Label: "Network", Type: "text", Section: section, VisibleWhen: map[string]string{"vpcMode": "direct"},
			Help: "A VPC network of the project; it must exist."},
		{Name: "vpcSubnet", Label: "Subnet", Type: "text", Section: section, VisibleWhen: map[string]string{"vpcMode": "direct"},
			Help: "A subnet of the network in the service's region; empty is the subnet named as the network."},
		{Name: "vpcTags", Label: "Network tags", Type: "text", Section: section, VisibleWhen: map[string]string{"vpcMode": "direct"},
			Help: "Optional, comma-separated."},
		{Name: "vpcConnector", Label: "VPC connector", Type: "text", Section: section,
			Help: "Optional. A connector's name or projects/…/locations/…/connectors/…; with Compute Engine on, it must exist, " +
				"be READY and be in the service's region. It is recorded, and no traffic is routed through it (docs/gaps.md)."},
		{Name: "vpcEgress", Label: "Traffic routing", Type: "radio", Options: runEgressOptions, Default: runEgressOptions[0],
			OptionLabels: map[string]string{runEgressOptions[0]: "Route only requests to private IPs to the VPC",
				runEgressOptions[1]: "Route all traffic to the VPC"},
			Section: section, Help: "Recorded with the VPC connection; it routes nothing locally."},
		{Name: "encryptionKey", Label: "Encryption key", Type: "text", Section: section,
			Help: "Optional. A Cloud KMS key name; stored and returned. Nothing is encrypted with it locally."},
	}
	if service {
		f = append(f, console.Field{Name: "sessionAffinity", Label: "Session affinity", Type: "checkbox", Section: section,
			Help: "Stored and returned. Requests are not pinned to an instance locally."})
	}
	return f
}

// runSettings points at a template's stored settings; sessionAffinity is
// nil for a job.
type runSettings struct {
	execEnv         *runpb.ExecutionEnvironment
	vpc             **runpb.VpcAccess
	encryptionKey   *string
	sessionAffinity *bool
}

// applyRunSettings sets the form's stored settings. A field the request did
// not send leaves that setting as it was.
func applyRunSettings(s runSettings, values map[string]string) error {
	if v, ok := values["executionEnvironment"]; ok {
		switch v {
		case "", runExecEnvOptions[0]:
			*s.execEnv = runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_UNSPECIFIED
		case runExecEnvOptions[1]:
			*s.execEnv = runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN1
		case runExecEnvOptions[2]:
			*s.execEnv = runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2
		default:
			return fmt.Errorf("execution environment %q is not one of %s", v, strings.Join(runExecEnvOptions, ", "))
		}
	}
	if mode, ok := values["vpcMode"]; ok && mode != "connector" {
		egress, err := runEgress(values["vpcEgress"])
		if err != nil {
			return err
		}
		switch mode {
		case "", "none":
			*s.vpc = nil
		case "direct":
			ni := &runpb.VpcAccess_NetworkInterface{Network: strings.TrimSpace(values["vpcNetwork"]), Subnetwork: strings.TrimSpace(values["vpcSubnet"])}
			for _, t := range strings.Split(values["vpcTags"], ",") {
				if t = strings.TrimSpace(t); t != "" {
					ni.Tags = append(ni.Tags, t)
				}
			}
			if ni.Network == "" && ni.Subnetwork == "" {
				return errors.New("Network: choose the VPC network to send traffic to")
			}
			*s.vpc = &runpb.VpcAccess{Egress: egress, NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{ni}}
		default:
			return fmt.Errorf("%q is not a VPC connection", mode)
		}
		return applyOtherRunSettings(s, values)
	}
	if conn, ok := values["vpcConnector"]; ok {
		conn = strings.TrimSpace(conn)
		if conn == "" {
			*s.vpc = nil
		} else {
			egress, err := runEgress(values["vpcEgress"])
			if err != nil {
				return err
			}
			// A connector replaces Direct VPC egress: Cloud Run refuses
			// the two together.
			*s.vpc = &runpb.VpcAccess{Connector: conn, Egress: egress}
		}
	}
	return applyOtherRunSettings(s, values)
}

// runEgress is the Traffic routing choice.
func runEgress(v string) (runpb.VpcAccess_VpcEgress, error) {
	switch v {
	case "", runEgressOptions[0]:
		return runpb.VpcAccess_PRIVATE_RANGES_ONLY, nil
	case runEgressOptions[1]:
		return runpb.VpcAccess_ALL_TRAFFIC, nil
	}
	return 0, fmt.Errorf("VPC egress %q is not one of %s", v, strings.Join(runEgressOptions, ", "))
}

// applyOtherRunSettings sets the settings beside the VPC connection.
func applyOtherRunSettings(s runSettings, values map[string]string) error {
	if v, ok := values["encryptionKey"]; ok {
		*s.encryptionKey = strings.TrimSpace(v)
	}
	if s.sessionAffinity != nil {
		if v, ok := values["sessionAffinity"]; ok {
			*s.sessionAffinity = v == "true"
		}
	}
	return nil
}

// runSettingsDefaults are the settings fields' prefill.
func runSettingsDefaults(execEnv runpb.ExecutionEnvironment, vpc *runpb.VpcAccess, key string, affinity bool) map[string]string {
	out := map[string]string{"executionEnvironment": runExecEnvOptions[0], "vpcConnector": vpc.GetConnector(),
		"vpcEgress": runEgressOptions[0], "encryptionKey": key}
	out["vpcMode"] = "none"
	switch {
	case vpc.GetConnector() != "":
		out["vpcMode"] = "connector"
	case len(vpc.GetNetworkInterfaces()) > 0:
		ni := vpc.GetNetworkInterfaces()[0]
		out["vpcMode"], out["vpcNetwork"], out["vpcSubnet"], out["vpcTags"] = "direct", ni.GetNetwork(), ni.GetSubnetwork(), strings.Join(ni.GetTags(), ", ")
	}
	switch execEnv {
	case runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN1:
		out["executionEnvironment"] = runExecEnvOptions[1]
	case runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2:
		out["executionEnvironment"] = runExecEnvOptions[2]
	}
	if vpc.GetEgress() == runpb.VpcAccess_ALL_TRAFFIC {
		out["vpcEgress"] = runEgressOptions[1]
	}
	if affinity {
		out["sessionAffinity"] = "true"
	}
	return out
}
