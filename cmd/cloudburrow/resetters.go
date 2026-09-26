package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Resetters for the services /admin/reset did not reach.
//
// It reset Cloud Tasks and nothing else, so a test that cleared state between
// runs still found last run's buckets, topics and secrets waiting for it (#274).
// Each resetter goes through the service's own API rather than its storage, so
// a reset is exactly what a client could have done, one call at a time.

// secretsResetter clears Secret Manager.
type secretsResetter struct{ svc *secretsService }

func (s *secretsResetter) Name() string { return "secretmanager" }

func (s *secretsResetter) Reset(context.Context) error {
	st := s.svc.Store()
	if st == nil {
		return errors.New("Secret Manager has not started")
	}
	return st.Reset()
}

// ResetProject deletes one project's secrets. Secret Manager scopes everything
// by project, so this can be exact.
func (s *secretsResetter) ResetProject(_ context.Context, project string) error {
	st := s.svc.Store()
	if st == nil {
		return errors.New("Secret Manager has not started")
	}
	all, err := st.ListSecrets(project)
	if err != nil {
		return err
	}
	for _, sec := range all {
		if err := st.DeleteSecret(project, lastSegment(sec.Name)); err != nil {
			return fmt.Errorf("delete %s: %w", sec.Name, err)
		}
	}
	return nil
}

// builtinStorageResetter empties the builtin Cloud Storage server (#510)
// through its reset endpoint, which clears the store directly: live,
// noncurrent and soft-deleted objects, sessions, notification
// configurations, IAM policies and HMAC keys, with no event emitted.
// Buckets record their project, so it can confine a reset to one project.
type builtinStorageResetter struct {
	tunnel *netfwd.Forwarder
}

func (s *builtinStorageResetter) Name() string { return "storage" }

func (s *builtinStorageResetter) Reset(ctx context.Context) error { return s.reset(ctx, "") }

func (s *builtinStorageResetter) ResetProject(ctx context.Context, project string) error {
	return s.reset(ctx, project)
}

func (s *builtinStorageResetter) reset(ctx context.Context, project string) error {
	if s.tunnel == nil || s.tunnel.HostAddr() == "" {
		return errors.New("the storage tunnel is not running")
	}
	u := "http://" + s.tunnel.HostAddr() + "/_cloudburrow/reset"
	if project != "" {
		u += "?project=" + url.QueryEscape(project)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reset the builtin storage server: %s", resp.Status)
	}
	return nil
}

// pubsubResetter clears Pub/Sub.
//
// The emulator has no way to list projects, so "every project" means every
// project CloudBurrow knows about: the registry and the instance's own. A topic
// created under a project nobody registered survives a full reset; resetting
// that project by name reaches it.
type pubsubResetter struct {
	tunnel   *netfwd.Forwarder
	projects func() []string
}

func (p *pubsubResetter) Name() string { return "pubsub" }

func (p *pubsubResetter) Reset(ctx context.Context) error {
	var errs []error
	for _, project := range p.projects() {
		if err := p.ResetProject(ctx, project); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", project, err))
		}
	}
	return errors.Join(errs...)
}

// ResetProject deletes one project's subscriptions, snapshots and topics, in
// that order, so nothing is left pointing at a topic that no longer exists.
func (p *pubsubResetter) ResetProject(ctx context.Context, project string) error {
	c, err := pubsubAdmin(ctx, p.tunnel, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	parent := "projects/" + project

	subs := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: parent})
	for {
		s, err := subs.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("list subscriptions: %w", err)
		}
		if err := c.SubscriptionAdminClient.DeleteSubscription(ctx,
			&pubsubpb.DeleteSubscriptionRequest{Subscription: s.Name}); err != nil {
			return fmt.Errorf("delete %s: %w", s.Name, err)
		}
	}
	snaps := c.SubscriptionAdminClient.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: parent})
	for {
		s, err := snaps.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if status.Code(err) == codes.Unimplemented {
			// An emulator without snapshots has none to clear. Any other error
			// is a real failure and is reported as one.
			break
		}
		if err != nil {
			return fmt.Errorf("list snapshots: %w", err)
		}
		if err := c.SubscriptionAdminClient.DeleteSnapshot(ctx,
			&pubsubpb.DeleteSnapshotRequest{Snapshot: s.Name}); err != nil {
			return fmt.Errorf("delete %s: %w", s.Name, err)
		}
	}
	topics := c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: parent})
	for {
		t, err := topics.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("list topics: %w", err)
		}
		if err := c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: t.Name}); err != nil {
			return fmt.Errorf("delete %s: %w", t.Name, err)
		}
	}
	return nil
}

// pubsubAdmin connects the official client to the emulator through its tunnel.
func pubsubAdmin(ctx context.Context, tunnel *netfwd.Forwarder, project string) (*pubsub.Client, error) {
	if tunnel == nil || tunnel.HostAddr() == "" {
		return nil, errors.New("the Pub/Sub tunnel is not running")
	}
	return pubsub.NewClient(ctx, project,
		option.WithEndpoint(tunnel.HostAddr()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
}

// knownProjects is every project CloudBurrow can name: the registry's and the
// instance's own default, deduplicated and sorted.
func knownProjects(defaultProject string, registered func() []string) []string {
	seen := map[string]bool{defaultProject: true}
	for _, p := range registered() {
		seen[p] = true
	}
	delete(seen, "")
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
