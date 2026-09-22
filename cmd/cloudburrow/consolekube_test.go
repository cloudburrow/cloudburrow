package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/identity-wael/cloudburrow/internal/console"
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
