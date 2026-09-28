package main

// Cloud Run: labels and secret-backed variables on the deploy and job forms,
// and Execute with overrides (#852).
//
// The adapter maps a service's and a job's labels, reads them back as set,
// and resolves a secretKeyRef variable to the Secret Manager store's
// Kubernetes Secret (docs/compatibility.md: TestCloudRunRevisionReadsASecretManagerSecret,
// TestRunJobReadsASecretManagerSecret); RunJob applies overrides of the
// arguments, variables, task count and timeout to one execution
// (TestRunJobFailureAndCancel). The console could set none of them.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runLabelsField is a service's or a job's labels.
func runLabelsField(section string) console.Field {
	return console.Field{Name: "labels", Label: "Labels", Type: "map", Section: section,
		Help: "Optional. One key=value per line. A key in a namespace Cloud Run reserves (run.googleapis.com/, " +
			"cloud.googleapis.com/, serving.knative.dev/, autoscaling.knative.dev/) or in cloudburrow.dev/ is refused."}
}

// runSecretEnvField is the variables drawn from Secret Manager.
func runSecretEnvField(section string) console.Field {
	return console.Field{Name: "secretEnv", Label: "Variables from Secret Manager", Type: "map", Section: section,
		Help: "Optional. One NAME=secret per line, or NAME=secret:version; the version is latest when not given, " +
			"and latest is resolved to a version when the revision is deployed or the execution starts. The secret is an ID of this project or " +
			"projects/{project}/secrets/{secret}, and must exist with an enabled version. The value is read into " +
			"the container and never shown here."}
}

// parseRunSecretEnv reads the secretEnv field into secretKeyRef variables,
// in name order.
func parseRunSecretEnv(raw string) ([]*runpb.EnvVar, error) {
	m, err := console.ParseMap(raw)
	if err != nil {
		return nil, fmt.Errorf("variables from Secret Manager: %w", err)
	}
	out := make([]*runpb.EnvVar, 0, len(m))
	for _, name := range sortedKeys(m) {
		ref := strings.TrimSpace(m[name])
		secret, version := ref, ""
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			secret, version = ref[:i], ref[i+1:]
		}
		if secret == "" || (strings.Contains(ref, ":") && version == "") {
			return nil, fmt.Errorf("variables from Secret Manager: %s=%q is not secret or secret:version", name, ref)
		}
		out = append(out, &runpb.EnvVar{Name: name, Values: &runpb.EnvVar_ValueSource{ValueSource: &runpb.EnvVarSource{
			SecretKeyRef: &runpb.SecretKeySelector{Secret: secret, Version: version}}}})
	}
	return out, nil
}

// formatRunSecretRef is one secretEnv line's value.
func formatRunSecretRef(secret, version string) string {
	if version == "" {
		return secret
	}
	return secret + ":" + version
}

// withSecretEnv adds the form's secret-backed variables to a container's
// plain ones. A name in both is refused: one variable cannot have two values.
func withSecretEnv(c *runpb.Container, raw string) error {
	secret, err := parseRunSecretEnv(raw)
	if err != nil {
		return err
	}
	plain := map[string]bool{}
	for _, e := range c.GetEnv() {
		plain[e.GetName()] = true
	}
	for _, e := range secret {
		if plain[e.GetName()] {
			return fmt.Errorf("%s is both an environment variable and a variable from Secret Manager; give it one value", e.GetName())
		}
	}
	c.Env = append(c.Env, secret...)
	return nil
}

// formEnv replaces a container's variables read from the API with the
// form's, plain and secret-backed. A request that did not send the secretEnv
// field (keepSecrets) leaves the secret-backed variables as they are, unless
// it sets a plain value of the same name, as a form without that field did.
// A variable read back with neither a value nor a source is a Kubernetes
// secret reference CloudBurrow holds no record of, which the form could not
// have shown, so a save that would drop it is refused rather than made.
func formEnv(cur *runpb.Container, next []*runpb.EnvVar, keepSecrets bool, redeploy string) error {
	named := map[string]bool{}
	for _, e := range next {
		named[e.GetName()] = true
	}
	env := next
	for _, e := range cur.GetEnv() {
		switch {
		case named[e.GetName()]:
		case e.GetValueSource() != nil:
			if keepSecrets {
				env = append(env, e)
			}
		case e.GetValues() == nil:
			return fmt.Errorf("environment variable %s comes from a secret this instance has no "+
				"record of; %s", e.GetName(), redeploy)
		}
	}
	cur.Env = env
	return nil
}

// runLabels reads the labels field; ok is false when the form did not send
// it, so an edit leaves the labels as they are.
func runLabels(values map[string]string) (labels map[string]string, ok bool, err error) {
	raw, ok := values["labels"]
	if !ok {
		return nil, false, nil
	}
	labels, err = console.ParseMap(raw)
	if err != nil {
		return nil, false, fmt.Errorf("labels: %w", err)
	}
	return labels, true, nil
}

// --- Execute with overrides ----------------------------------------------

const actExecuteOverrides = "execute-overrides"

// executeOverridesAction is Execute with overrides on a job's page: the
// overrides RunJob applies to that execution only.
func executeOverridesAction() console.Action {
	return console.Action{ID: actExecuteOverrides, Label: "Execute with overrides", Fields: []console.Field{
		{Name: "args", Label: "Container arguments", Type: "text",
			Help: `Optional. Replace the job's arguments for this execution, quoted as a shell would: 'echo "hello, world"'. ` +
				"Empty: the job's own."},
		{Name: "env", Label: "Environment variables", Type: "map",
			Help: "Optional. One KEY=value per line, added to the job's, or replacing one of the same name, for this execution."},
		{Name: "taskCount", Label: "Number of tasks", Type: "text", Pattern: `^[0-9]{0,5}$`,
			Help: "Optional. Empty: the job's own."},
		{Name: "timeout", Label: "Task timeout (seconds)", Type: "text", Pattern: `^[0-9]{0,6}$`,
			Help: "Optional. Empty: the job's own. The job's configuration is not changed by any of these."},
	}}
}

// runJobOverrides is the RunJob overrides the form describes. A form that
// overrides nothing is refused: that execution is Execute's.
func runJobOverrides(values map[string]string) (*runpb.RunJobRequest_Overrides, error) {
	o := &runpb.RunJobRequest_Overrides{}
	co := &runpb.RunJobRequest_Overrides_ContainerOverride{}
	var err error
	if co.Args, err = shellSplit(values["args"]); err != nil {
		return nil, fmt.Errorf("container arguments: %w", err)
	}
	env, err := console.ParseMap(values["env"])
	if err != nil {
		return nil, fmt.Errorf("environment variables: %w", err)
	}
	for _, k := range sortedKeys(env) {
		co.Env = append(co.Env, &runpb.EnvVar{Name: k, Values: &runpb.EnvVar_Value{Value: env[k]}})
	}
	if len(co.Args) > 0 || len(co.Env) > 0 {
		o.ContainerOverrides = []*runpb.RunJobRequest_Overrides_ContainerOverride{co}
	}
	tasks, err := optionalInt(values["taskCount"], "number of tasks")
	if err != nil {
		return nil, err
	}
	o.TaskCount = int32(tasks)
	timeout, err := optionalInt(values["timeout"], "task timeout")
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		o.Timeout = durationpb.New(time.Duration(timeout) * time.Second)
	}
	if o.ContainerOverrides == nil && o.TaskCount == 0 && o.Timeout == nil {
		return nil, errors.New("give at least one override, or use Execute to run the job as it is")
	}
	return o, nil
}

// secretEnvLines is the secretEnv field's prefill from a container's
// secret-backed variables, and the names of those with no record of their
// secret, which it cannot show.
func secretEnvLines(env []*runpb.EnvVar) (string, []string) {
	m := map[string]string{}
	var unknown []string
	for _, e := range env {
		if ref := e.GetValueSource().GetSecretKeyRef(); ref != nil {
			m[e.GetName()] = formatRunSecretRef(ref.GetSecret(), ref.GetVersion())
		} else if e.GetValues() == nil {
			unknown = append(unknown, e.GetName())
		}
	}
	sort.Strings(unknown)
	return console.FormatMap(m), unknown
}
