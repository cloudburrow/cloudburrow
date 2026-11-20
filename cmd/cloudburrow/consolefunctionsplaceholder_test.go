package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestWriteFunctionIsRound4sForm (#2049): Write a function has no
// Environment, Add trigger reveals the Eventarc pane (its fields
// prefixed, each shown only while Add trigger is checked), and the Cloud
// Run settings sit in Google's tabs.
func TestWriteFunctionIsRound4sForm(t *testing.T) {
	a := writeFunctionLayout(functionsProvider{storage: true}.createAction())
	pane := eventarcPane(eventarcPaneCatalog{}, "us-central1")
	fields := placeholderWriteFields(a.Fields, pane)
	byName := map[string]console.Field{}
	var names []string
	for _, f := range fields {
		byName[f.Name] = f
		names = append(names, f.Name)
	}
	if _, ok := byName["environment"]; ok {
		t.Error("Write a function still asks for an Environment")
	}
	add, ok := byName[writeFunctionAddTrigger]
	if !ok || add.Label != "Add trigger" || add.Type != "checkbox" || add.Section != "Trigger" {
		t.Fatalf("Add trigger = %+v", add)
	}
	if i, j := slices.Index(names, "runtime"), slices.Index(names, writeFunctionAddTrigger); i < 0 || j != i+1 ||
		slices.Index(names, "authentication") < j {
		t.Errorf("order = %v; want Runtime, Add trigger, then Authentication", names)
	}
	for _, f := range pane {
		got, ok := byName[writeFunctionTrigger+f.Name]
		if !ok || !strings.HasPrefix(got.ShowWhen, writeFunctionAddTrigger) {
			t.Errorf("pane field %s = %+v", f.Name, got)
		}
		if f.ShowWhen != "" && !strings.Contains(got.ShowWhen, "&"+writeFunctionTrigger) {
			t.Errorf("pane field %s's condition %q is not prefixed", f.Name, got.ShowWhen)
		}
	}
	for name, tab := range map[string]string{"executionEnvironment": runTabContainers, "cloudSqlInstances": runTabContainers,
		"memoryVolumes": runTabVolumes, "gcsVolumes": runTabVolumes, "nfsVolumes": runTabVolumes, "useHttp2": runTabNetworking,
		"vpcConnector": runTabNetworking, "vpcEgress": runTabNetworking, "sessionAffinity": runTabNetworking} {
		if f := byName[name]; f.Section != runSectionMore || f.Tab != tab {
			t.Errorf("%s = %+v; want on %s", name, f, tab)
		}
	}
	// The tabs run in Google's order.
	last := 0
	for _, f := range fields {
		if f.Section != runSectionMore {
			continue
		}
		if o := functionTabOrder(f); o < last {
			t.Errorf("%s (%s) is out of tab order", f.Name, f.Tab)
		} else {
			last = o
		}
	}
}

// TestPlaceholderServiceCarriesTheForm (#2049): the placeholder's service
// is the function's, running Google's placeholder image, with the form's
// Cloud Run settings, and reads back to the function's ServiceConfig.
func TestPlaceholderServiceCarriesTheForm(t *testing.T) {
	fn := &functionspb.Function{Name: "projects/p/locations/us-central1/functions/f",
		BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22", EntryPoint: "helloHttp",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: &functionspb.StorageSource{Bucket: "b", Object: "o.zip"}}}},
		ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: "512M", AvailableCpu: "1", TimeoutSeconds: 120, MinInstanceCount: 1, MaxInstanceCount: 5,
			EnvironmentVariables:       map[string]string{"A": "1"},
			SecretEnvironmentVariables: []*functionspb.SecretEnvVar{{Key: "S", ProjectId: "p", Secret: "s", Version: "2"}},
			SecretVolumes:              []*functionspb.SecretVolume{{MountPath: "/etc/s", ProjectId: "p", Secret: "v"}}}}
	svc, err := placeholderService(fn, map[string]string{"executionEnvironment": "Second generation", "vpcConnector": "projects/p/locations/us-central1/connectors/c",
		"vpcEgress": "All traffic", "sessionAffinity": "true", "memoryVolumes": `{"/cache":"64Mi"}`, "useHttp2": "true"})
	if err != nil {
		t.Fatal(err)
	}
	tm := svc.GetTemplate()
	c := tm.GetContainers()[0]
	if c.GetImage() != runadapter.PlaceholderImage || c.GetPorts()[0].GetName() != "h2c" ||
		tm.GetExecutionEnvironment() != runpb.ExecutionEnvironment_EXECUTION_ENVIRONMENT_GEN2 ||
		tm.GetVpcAccess().GetEgress() != runpb.VpcAccess_ALL_TRAFFIC || !tm.GetSessionAffinity() ||
		svc.GetLabels()["goog-managed-by"] != "cloudfunctions" {
		t.Errorf("service = %v", svc)
	}
	var vols []string
	for _, v := range tm.GetVolumes() {
		vols = append(vols, v.GetName())
	}
	if !slices.Contains(vols, placeholderSecretVolume+"0") || len(vols) != 2 {
		t.Errorf("volumes = %v; want the secret volume as Cloud Run's, and the in-memory one", vols)
	}
	if bc := svc.GetBuildConfig(); bc.GetFunctionTarget() != "helloHttp" || bc.GetSourceLocation() != "gs://b/o.zip" ||
		bc.GetImageUri() != "" || runadapter.RuntimeOfBaseImage(bc.GetBaseImage()) != "nodejs22" {
		t.Errorf("build_config = %v", bc)
	}
	svc.Name = "projects/p/locations/us-central1/services/f"
	sc := functionServiceConfigOf(svc)
	if sc.GetAvailableMemory() != "512M" || sc.GetAvailableCpu() != "1" || sc.GetTimeoutSeconds() != 120 ||
		sc.GetMinInstanceCount() != 1 || sc.GetMaxInstanceCount() != 5 || sc.GetEnvironmentVariables()["A"] != "1" ||
		len(sc.GetSecretEnvironmentVariables()) != 1 || sc.GetSecretEnvironmentVariables()[0].GetSecret() != "s" ||
		sc.GetSecretEnvironmentVariables()[0].GetVersion() != "2" {
		t.Errorf("service config read back = %v", sc)
	}
}

// TestBaseImageAndCloudShell (#2049): Edit runtime and security updates
// offers the runtimes and the two policies; Run in Cloud Shell carries the
// test command, reaching the URL's host through the gateway.
func TestBaseImageAndCloudShell(t *testing.T) {
	fn := &functionspb.Function{Name: "projects/p/locations/us-central1/functions/f", State: functionspb.Function_ACTIVE,
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123",
			RuntimeUpdatePolicy: &functionspb.BuildConfig_OnDeployUpdatePolicy{OnDeployUpdatePolicy: &functionspb.OnDeployUpdatePolicy{RuntimeVersion: "v"}}}}
	a, ok := editRuntimeAction(fn)
	if !ok || a.Label != "Edit runtime and security updates" || a.Submit != "Save and redeploy" ||
		a.Fields[0].Default != "go123" || a.Fields[1].Default != runtimeUpdateOnDeploy ||
		strings.Join(a.Fields[1].Options, ",") != "Automatic,On deploy" {
		t.Errorf("Edit runtime and security updates = %+v", a)
	}
	if n := baseImageNote(fn); !strings.Contains(n, "runtimes/go123") || !strings.Contains(n, "on deploy") || !strings.Contains(n, "Runtime version: v") {
		t.Errorf("Base image = %q", n)
	}
	if _, ok := editRuntimeAction(&functionspb.Function{Environment: functionspb.Environment_GEN_1}); ok {
		t.Error("a 1st gen function offers Edit runtime and security updates")
	}
	sec := testingSection(&functionspb.Function{Name: fn.Name}, "http://f.default.cloudburrow.localhost:24280", false, false)
	sh := sec.Actions[len(sec.Actions)-1]
	if sh.Label != "Run in Cloud Shell" || !strings.HasPrefix(sh.Terminal, "curl -m 70 --connect-to ::kourier.kourier-system.svc.cluster.local:80 -X POST http://f.") ||
		!strings.Contains(sh.Terminal, `-d '{"name":"Hello World"}'`) || strings.Contains(sh.Terminal, "Test command") {
		t.Errorf("Run in Cloud Shell = %+v", sh)
	}
	gen1 := testingSection(&functionspb.Function{Name: fn.Name}, "", true, false)
	if got := gen1.Actions[len(gen1.Actions)-1].Terminal; !strings.HasPrefix(got, "gcloud functions call f") {
		t.Errorf("1st gen Run in Cloud Shell = %q", got)
	}
}

// TestDeployedByIsTheLastModifier (#2049).
func TestDeployedByIsTheLastModifier(t *testing.T) {
	list := []*runpb.Service{{Name: "projects/p/locations/l/services/a", Creator: "c@x", LastModifier: "m@x"},
		{Name: "projects/p/locations/l/services/b", Creator: "c@x"}}
	i := 0
	got := lastModifiersOf(func() (*runpb.Service, error) {
		if i == len(list) {
			return nil, iterator.Done
		}
		i++
		return list[i-1], nil
	})
	if got["a"] != "m@x" || got["b"] != "c@x" {
		t.Errorf("deployed by = %v", got)
	}
	if got := lastModifiersOf(func() (*runpb.Service, error) { return nil, errors.New("down") }); len(got) != 0 {
		t.Errorf("an error gave %v", got)
	}
}
