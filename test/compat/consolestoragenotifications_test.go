//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// TestConsoleStorageNotifications (#791), in the storage shard: a Pub/Sub
// notification created from a bucket's page, through the console's own route
// with the values its form submits, is what the official Storage client's
// BucketHandle.Notifications lists; an object uploaded afterwards with the
// official Storage client produces a message that the official Pub/Sub client
// pulls from a subscription on the topic; and a notification deleted from its
// row is gone from the client's list.
func TestConsoleStorageNotifications(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()

	topicID := "console-notify-" + project
	topicName := topic(t, h, ps, topicID)
	sub := subscription(t, h, ps, topicID+"-sub", topicName)
	bh := sc.Bucket(project + "-notify-console")
	if err := bh.Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	b := bh.BucketName()

	type action struct {
		ID     string
		Fields []struct {
			Name, Type, Default string
			Options             []string
		}
	}
	type page struct {
		Unavailable string
		Actions     []action
		Sections    []struct {
			ID      string
			Listing struct {
				Note  string
				Items []struct {
					Name   string
					Opens  []string
					Fields map[string]string
				}
			}
			Groups []struct {
				Heading    string
				Properties []struct{ Label, Value string }
			}
		}
	}
	detail := func(path ...string) page {
		t.Helper()
		v := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d page
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
			t.Fatalf("console detail %v = %d (%v): %s", path, code, err, body)
		}
		return d
	}
	act := func(path []string, id string, values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/storage?project="+project, string(body))
	}

	// The bucket's page offers Create notification, its topic picker the
	// project's topics.
	d := detail(b)
	var create *action
	for i := range d.Actions {
		if d.Actions[i].ID == "createnotification" {
			create = &d.Actions[i]
		}
	}
	if create == nil {
		t.Fatalf("the bucket's page does not offer Create notification: %+v", d.Actions)
	}
	values := map[string]string{}
	offered := false
	for _, f := range create.Fields {
		values[f.Name] = f.Default
		if f.Type == "checkbox" {
			values[f.Name] = "false"
		}
		if f.Name == "topic" {
			for _, o := range f.Options {
				offered = offered || o == topicName
			}
		}
	}
	if !offered {
		t.Fatalf("the topic picker does not offer %s: %+v", topicName, create.Fields)
	}
	values["topic"] = topicName
	values["event_"+storage.ObjectFinalizeEvent] = "true"
	values["prefix"] = "watched/"
	values["attributes"] = `{"origin":"console"}`
	if code, body := act([]string{b}, "createnotification", values); code != http.StatusOK {
		t.Fatalf("console Create notification = %d: %s", code, body)
	}

	all, err := bh.Notifications(ctx)
	if err != nil {
		t.Fatalf("Notifications: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("BucketHandle.Notifications lists %d configurations, want 1: %v", len(all), all)
	}
	var n *storage.Notification
	for _, v := range all {
		n = v
	}
	if n.TopicProjectID != project || n.TopicID != topicID || n.PayloadFormat != storage.JSONPayload ||
		n.ObjectNamePrefix != "watched/" || n.CustomAttributes["origin"] != "console" ||
		strings.Join(n.EventTypes, ",") != storage.ObjectFinalizeEvent {
		t.Errorf("the client reads the console's notification as %+v", n)
	}

	// The tab lists it, and its page is notifications.get's.
	var row []string
	for _, s := range detail(b).Sections {
		if s.ID != "notifications" {
			continue
		}
		for _, it := range s.Listing.Items {
			if it.Name == "notificationConfigs/"+n.ID && it.Fields["Topic"] == topicName {
				row = it.Opens
			}
		}
	}
	if row == nil {
		t.Fatalf("the Notifications tab does not list %s", n.ID)
	}
	props := map[string]string{}
	for _, s := range detail(row...).Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				props[g.Heading+"/"+p.Label] = p.Value
			}
		}
	}
	if props["Delivery/Topic"] != topicName || props["Delivery/Object name prefix"] != "watched/" {
		t.Errorf("the notification's page reads %v", props)
	}

	// An upload with the official client, outside the prefix and inside it:
	// only the one inside is delivered.
	write(t, ctx, sc, b, "other/skip.txt", "no")
	write(t, ctx, sc, b, "watched/report.txt", "yes")
	msg := receiveOne(t, ps, sub, 60*time.Second)
	if msg == nil {
		t.Fatal("no notification arrived within 60s")
	}
	for k, want := range map[string]string{
		"eventType": storage.ObjectFinalizeEvent, "bucketId": b, "objectId": "watched/report.txt",
		"payloadFormat": storage.JSONPayload, "origin": "console",
	} {
		if got := msg.Attributes[k]; got != want {
			t.Errorf("attribute %s = %q, want %q (all: %v)", k, got, want, msg.Attributes)
		}
	}
	if !strings.Contains(msg.Attributes["notificationConfig"], "notificationConfigs/"+n.ID) {
		t.Errorf("notificationConfig = %q, want it to name %s", msg.Attributes["notificationConfig"], n.ID)
	}

	// Delete from its row.
	if code, body := act(row, "deletenotification", nil); code != http.StatusOK {
		t.Fatalf("console Delete = %d: %s", code, body)
	}
	all, err = bh.Notifications(ctx)
	if err != nil {
		t.Fatalf("Notifications after delete: %v", err)
	}
	if _, ok := all[n.ID]; ok || len(all) != 0 {
		t.Errorf("after the console's delete the client lists %v", all)
	}
}
