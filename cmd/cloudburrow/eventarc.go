package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// eventTransport is the Cloud Run adapter's runadapter.EventTransport
// (#1414): the topics and push subscriptions a function's event trigger
// makes, through the official Pub/Sub client on the instance's Pub/Sub
// tunnel, and its Cloud Storage notifications, through the Storage JSON API
// on the storage tunnel.
type eventTransport struct {
	pubsub  func() *netfwd.Forwarder
	storage func() *netfwd.Forwarder
	client  *http.Client
}

func (e eventTransport) pubsubClient(ctx context.Context, project string) (*pubsub.Client, error) {
	f := e.pubsub()
	if f == nil || f.HostAddr() == "" {
		return nil, errors.New("Pub/Sub has not started on this instance: start it with --services run,storage,pubsub")
	}
	return pubsub.NewClient(ctx, project, option.WithEndpoint(f.HostAddr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
}

func eventProject(name string) string {
	p, _, _ := strings.Cut(strings.TrimPrefix(name, "projects/"), "/")
	return p
}

func (e eventTransport) CreateTopic(ctx context.Context, topic string, labels map[string]string) error {
	c, err := e.pubsubClient(ctx, eventProject(topic))
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic, Labels: labels})
	if status.Code(err) == codes.AlreadyExists {
		return nil
	}
	return err
}

func (e eventTransport) DeleteTopic(ctx context.Context, topic string) error {
	c, err := e.pubsubClient(ctx, eventProject(topic))
	if err != nil {
		return err
	}
	defer c.Close()
	err = c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: topic})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// CreatePushSubscription makes the subscription as Eventarc configures a
// trigger's: a 600-second ack deadline, a day's retention and a 10 to 600
// second exponential backoff between attempts.
func (e eventTransport) CreatePushSubscription(ctx context.Context, s runadapter.PushSubscription) error {
	c, err := e.pubsubClient(ctx, eventProject(s.Name))
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: s.Name, Topic: s.Topic, Labels: s.Labels,
		PushConfig:               &pubsubpb.PushConfig{PushEndpoint: s.Endpoint},
		AckDeadlineSeconds:       600,
		MessageRetentionDuration: durationpb.New(24 * time.Hour),
		RetryPolicy:              &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(10 * time.Second), MaximumBackoff: durationpb.New(600 * time.Second)},
	})
	return err
}

func (e eventTransport) DeleteSubscription(ctx context.Context, name string) error {
	c, err := e.pubsubClient(ctx, eventProject(name))
	if err != nil {
		return err
	}
	defer c.Close()
	err = c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

func (e eventTransport) storageBase() (string, error) {
	f := e.storage()
	if f == nil || f.HostAddr() == "" {
		return "", errors.New("Cloud Storage has not started on this instance: start it with --services run,storage,pubsub")
	}
	return hostURL(f.HostAddr()), nil
}

func (e eventTransport) CreateNotification(ctx context.Context, bucket, topic, eventType string) (string, error) {
	base, err := e.storageBase()
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"topic": "//pubsub.googleapis.com/" + topic, "payload_format": "JSON_API_V1",
		"event_types": []string{eventType}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/storage/v1/b/"+url.PathEscape(bucket)+"/notificationConfigs", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("notificationConfigs.insert on bucket %s: %s: %s", bucket, resp.Status, bytes.TrimSpace(b))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("notificationConfigs.insert answered %s", b)
	}
	return out.ID, nil
}

func (e eventTransport) DeleteNotification(ctx context.Context, bucket, id string) error {
	base, err := e.storageBase()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/storage/v1/b/"+url.PathEscape(bucket)+"/notificationConfigs/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("notificationConfigs.delete on bucket %s: %s", bucket, resp.Status)
	}
	return nil
}

// eventsConfig is what the Cloud Run adapter's event triggers are given.
type eventsConfig struct {
	transport eventTransport
	tokens    *metadata.Credentials
	// pushBase is the Run port as the cluster reaches it.
	pushBase func() string
}
