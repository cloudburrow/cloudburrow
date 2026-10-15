package main

// Cloud Run: the settings the adapter stores and returns without effect
// (#1346), on the deploy and job forms and their edit forms: the execution
// environment, VPC access (a connector and its egress), the encryption key
// and, for a service, session affinity. Each field's help says nothing acts
// on it here, as docs/compatibility.md does
// (TestRunStoredSettingsAndSubPath).

import (
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
		{Name: "vpcConnector", Label: "VPC connector", Type: "text", Section: section,
			Help: "Optional. projects/…/locations/…/connectors/…; stored and returned. No VPC exists locally, " +
				"so traffic is not routed through it."},
		{Name: "vpcEgress", Label: "VPC egress", Type: "select", Options: runEgressOptions, Default: runEgressOptions[0],
			Section: section, Help: "Stored with the connector; it routes nothing locally."},
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
	if conn, ok := values["vpcConnector"]; ok {
		conn = strings.TrimSpace(conn)
		if conn == "" {
			*s.vpc = nil
		} else {
			egress := runpb.VpcAccess_PRIVATE_RANGES_ONLY
			switch values["vpcEgress"] {
			case "", runEgressOptions[0]:
			case runEgressOptions[1]:
				egress = runpb.VpcAccess_ALL_TRAFFIC
			default:
				return fmt.Errorf("VPC egress %q is not one of %s", values["vpcEgress"], strings.Join(runEgressOptions, ", "))
			}
			// Network interfaces, which the form does not show, are kept.
			v := &runpb.VpcAccess{Connector: conn, Egress: egress}
			if *s.vpc != nil {
				v.NetworkInterfaces = (*s.vpc).GetNetworkInterfaces()
			}
			*s.vpc = v
		}
	}
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
