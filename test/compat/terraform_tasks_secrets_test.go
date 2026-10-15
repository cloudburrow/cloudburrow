//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformTasksQueueHTTPTarget (#1304, #1282): a google_cloud_tasks_queue
// with an http_target (method, a URI override, header overrides and an OIDC
// token) applies through `cloudburrow terraform`; the official client reads
// the target back; the plan is clean; changing the method, the override's
// host and a header applies in place; the plan is clean again; and destroy
// deletes the queue.
// covers: google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/GetQueue, google.cloud.tasks.v2.CloudTasks/UpdateQueue, google.cloud.tasks.v2.CloudTasks/DeleteQueue
func TestTerraformTasksQueueHTTPTarget(t *testing.T) {
	h := New(t)
	tc := tasksClient(t, h) // skips without CLOUDBURROW_TEST_TASKS
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	queueID := fmt.Sprintf("tf-http-%d", time.Now().UnixNano())
	queue := "projects/" + project + "/locations/us-central1/queues/" + queueID
	sa := "tf-tasks@" + project + ".iam.gserviceaccount.com"
	module := func(method, host, header string) string {
		return fmt.Sprintf(`
resource "google_cloud_tasks_queue" "q" {
  project  = %[1]q
  name     = %[2]q
  location = "us-central1"
  http_target {
    http_method = %[3]q
    uri_override {
      scheme = "HTTPS"
      host   = %[4]q
      port   = 8443
      path_override {
        path = "/hooks/task"
      }
      query_override {
        query_params = "source=queue"
      }
      uri_override_enforce_mode = "ALWAYS"
    }
    header_overrides {
      header {
        key   = "X-Queue"
        value = %[5]q
      }
    }
    oidc_token {
      service_account_email = %[6]q
      audience              = "https://%[4]s"
    }
  }
}
`, project, queueID, method, host, header, sa)
	}
	check := func(when string, method cloudtaskspb.HttpMethod, host, header string) {
		t.Helper()
		q, err := tc.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: queue})
		if err != nil {
			t.Fatalf("%s, GetQueue: %v", when, err)
		}
		ht := q.GetHttpTarget()
		uo := ht.GetUriOverride()
		if ht.GetHttpMethod() != method || uo.GetScheme() != cloudtaskspb.UriOverride_HTTPS || uo.GetHost() != host ||
			uo.GetPort() != 8443 || uo.GetPathOverride().GetPath() != "/hooks/task" ||
			uo.GetQueryOverride().GetQueryParams() != "source=queue" ||
			uo.GetUriOverrideEnforceMode() != cloudtaskspb.UriOverride_ALWAYS {
			t.Errorf("%s, the queue's http_target = %v", when, ht)
		}
		if hs := ht.GetHeaderOverrides(); len(hs) != 1 || hs[0].GetHeader().GetKey() != "X-Queue" || hs[0].GetHeader().GetValue() != header {
			t.Errorf("%s, the header overrides = %v; want X-Queue: %s", when, hs, header)
		}
		if o := ht.GetOidcToken(); o.GetServiceAccountEmail() != sa || o.GetAudience() != "https://"+host {
			t.Errorf("%s, the OIDC token = %v", when, o)
		}
	}

	m.write(module("POST", "hooks.example.test", "one"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", cloudtaskspb.HttpMethod_POST, "hooks.example.test", "one")
	m.planClean()

	m.write(module("PUT", "other.example.test", "two"))
	m.apply()
	check("after the change", cloudtaskspb.HttpMethod_PUT, "other.example.test", "two")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := tc.GetQueue(ctx, &cloudtaskspb.GetQueueRequest{Name: queue}); status.Code(err) != codes.NotFound {
		t.Errorf("GetQueue after destroy: %v; want NotFound", err)
	}
}

// TestTerraformSecretRotationAndTopics (#1304, #1259, #1278): a
// google_secret_manager_secret with event topics, a rotation schedule and a
// version_destroy_ttl applies, after the provider's google_pubsub_topic; the
// official client reads them back; the plan is clean; a second topic and a
// changed rotation period apply in place; the plan is clean again; and
// destroy deletes the secret and the topics.
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret, google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/DeleteTopic
func TestTerraformSecretRotationAndTopics(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h) // skips without CLOUDBURROW_TEST_SECRETS
	ps := pubsubClient(t, h)  // skips without CLOUDBURROW_TEST_PUBSUB
	m := newTFModule(t)
	ctx := h.Context()
	project := h.Project()
	secret := "projects/" + project + "/secrets/tf-rotation"
	topicA := "projects/" + project + "/topics/tf-secret-events-a"
	topicB := "projects/" + project + "/topics/tf-secret-events-b"
	// A fixed next rotation time a year ahead, so the plan stays stable.
	next := time.Now().UTC().AddDate(1, 0, 0).Truncate(time.Hour).Format(time.RFC3339)
	module := func(period string, topics string) string {
		return fmt.Sprintf(`
resource "google_pubsub_topic" "a" {
  project = %[1]q
  name    = "tf-secret-events-a"
}
resource "google_pubsub_topic" "b" {
  project = %[1]q
  name    = "tf-secret-events-b"
}
resource "google_secret_manager_secret" "s" {
  project   = %[1]q
  secret_id = "tf-rotation"
  replication {
    auto {}
  }
  %[4]s
  rotation {
    next_rotation_time = %[2]q
    rotation_period    = %[3]q
  }
  version_destroy_ttl = "86400s"
}
`, project, next, period, topics)
	}
	oneTopic := `topics {
    name = google_pubsub_topic.a.id
  }`
	twoTopics := oneTopic + `
  topics {
    name = google_pubsub_topic.b.id
  }`
	check := func(when string, period time.Duration, topics ...string) {
		t.Helper()
		s, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secret})
		if err != nil {
			t.Fatalf("%s, GetSecret: %v", when, err)
		}
		r := s.GetRotation()
		if r.GetRotationPeriod().AsDuration() != period || r.GetNextRotationTime().AsTime().UTC().Format(time.RFC3339) != next {
			t.Errorf("%s, the rotation = %v; want every %v from %s", when, r, period, next)
		}
		if s.GetVersionDestroyTtl().AsDuration() != 24*time.Hour {
			t.Errorf("%s, version_destroy_ttl = %v; want 24h", when, s.GetVersionDestroyTtl().AsDuration())
		}
		var got []string
		for _, tp := range s.GetTopics() {
			got = append(got, tp.GetName())
		}
		if fmt.Sprint(got) != fmt.Sprint(topics) {
			t.Errorf("%s, the topics = %v; want %v", when, got, topics)
		}
	}

	m.write(module("2592000s", oneTopic))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", 30*24*time.Hour, topicA)
	m.planClean()

	m.write(module("604800s", twoTopics))
	m.apply()
	check("after the change", 7*24*time.Hour, topicA, topicB)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secret}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy: %v; want NotFound", err)
	}
	for _, tp := range []string{topicA, topicB} {
		if _, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tp}); status.Code(err) != codes.NotFound {
			t.Errorf("GetTopic %s after destroy: %v; want NotFound", tp, err)
		}
	}
}
