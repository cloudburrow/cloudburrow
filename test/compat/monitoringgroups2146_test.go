//go:build compat

package compat

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/logadmin"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	monitoringapi "google.golang.org/api/monitoring/v3"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestMonitoringGroupsMatchResourceMetadata (#2146): a group's members are
// the project's real resources, here buckets and topics made with their
// official clients, matched by the metadata Google's Name, Tag and Region
// criteria write: resource.metadata.name, resource.metadata.region and
// resource.metadata.tag / metadata.user_labels.
//
// covers: google.monitoring.v3.GroupService/CreateGroup, google.monitoring.v3.GroupService/ListGroupMembers, google.monitoring.v3.GroupService/DeleteGroup
func TestMonitoringGroupsMatchResourceMetadata(t *testing.T) {
	h := New(t)
	opts := monitoringOpts(h)
	sc := storageClient(t, h)
	pc := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	gc, err := monitoring.NewGroupClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()

	euName := project + "-eu-logs"
	eu := sc.Bucket(euName)
	if err := eu.Create(ctx, project, &storage.BucketAttrs{Location: "EUROPE-WEST2", Labels: map[string]string{"env": "prod"}}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(h.Context(), eu) })
	usName := project + "-us-cache"
	us := sc.Bucket(usName)
	if err := us.Create(ctx, project, &storage.BucketAttrs{Location: "US-CENTRAL1", Labels: map[string]string{"env": "dev"}}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(h.Context(), us) })
	topicName := fmt.Sprintf("projects/%s/topics/orders-prod", project)
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName, Labels: map[string]string{"env": "prod"}}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.TopicAdminClient.DeleteTopic(h.Context(), &pubsubpb.DeleteTopicRequest{Topic: topicName})
	})

	parent := "projects/" + project
	members := func(filter, extra string) string {
		t.Helper()
		g, err := gc.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: parent, Group: &monitoringpb.Group{DisplayName: "g", Filter: filter}})
		if err != nil {
			t.Fatalf("CreateGroup %s: %v", filter, err)
		}
		defer func() { _ = gc.DeleteGroup(ctx, &monitoringpb.DeleteGroupRequest{Name: g.GetName()}) }()
		var out []string
		it := gc.ListGroupMembers(ctx, &monitoringpb.ListGroupMembersRequest{Name: g.GetName(), Filter: extra})
		for {
			m, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListGroupMembers %s: %v", filter, err)
			}
			out = append(out, m.GetType()+":"+m.GetLabels()["bucket_name"]+m.GetLabels()["topic_id"])
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	for _, c := range []struct{ filter, extra, want string }{
		{`resource.metadata.region = "europe-west2"`, "", "gcs_bucket:" + euName},
		{`resource.type = "gcs_bucket" AND resource.metadata.name = has_substring("cache")`, "", "gcs_bucket:" + usName},
		{`resource.metadata.tag."env" = has_substring("prod")`, "", "gcs_bucket:" + euName + ",pubsub_topic:orders-prod"},
		{`metadata.user_labels."env" = "prod"`, `resource.type = "pubsub_topic"`, "pubsub_topic:orders-prod"},
		{`resource.metadata.region = "us-central1" OR resource.metadata.region = "global"`, `resource.metadata.name = starts_with("orders")`, "pubsub_topic:orders-prod"},
	} {
		if got := members(c.filter, c.extra); got != c.want {
			t.Errorf("%s | %s: members %q, want %q", c.filter, c.extra, got, c.want)
		}
	}
	if _, err := gc.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: parent, Group: &monitoringpb.Group{DisplayName: "bad",
		Filter: `resource.metadata.colour = "red"`}}); err == nil {
		t.Error("a group on an unknown metadata selector was created")
	}
}

// TestMonitoringUptimeLogCheckFailures (#2146): logCheckFailures and
// disabled, which the REST reference publishes and monitoringpb lacks,
// through the official discovery client; a failing check with
// logCheckFailures writes its runs to Cloud Logging on the uptime_url
// resource, labelled check_id, with an UptimeCheckResult payload.
//
// covers: google.monitoring.v3.UptimeCheckService/CreateUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/GetUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/UpdateUptimeCheckConfig, google.monitoring.v3.UptimeCheckService/DeleteUptimeCheckConfig
func TestMonitoringUptimeLogCheckFailures(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvMonitoring)
	logAddr := h.Endpoint(EnvLogging)
	ctx := h.Context()
	project := h.Project()
	svc, err := monitoringapi.NewService(ctx, option.WithEndpoint("http://"+endpoint+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	// A local port nothing listens on: every run fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	c, err := svc.Projects.UptimeCheckConfigs.Create("projects/"+project, &monitoringapi.UptimeCheckConfig{
		DisplayName: "down", LogCheckFailures: true, Period: "60s", Timeout: "2s",
		MonitoredResource: &monitoringapi.MonitoredResource{Type: "uptime_url", Labels: map[string]string{"host": "127.0.0.1", "project_id": project}},
		TcpCheck:          &monitoringapi.TcpCheck{Port: int64(port)},
	}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = svc.Projects.UptimeCheckConfigs.Delete(c.Name).Context(h.Context()).Do() })
	if !c.LogCheckFailures {
		t.Fatalf("created without logCheckFailures: %+v", c)
	}
	got, err := svc.Projects.UptimeCheckConfigs.Get(c.Name).Context(ctx).Do()
	if err != nil || !got.LogCheckFailures || got.Disabled {
		t.Fatalf("Get: %+v, %v", got, err)
	}
	checkID := c.Name[strings.LastIndex(c.Name, "/")+1:]

	ac, err := logadmin.NewClient(ctx, "projects/"+project, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	filter := `resource.type="uptime_url" AND labels.check_id="` + checkID + `" AND "UptimeCheckResult"`
	deadline := time.Now().Add(90 * time.Second)
	found := false
	for !found && time.Now().Before(deadline) {
		it := ac.Entries(ctx, logadmin.Filter(filter))
		if e, err := it.Next(); err == nil {
			found = true
			// logadmin unescapes the log ID's %2F.
			if e.Resource.GetLabels()["host"] != "127.0.0.1" || !strings.HasSuffix(strings.ReplaceAll(e.LogName, "%2F", "/"), "/logs/monitoring.googleapis.com/uptime_checks") {
				t.Errorf("the failure's entry: %s %v", e.LogName, e.Resource)
			}
			break
		}
		time.Sleep(3 * time.Second)
	}
	if !found {
		t.Fatalf("no entry matched %s", filter)
	}

	// Clearing it and disabling the check over PATCH.
	p, err := svc.Projects.UptimeCheckConfigs.Patch(c.Name, &monitoringapi.UptimeCheckConfig{Disabled: true, ForceSendFields: []string{"LogCheckFailures"}}).
		UpdateMask("logCheckFailures,disabled").Context(ctx).Do()
	if err != nil || p.LogCheckFailures || !p.Disabled {
		t.Fatalf("Patch: %+v, %v", p, err)
	}
	// gRPC answers as the proto defines it.
	uc, err := monitoring.NewUptimeCheckClient(ctx, monitoringOpts(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if g, err := uc.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: c.Name}); err != nil || g.GetDisplayName() != "down" {
		t.Errorf("gRPC Get: %v, %v", g, err)
	}
}
