package main

import (
	"context"
	"encoding/json"
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

	"github.com/cloudburrow/cloudburrow/internal/components"
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

// storageResetter empties Cloud Storage.
//
// It deliberately does not implement ProjectResetter: fake-gcs-server lists
// every bucket on the server whatever project is asked for, so a project-scoped
// reset would either delete another project's buckets or delete nothing. The
// admin API refuses such a request instead of guessing.
type storageResetter struct {
	// tunnel is the direct address of the storage backend. Resetting through
	// it rather than the notification front keeps reset traffic out of the
	// event log's notion of what an application did.
	tunnel *netfwd.Forwarder
	notify *notifyService
}

func (s *storageResetter) Name() string { return "storage" }

func (s *storageResetter) Reset(ctx context.Context) error {
	// Notification configurations go first. Deleting objects fires
	// OBJECT_DELETE events, and with the configurations still in place those
	// would be delivered into topics a Pub/Sub reset is about to remove.
	if s.notify != nil {
		if cfgs := s.notify.Configs(); cfgs != nil {
			if err := cfgs.Reset(); err != nil {
				return fmt.Errorf("clear notification configurations: %w", err)
			}
		}
	}
	if s.tunnel == nil || s.tunnel.HostAddr() == "" {
		return errors.New("the storage tunnel is not running")
	}
	base := "http://" + s.tunnel.HostAddr() + "/storage/v1"
	c := &http.Client{Timeout: 30 * time.Second}

	buckets, err := gcsNames(ctx, c, base+"/b", "")
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	for _, b := range buckets {
		objects, err := gcsNames(ctx, c, base+"/b/"+url.PathEscape(b)+"/o", "")
		if err != nil {
			return fmt.Errorf("list objects in %s: %w", b, err)
		}
		for _, o := range objects {
			if err := gcsDelete(ctx, c, base+"/b/"+url.PathEscape(b)+"/o/"+url.PathEscape(o)); err != nil {
				return fmt.Errorf("delete gs://%s/%s: %w", b, o, err)
			}
		}
		if err := gcsDelete(ctx, c, base+"/b/"+url.PathEscape(b)); err != nil {
			return fmt.Errorf("delete bucket %s: %w", b, err)
		}
	}
	return nil
}

// gcsNames lists every name at a JSON API list endpoint, following pages.
func gcsNames(ctx context.Context, c *http.Client, endpoint, pageToken string) ([]string, error) {
	var out []string
	for {
		u := endpoint
		if pageToken != "" {
			u += "?pageToken=" + url.QueryEscape(pageToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items         []struct{ Name string } `json:"items"`
			NextPageToken string                  `json:"nextPageToken"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", u, resp.Status)
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", u, err)
		}
		for _, it := range page.Items {
			out = append(out, it.Name)
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		pageToken = page.NextPageToken
	}
}

func gcsDelete(ctx context.Context, c *http.Client, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	// Gone already is the outcome a reset wants.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode/100 == 2 {
		return nil
	}
	return errors.New(resp.Status)
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
	// CloudBurrow's own event topic carries storage notifications to the
	// router. It is infrastructure, not state a test created, and deleting it
	// would silently stop notifications until the next restart.
	if project == components.EventProject() {
		return nil
	}
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
