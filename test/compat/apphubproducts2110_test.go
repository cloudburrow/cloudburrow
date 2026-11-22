//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"bytes"
	"encoding/json"
	"net/http"

	"cloud.google.com/go/apphub/apiv1/apphubpb"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/logging/logadmin"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestAppHubDiscoversOtherProducts (#2110, round 3), with the official
// Pub/Sub, Cloud Storage, Secret Manager, Cloud Tasks and App Hub clients,
// over gRPC and REST: as supported-resources writes their URIs, a Pub/Sub
// topic and subscription (//pubsub.googleapis.com/projects/P/...) and a
// global secret (//secretmanager.googleapis.com/projects/NUMBER/secrets/S)
// are discovered services in global, a US multi-region bucket
// (//storage.googleapis.com/B) in us, a Cloud Tasks queue
// (//cloudtasks.googleapis.com/projects/NUMBER/locations/L/queues/Q) in its
// region, the _Default log bucket
// (//logging.googleapis.com/projects/NUMBER/locations/global/buckets/_Default)
// in global; each is found by LookupDiscoveredService, and the topic is
// registered to a global application.
func TestAppHubDiscoversOtherProducts(t *testing.T) {
	h := New(t)
	clients := appHubClients(t, h)
	// The run shard, which serves App Hub, exports Pub/Sub, Cloud Storage,
	// Resource Manager and Cloud Logging under its own RUN_* names, so it
	// does not run their suites a second time; shardEndpoint exports each
	// under its ordinary name for this test only.
	pubsubAddr := shardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_RESOURCEMANAGER", EnvResourceManager)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if runShardEndpointIfSet(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging) != "" {
		shardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	}
	sc := storageClient(t, h)
	sm := secretsClientWith(t, h)
	tc := tasksClient(t, h)
	logged := os.Getenv("CLOUDBURROW_TEST_LOGGING") != ""
	ctx := h.Context()
	project := h.Project()
	suffix := time.Now().UnixNano() % 1e6

	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	topic := fmt.Sprintf("projects/%s/topics/ah-topic-%d", project, suffix)
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() { _ = pc.TopicAdminClient.DeleteTopic(h.Context(), &pubsubpb.DeleteTopicRequest{Topic: topic}) })
	sub := fmt.Sprintf("projects/%s/subscriptions/ah-sub-%d", project, suffix)
	if _, err := pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.SubscriptionAdminClient.DeleteSubscription(h.Context(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})

	bucket := fmt.Sprintf("%s-ah-%d", project, suffix)
	if err := sc.Bucket(bucket).Create(ctx, project, &storage.BucketAttrs{Location: "US"}); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	t.Cleanup(func() { _ = sc.Bucket(bucket).Delete(h.Context()) })

	secret, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: fmt.Sprintf("ah-secret-%d", suffix),
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = sm.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: secret.GetName()}) })
	secretID := strings.Split(secret.GetName(), "/")[3]

	queue, err := tc.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: "projects/" + project + "/locations/us-central1",
		Queue: &cloudtaskspb.Queue{Name: fmt.Sprintf("projects/%s/locations/us-central1/queues/ah-q-%d", project, suffix)}})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	t.Cleanup(func() { _ = tc.DeleteQueue(h.Context(), &cloudtaskspb.DeleteQueueRequest{Name: queue.GetName()}) })
	// supported-resources names a secret, a queue and a log bucket by the
	// project's number, which Resource Manager has once App Hub has
	// resolved the project (the attachment).
	var number string
	var want map[string]string
	wantURIs := func() {
		if want != nil {
			return
		}
		number = projectNumber(t, h, project)
		queueURI := fmt.Sprintf("//cloudtasks.googleapis.com/projects/%s/locations/us-central1/queues/ah-q-%d", number, suffix)

		want = map[string]string{
			"//pubsub.googleapis.com/" + topic:                                           "global",
			"//pubsub.googleapis.com/" + sub:                                             "global",
			"//storage.googleapis.com/" + bucket:                                         "us",
			"//secretmanager.googleapis.com/projects/" + number + "/secrets/" + secretID: "global",
			queueURI: "us-central1",
		}
		if logged {
			want["//logging.googleapis.com/projects/"+number+"/locations/global/buckets/_Default"] = "global"
		}
	}

	for _, via := range []string{"grpc", "rest"} {
		t.Run(via, func(t *testing.T) {
			h := h.For(t)
			c := clients[via]
			detachAppHubProject(t, h, c, project)
			host := "projects/" + project + "/locations/"
			op, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: host + "global",
				ServiceProjectAttachmentId: project, ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}})
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if _, err := op.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			wantURIs()
			listed := map[string]map[string]*apphubpb.DiscoveredService{}
			for _, loc := range []string{"global", "us", "us-central1"} {
				listed[loc] = map[string]*apphubpb.DiscoveredService{}
				it := c.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: host + loc})
				for {
					ds, err := it.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						t.Fatalf("ListDiscoveredServices %s: %v", loc, err)
					}
					listed[loc][ds.GetServiceReference().GetUri()] = ds
				}
			}
			for uri, loc := range want {
				ds := listed[loc][uri]
				if ds == nil {
					var in []string
					for u := range listed[loc] {
						in = append(in, u)
					}
					t.Errorf("%s is not discovered in %s, which lists %v", uri, loc, in)
					continue
				}
				if ds.GetServiceProperties().GetLocation() != loc || ds.GetServiceProperties().GetGcpProject() != "projects/"+project {
					t.Errorf("%s: properties %v", uri, ds.GetServiceProperties())
				}
				got, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: host + loc, Uri: uri})
				if err != nil || got.GetDiscoveredService().GetName() != ds.GetName() {
					t.Errorf("LookupDiscoveredService %s: %v, %v", uri, got, err)
				}
			}
			services := 0
			for uri := range listed["global"] {
				if strings.HasPrefix(uri, "//serviceusage.googleapis.com/projects/"+number+"/services/") {
					services++
				}
			}
			t.Logf("%d enabled services discovered", services)

			app := fmt.Sprintf("ah-prod-%s-%d", via, suffix)
			aop, err := c.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: host + "global", ApplicationId: app,
				Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_GLOBAL}}})
			if err != nil {
				t.Fatalf("CreateApplication: %v", err)
			}
			if _, err := aop.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			name := host + "global/applications/" + app
			t.Cleanup(func() {
				if op, err := c.DeleteService(h.Context(), &apphubpb.DeleteServiceRequest{Name: name + "/services/topic"}); err == nil {
					_ = op.Wait(h.Context())
				}
				if op, err := c.DeleteApplication(h.Context(), &apphubpb.DeleteApplicationRequest{Name: name}); err == nil {
					_ = op.Wait(h.Context())
				}
			})
			ds := listed["global"]["//pubsub.googleapis.com/"+topic]
			if ds == nil {
				t.Fatal("the topic is not discovered")
			}
			sop, err := c.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: name, ServiceId: "topic",
				Service: &apphubpb.Service{DiscoveredService: ds.GetName()}})
			if err != nil {
				t.Fatalf("CreateService: %v", err)
			}
			svc, err := sop.Wait(ctx)
			if err != nil || svc.GetServiceReference().GetUri() != "//pubsub.googleapis.com/"+topic {
				t.Fatalf("registered: %v, %v", svc, err)
			}
			if logged {
				appHubLogEntryHasApp(t, h, project, fmt.Sprintf("ah-topic-%d", suffix), via, app)
			}
		})
	}
}

// appHubLogEntryHasApp (#2096): an entry written for the registered topic's
// monitored resource (pubsub_topic) carries, in entries.list's JSON, the
// output-only apphub Google's LogEntry REST reference documents: the
// application (container, location, id) and the service's id. The proto,
// and so the gRPC answer, has no such field.
func appHubLogEntryHasApp(t *testing.T, h *Harness, project, topicID, via, app string) {
	t.Helper()
	base := "http://" + h.Endpoint(EnvLogging)
	logName := fmt.Sprintf("projects/%s/logs/apphub-%s", project, via)
	post := func(path string, body any, out any) {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %s", path, resp.Status)
		}
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	post("/v2/entries:write", map[string]any{"entries": []any{map[string]any{"logName": logName, "textPayload": "hello",
		"resource": map[string]any{"type": "pubsub_topic", "labels": map[string]string{"project_id": project, "topic_id": topicID}}}}}, nil)
	var got struct {
		Entries []struct {
			AppHub *struct {
				Application struct{ Container, Location, ID string } `json:"application"`
				Service     *struct{ ID string }                     `json:"service"`
			} `json:"apphub"`
		} `json:"entries"`
	}
	post("/v2/entries:list", map[string]any{"resourceNames": []string{"projects/" + project}, "filter": fmt.Sprintf("logName=%q", logName)}, &got)
	if len(got.Entries) != 1 || got.Entries[0].AppHub == nil {
		t.Fatalf("entries.list: %+v; want one entry with apphub", got)
	}
	a := got.Entries[0].AppHub
	if a.Application.ID != app || a.Application.Location != "global" || a.Application.Container != "projects/"+project ||
		a.Service == nil || a.Service.ID != "topic" {
		t.Errorf("apphub = %+v; want application %s in global of projects/%s, service topic", a, app, project)
	}

	appHubLogFilters(t, h, project, logName, app, "service", "topic")
}

// appHubLogFilters (#2096): the Logging API's own filter on apphub.*, over
// REST and with the official client over gRPC. The query language takes
// every LogEntry field, so the registered resource's entry in logName is
// the one apphub.application.id="APP" and apphub.KIND.id="ID" match;
// another application's matches none.
func appHubLogFilters(t *testing.T, h *Harness, project, logName, app, kind, id string) {
	t.Helper()
	logAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	base := "http://" + logAddr
	var got struct{ Entries []json.RawMessage }
	post := func(filter string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"resourceNames": []string{"projects/" + project}, "filter": filter})
		resp, err := http.Post(base+"/v2/entries:list", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("entries.list: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("entries.list %s: %s", filter, resp.Status)
		}
		got.Entries = nil
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
	}
	for filter, want := range map[string]int{
		fmt.Sprintf("logName=%q AND apphub.application.id=%q", logName, app):   1,
		fmt.Sprintf("logName=%q AND apphub.%s.id=%q", logName, kind, id):       1,
		fmt.Sprintf("logName=%q AND apphub.application.id=\"other\"", logName): 0,
		fmt.Sprintf("logName=%q AND NOT apphub.application.id:*", logName):     0,
	} {
		post(filter)
		if len(got.Entries) != want {
			t.Errorf("entries.list (REST) filter %s: %d entries, want %d", filter, len(got.Entries), want)
		}
		ctx := h.Context()
		ac, err := logadmin.NewClient(ctx, "projects/"+project, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		it := ac.Entries(ctx, logadmin.Filter(filter))
		for {
			_, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("entries.list (gRPC) filter %s: %v", filter, err)
			}
			n++
		}
		_ = ac.Close()
		if n != want {
			t.Errorf("entries.list (gRPC) filter %s: %d entries, want %d", filter, n, want)
		}
	}
}
