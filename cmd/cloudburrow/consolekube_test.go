package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func podFixture(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return m
}

// A pod's phase is the wrong field to show.
//
// A container in CrashLoopBackOff sits in a pod whose phase is Running, and
// one that cannot pull its image sits in a pod whose phase is Pending. Both
// were rendered as if nothing had happened, which is the console saying
// nothing is wrong while the two most common deployment failures are.
func TestPodStatusReportsWhatIsActuallyWrong(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body, want string
	}{
		{
			name: "a crash loop, whose phase is Running",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":
			        [{"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`,
			want: "CrashLoopBackOff",
		},
		{
			name: "an image that does not exist, whose phase is Pending",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Pending","containerStatuses":
			        [{"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`,
			want: "ImagePullBackOff",
		},
		{
			name: "a bad config, whose phase is Pending",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Pending","containerStatuses":
			        [{"state":{"waiting":{"reason":"CreateContainerConfigError"}}}]}}`,
			want: "CreateContainerConfigError",
		},
		{
			name: "a container that exited non-zero",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":
			        [{"state":{"terminated":{"exitCode":137,"reason":"OOMKilled"}}}]}}`,
			want: "OOMKilled",
		},
		{
			name: "a non-zero exit with no reason still is not Running",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":
			        [{"state":{"terminated":{"exitCode":2}}}]}}`,
			want: "Error",
		},
		{
			name: "a clean exit is not an error",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Succeeded","containerStatuses":
			        [{"state":{"terminated":{"exitCode":0,"reason":"Completed"}}}]}}`,
			want: "Succeeded",
		},
		{
			name: "deletion beats whatever the containers are doing",
			body: `{"metadata":{"name":"p","deletionTimestamp":"2026-09-22T10:00:00Z"},
			        "status":{"phase":"Running","containerStatuses":
			        [{"state":{"running":{}}}]}}`,
			want: "Terminating",
		},
		{
			name: "a healthy pod still reports its phase",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":
			        [{"ready":true,"state":{"running":{"startedAt":"2026-09-22T10:00:00Z"}}}]}}`,
			want: "Running",
		},
		{
			name: "a pod with no container statuses yet",
			body: `{"metadata":{"name":"p"},"status":{"phase":"Pending"}}`,
			want: "Pending",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := podStatus(podFixture(t, c.body)); got != c.want {
				t.Errorf("podStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// An event carries its time in one of several fields depending on which API
// shape produced it, and on this cluster all three observed events have
// eventTime null and rely on lastTimestamp — so the fallback is not
// hypothetical.
func TestEventTimeFallsBackThroughEveryShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body, field, want string
	}{
		{
			name: "core/v1: lastTimestamp",
			body: `{"metadata":{"creationTimestamp":"2026-01-01T00:00:00Z"},
			         "firstTimestamp":"2026-09-01T00:00:00Z","lastTimestamp":"2026-09-22T00:00:00Z"}`,
			field: "last", want: "2026-09-22T00:00:00Z",
		},
		{
			name: "events.k8s.io: a repeating event's series",
			body: `{"metadata":{"creationTimestamp":"2026-01-01T00:00:00Z"},
			         "eventTime":"2026-09-01T00:00:00Z",
			         "series":{"lastObservedTime":"2026-09-22T00:00:00Z"}}`,
			field: "last", want: "2026-09-22T00:00:00Z",
		},
		{
			name: "events.k8s.io: a single event",
			body: `{"metadata":{"creationTimestamp":"2026-01-01T00:00:00Z"},
			         "eventTime":"2026-09-20T00:00:00Z"}`,
			field: "last", want: "2026-09-20T00:00:00Z",
		},
		{
			name:  "nothing but creation",
			body:  `{"metadata":{"creationTimestamp":"2026-01-01T00:00:00Z"}}`,
			field: "last", want: "2026-01-01T00:00:00Z",
		},
		{
			name:  "no usable timestamp at all",
			body:  `{"metadata":{}}`,
			field: "last", want: "",
		},
		{
			name: "first seen prefers firstTimestamp",
			body: `{"metadata":{"creationTimestamp":"2026-01-01T00:00:00Z"},
			         "firstTimestamp":"2026-09-01T00:00:00Z","lastTimestamp":"2026-09-22T00:00:00Z"}`,
			field: "first", want: "2026-09-01T00:00:00Z",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := eventTime(podFixture(t, c.body), c.field); got != c.want {
				t.Errorf("eventTime(%s) = %q, want %q", c.field, got, c.want)
			}
		})
	}
}

// An absent timestamp renders as nothing, not as 1970.
func TestShortAgeRefusesToInventATime(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "not a time", "0001-01-01T00:00:00Z"} {
		if got := shortAge(in); in != "0001-01-01T00:00:00Z" && got != "" {
			t.Errorf("shortAge(%q) = %q, want empty", in, got)
		}
	}
}

// The digest is the part a developer compares, not the whole 71 characters.
func TestShortDigest(t *testing.T) {
	t.Parallel()
	got := shortDigest("docker.io/library/postgres@sha256:abcdef0123456789abcdef")
	if got != "abcdef012345" {
		t.Errorf("shortDigest = %q", got)
	}
	if shortDigest("postgres:17") != "" {
		t.Error("an imageID with no digest must yield nothing rather than a guess")
	}
}

// A Cloud Run row has to answer which build is serving, and why a deploy
// failed.
//
// The listing used to carry the URL alone: the one question a deploy screen
// exists to settle could not be answered from it, and a failed service said
// "Failed" with the condition's reason discarded.
func TestCloudRunRowSaysWhatIsServingAndWhyNot(t *testing.T) {
	t.Parallel()

	decode := func(t *testing.T, body string) ksvcStatus {
		t.Helper()
		var s ksvcStatus
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		return s
	}

	t.Run("ready", func(t *testing.T) {
		r := decode(t, `{"metadata":{"name":"hello","creationTimestamp":"2026-09-22T10:00:00Z"},
		  "spec":{"template":{"spec":{"containers":[{"image":"ghcr.io/knative/helloworld-go:latest"}]}}},
		  "status":{"url":"http://hello.example","latestReadyRevisionName":"hello-00002",
		            "latestCreatedRevisionName":"hello-00002",
		            "conditions":[{"type":"Ready","status":"True"}]}}`).resource()

		if r.Status != "Ready" {
			t.Errorf("status = %q", r.Status)
		}
		if r.Fields["Image"] != "ghcr.io/knative/helloworld-go:latest" {
			t.Errorf("image = %q; the row cannot say what is running", r.Fields["Image"])
		}
		if r.Fields["Revision"] != "hello-00002" {
			t.Errorf("revision = %q", r.Fields["Revision"])
		}
		if r.Fields["Deploying"] != "" {
			t.Errorf("nothing is in flight, but Deploying = %q", r.Fields["Deploying"])
		}
	})

	t.Run("failed, with the reason kept", func(t *testing.T) {
		r := decode(t, `{"metadata":{"name":"broken"},
		  "spec":{"template":{"spec":{"containers":[{"image":"example.com/nope:1"}]}}},
		  "status":{"latestCreatedRevisionName":"broken-00001",
		            "conditions":[{"type":"Ready","status":"False","reason":"RevisionFailed",
		                           "message":"Revision \"broken-00001\" failed with message: Unable to fetch image."}]}}`).resource()

		if r.Status != "Failed" {
			t.Errorf("status = %q", r.Status)
		}
		if r.Fields["Reason"] != "RevisionFailed" {
			t.Errorf("reason = %q; \"Failed\" alone tells the reader nothing", r.Fields["Reason"])
		}
		if !strings.Contains(r.Fields["Detail"], "Unable to fetch image") {
			t.Errorf("detail = %q; the condition's message was discarded", r.Fields["Detail"])
		}
	})

	t.Run("a new revision that has not become ready", func(t *testing.T) {
		r := decode(t, `{"metadata":{"name":"rolling"},
		  "spec":{"template":{"spec":{"containers":[{"image":"example.com/app:2"}]}}},
		  "status":{"latestReadyRevisionName":"rolling-00001",
		            "latestCreatedRevisionName":"rolling-00002",
		            "conditions":[{"type":"Ready","status":"True"}]}}`).resource()

		if r.Fields["Revision"] != "rolling-00001" {
			t.Errorf("the serving revision is %q", r.Fields["Revision"])
		}
		if r.Fields["Deploying"] != "rolling-00002" {
			t.Errorf("a deploy is in flight and the row does not say so: %q", r.Fields["Deploying"])
		}
	})
}

// A YAML pane must not hand out credentials.
//
// Kubernetes stores a Secret's data base64-encoded, which is not encryption.
// A pane labelled "read-only" that rendered it would be distributing secrets.
func TestObjectSectionRemovesSecretData(t *testing.T) {
	t.Parallel()
	item := podFixture(t, `{
	  "kind":"Secret","metadata":{"name":"creds","namespace":"default"},
	  "data":{"password":"c3VwZXJzZWNyZXQ=","token":"YWJjMTIz"},
	  "stringData":{"apiKey":"plaintext-key"},
	  "spec":{"containers":[{"name":"app","env":[{"name":"SAFE","value":"yes"}]}]}
	}`)

	section := objectSection(item)
	if section.Kind != console.KindText {
		t.Fatalf("kind = %q, want text", section.Kind)
	}
	for _, leaked := range []string{"c3VwZXJzZWNyZXQ=", "YWJjMTIz", "plaintext-key"} {
		if strings.Contains(section.Text, leaked) {
			t.Errorf("the object pane leaked %q", leaked)
		}
	}
	if !strings.Contains(section.Text, "[REDACTED]") {
		t.Error("nothing was redacted, so the rule did not run at all")
	}
	// And it still shows the object: redaction that removes everything is a
	// pane nobody can use.
	for _, kept := range []string{`"name": "creds"`, `"SAFE"`} {
		if !strings.Contains(section.Text, kept) {
			t.Errorf("the object pane lost %q, which is not a credential", kept)
		}
	}
	// The source map is untouched: it is also what the table columns read.
	data, _ := item["data"].(map[string]any)
	if data["password"] != "c3VwZXJzZWNyZXQ=" {
		t.Error("redaction mutated the caller's object, which also feeds the columns")
	}
}

// A NULL and an empty string are different answers.
//
// A table that renders them identically is lying about one of them, and
// which one is the question the query was asked to settle.
func TestFormatSQLValueDistinguishesNullFromEmpty(t *testing.T) {
	t.Parallel()
	if got := formatSQLValue(nil); got != "—" {
		t.Errorf("NULL rendered as %q", got)
	}
	if got := formatSQLValue(""); got != "" {
		t.Errorf("the empty string rendered as %q, which is what NULL renders as", got)
	}
	if got := formatSQLValue([]byte("bytes")); got != "bytes" {
		t.Errorf("bytea rendered as %q", got)
	}
	if got := formatSQLValue(42); got != "42" {
		t.Errorf("an integer rendered as %q", got)
	}
}

// A selector has to come out the same way twice, or a workload's pod list
// depends on Go's map iteration order.
func TestSelectorOfIsStable(t *testing.T) {
	item := podFixture(t, `{"spec":{"selector":{"matchLabels":{
		"app":"api","tier":"backend","release":"v2"}}}}`)
	first := selectorOf(item)
	if first != "app=api,release=v2,tier=backend" {
		t.Fatalf("selector = %q", first)
	}
	for i := 0; i < 20; i++ {
		if got := selectorOf(item); got != first {
			t.Fatalf("selector changed between reads: %q then %q", first, got)
		}
	}
}

// A workload with no selector must produce no selector, not an empty one.
// kubectl reads "-l ”" as "every object", so the difference is between a
// section that says "no pods" and one that lists the whole namespace.
func TestSelectorOfRefusesToMatchEverything(t *testing.T) {
	if got := selectorOf(podFixture(t, `{"spec":{}}`)); got != "" {
		t.Fatalf("selector for a spec with no selector = %q, want empty", got)
	}
}

// Revision history is a Deployment's ReplicaSets, not every ReplicaSet in
// the namespace.
func TestOwnedByNameMatchesKindAndName(t *testing.T) {
	item := podFixture(t, `{"metadata":{"ownerReferences":[
		{"kind":"Deployment","name":"api"},
		{"kind":"ReplicaSet","name":"other"}]}}`)
	if !ownedByName(item, "Deployment", "api") {
		t.Fatal("owner not found")
	}
	// Same name, wrong kind: a ReplicaSet named "api" is not the Deployment.
	if ownedByName(item, "Deployment", "other") {
		t.Fatal("matched a ReplicaSet owner as a Deployment")
	}
	if ownedByName(podFixture(t, `{"metadata":{}}`), "Deployment", "api") {
		t.Fatal("matched an object with no owners")
	}
}

// A DaemonSet counts its replicas under different field names than a
// Deployment does, and reporting 0/0 for a healthy DaemonSet would read as
// an outage.
func TestWorkloadReplicasReadsBothShapes(t *testing.T) {
	deploy := podFixture(t, `{"kind":"Deployment","status":{"readyReplicas":2},
		"spec":{"replicas":3}}`)
	if ready, desired := workloadReplicas(deploy); ready != 2 || desired != 3 {
		t.Fatalf("deployment = %d/%d, want 2/3", ready, desired)
	}
	daemon := podFixture(t, `{"kind":"DaemonSet","status":{
		"numberReady":4,"desiredNumberScheduled":4}}`)
	if ready, desired := workloadReplicas(daemon); ready != 4 || desired != 4 {
		t.Fatalf("daemonset = %d/%d, want 4/4", ready, desired)
	}
}

// TestRunConfigurationReportsScalingAndConcurrency.
//
// The Configuration tab showed an image and its environment variables. Scaling,
// concurrency and the request timeout — where a service's behaviour under load
// is actually decided — appeared nowhere.
func TestRunConfigurationReportsScalingAndConcurrency(t *testing.T) {
	var svc ksvcStatus
	if err := json.Unmarshal([]byte(`{
		"metadata": {"name": "api", "creationTimestamp": "2026-09-22T00:00:00Z"},
		"status": {"url": "http://api.example", "latestCreatedRevisionName": "api-00002"},
		"spec": {"template": {
			"metadata": {"annotations": {
				"autoscaling.knative.dev/min-scale": "1",
				"autoscaling.knative.dev/max-scale": "10"}},
			"spec": {
				"containerConcurrency": 80,
				"timeoutSeconds": 600,
				"containers": [{
					"name": "user-container",
					"image": "example.com/api:v1",
					"command": ["/bin/api"],
					"resources": {"limits": {"cpu": "1", "memory": "512Mi"}},
					"env": [
						{"name": "MODE", "value": "live"},
						{"name": "TOKEN", "valueFrom": {"secretKeyRef":
							{"name": "api-key", "key": "latest"}}}]}]}}}}`), &svc); err != nil {
		t.Fatal(err)
	}

	got := sectionText(runConfiguration(&svc))
	for _, want := range []string{
		"Minimum instances=1", "Maximum instances=10",
		"Requests per instance=80", "Request timeout=600 seconds",
		"Limit cpu=1", "Limit memory=512Mi",
		"Entrypoint=/bin/api",
		"Env MODE=live",
		// A variable drawn from a Secret names the secret, because the value is
		// the secret.
		"Env TOKEN=from secret api-key key latest",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("configuration is missing %q:\n%s", want, got)
		}
	}
}

// TestUnsetScalingSettingsSayWhatTheyMean.
//
// Knative reads 0 as "use the default". Printing "0" would read as "no requests
// allowed" and "no timeout", which is the opposite of what it does.
func TestUnsetScalingSettingsSayWhatTheyMean(t *testing.T) {
	var svc ksvcStatus
	if err := json.Unmarshal([]byte(`{
		"metadata": {"name": "api"},
		"spec": {"template": {"spec": {"containers": [{"image": "example.com/api:v1"}]}}}}`),
		&svc); err != nil {
		t.Fatal(err)
	}
	got := sectionText(runConfiguration(&svc))
	for _, want := range []string{
		"Requests per instance=unlimited",
		"Request timeout=Knative's default",
		"Minimum instances=—",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("configuration is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Requests per instance=0") {
		t.Error(`an unset concurrency is reported as "0", which reads as "no requests"`)
	}
}

// sectionText renders a properties section for assertion.
func sectionText(sec console.Section) string {
	var b strings.Builder
	for _, g := range sec.Groups {
		b.WriteString(g.Heading + "\n")
		for _, prop := range g.Properties {
			b.WriteString(prop.Label + "=" + prop.Value + "\n")
		}
	}
	b.WriteString(sec.Note + "\n")
	return b.String()
}

// TestDeployFormOffersOnlyWhatTheAdapterMaps.
//
// A field the adapter refuses is a control that exists to fail, and a field it
// maps but the form omits is support the console hides. Both are the same bug in
// opposite directions.
func TestDeployFormOffersOnlyWhatTheAdapterMaps(t *testing.T) {
	_, fields := runProvider{}.CreateForm()
	offered := map[string]bool{}
	for _, f := range fields {
		offered[f.Name] = true
	}

	// Every field ToKnative writes into the manifest.
	for _, want := range []string{
		"name", "image", "port", "command", "args", "env",
		"cpu", "memory", "minInstances", "maxInstances", "concurrency", "timeout",
	} {
		if !offered[want] {
			t.Errorf("the deploy form does not offer %q, which the adapter maps", want)
		}
	}
	// Every field its Unsupported refuses.
	for _, absent := range []string{
		"serviceAccount", "vpcAccess", "volumes", "encryptionKey",
		"binaryAuthorization", "executionEnvironment", "sessionAffinity", "ingress",
	} {
		if offered[absent] {
			t.Errorf("the deploy form offers %q, which the adapter refuses", absent)
		}
	}
}

// TestOptionalIntTreatsBlankAsUnset.
//
// A blank scaling field means "leave it alone". Reading it as an error would
// make every field on the deploy form required in practice.
func TestOptionalIntTreatsBlankAsUnset(t *testing.T) {
	for _, blank := range []string{"", "  "} {
		n, err := optionalInt(blank, "minimum instances")
		if err != nil || n != 0 {
			t.Fatalf("optionalInt(%q) = %d, %v", blank, n, err)
		}
	}
	if n, err := optionalInt("3", "x"); err != nil || n != 3 {
		t.Fatalf("optionalInt(\"3\") = %d, %v", n, err)
	}
	for _, bad := range []string{"-1", "two", "1.5"} {
		if _, err := optionalInt(bad, "minimum instances"); err == nil {
			t.Errorf("optionalInt(%q) was accepted", bad)
		}
	}
}
