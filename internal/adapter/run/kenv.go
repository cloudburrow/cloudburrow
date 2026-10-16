package run

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Cloud Run's K_SERVICE, K_CONFIGURATION and K_REVISION (#1448).
//
// Knative sets the three on every container of a revision, after the
// container's own env, as the Knative names: the scoped Knative Service
// (<id>-<scope>, #1400) and its revision (<id>-<scope>-00001). It refuses a
// revision that sets them itself, so a revision cannot carry Cloud Run's
// values. They are set at the pod instead, by a MutatingAdmissionPolicy
// (Kubernetes' own, CEL-evaluated; no webhook server): when a revision's pod
// is created, the policy reads the Cloud Run names its Knative names stand
// for from a ConfigMap the adapter writes beside the Knative Service, and
// sets K_SERVICE and K_CONFIGURATION to the service ID and K_REVISION to the
// revision ID the API reports (RevisionID), as Cloud Run documents them.
//
// A revision's Knative name is chosen by Knative when the Configuration
// changes, after the adapter has applied the Service, and a pod is created
// soon after; RevisionID's suffix is a hash that CEL cannot compute. So the
// ConfigMap holds the IDs for every generation up to kEnvAhead beyond the
// latest created revision, written before the Service is applied, and a pod
// whose revision is not in it keeps Knative's values.

// kEnvAhead is how many generations past the latest created revision the
// ConfigMap names: one update makes one revision, so this many updates may
// be applied before the adapter writes the map again (it does on each).
const kEnvAhead = 5

// kEnvPolicyName names the MutatingAdmissionPolicy and its binding.
const kEnvPolicyName = "cloudburrow-run-k-env"

// labelKEnv marks the ConfigMaps the binding reads as parameters.
const labelKEnv = "cloudburrow.dev/run-k-env"

// kEnvPolicy is the policy and its binding, applied with every service.
// failurePolicy Ignore: a pod the policy cannot evaluate still starts, with
// Knative's values, rather than not at all.
func kEnvPolicy(namespace string) string {
	return fmt.Sprintf(`apiVersion: admissionregistration.k8s.io/v1
kind: MutatingAdmissionPolicy
metadata:
  name: %[1]s
  labels:
    cloudburrow.dev/owned: "true"
spec:
  paramKind:
    apiVersion: v1
    kind: ConfigMap
  matchConstraints:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: %[2]s
    resourceRules:
      - apiGroups: [""]
        apiVersions: ["v1"]
        operations: ["CREATE"]
        resources: ["pods"]
  matchConditions:
    - name: knative-revision
      expression: "has(object.metadata.labels) && 'serving.knative.dev/revision' in object.metadata.labels && 'serving.knative.dev/service' in object.metadata.labels"
  variables:
    - name: rev
      expression: "object.metadata.labels['serving.knative.dev/revision']"
    - name: known
      expression: "has(params.data) && variables.rev in params.data && ('service.' + object.metadata.labels['serving.knative.dev/service']) in params.data"
  failurePolicy: Ignore
  reinvocationPolicy: Never
  mutations:
    - patchType: ApplyConfiguration
      applyConfiguration:
        expression: >-
          !variables.known ? Object{} : Object{
            spec: Object.spec{
              containers: object.spec.containers.filter(c, has(c.env) && c.env.exists(e, e.name == 'K_REVISION')).map(c,
                Object.spec.containers{
                  name: c.name,
                  env: [
                    Object.spec.containers.env{name: 'K_SERVICE', value: params.data['service.' + object.metadata.labels['serving.knative.dev/service']]},
                    Object.spec.containers.env{name: 'K_CONFIGURATION', value: params.data['service.' + object.metadata.labels['serving.knative.dev/service']]},
                    Object.spec.containers.env{name: 'K_REVISION', value: params.data[variables.rev]}
                  ]
                })
            }
          }
---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingAdmissionPolicyBinding
metadata:
  name: %[1]s
  labels:
    cloudburrow.dev/owned: "true"
spec:
  policyName: %[1]s
  paramRef:
    namespace: %[2]s
    selector:
      matchLabels:
        %[3]s: "true"
    parameterNotFoundAction: Allow
---
`, kEnvPolicyName, namespace, labelKEnv)
}

// kEnvConfigMap is the ConfigMap of one service's Cloud Run names: its ID,
// under service.<kname>, and each revision's ID, under its Knative name, for
// generations 1 to latest+kEnvAhead, where latest is the generation of the
// Knative revision latestCreated names ("" before the first).
func kEnvConfigMap(name, kname, namespace, instance, latestCreated string) string {
	id := name[strings.LastIndex(name, "/")+1:]
	latest := 0
	if i := strings.LastIndex(latestCreated, "-"); i >= 0 {
		latest, _ = strconv.Atoi(latestCreated[i+1:])
	}
	data := map[string]string{"service." + kname: id}
	for g := 1; g <= latest+kEnvAhead; g++ {
		krev := fmt.Sprintf("%s-%05d", kname, g)
		data[krev] = RevisionID(name, kname, krev)
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
  labels:
    cloudburrow.dev/owned: "true"
    cloudburrow.dev/instance: %q
    %s: "true"
data:
`, dnsLabel("run-k-env-"+kname), namespace, instance, labelKEnv)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s: %q\n", k, data[k])
	}
	b.WriteString("---\n")
	return b.String()
}
