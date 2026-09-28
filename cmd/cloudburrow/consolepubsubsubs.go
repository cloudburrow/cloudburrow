package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// pubsubSubscriptionsProvider is the Pub/Sub Subscriptions screen (#595).
//
// The topic page reaches a subscription through its topic, which leaves out
// exactly the ones a developer goes looking for when something is wrong: a
// subscription whose topic was deleted, or one that was detached. This screen
// lists every subscription of the project through ListSubscriptions, opens
// each one, and deletes it, all through the official pubsub/v2 admin client
// against the emulator, so what it shows is what an SDK reads.
type pubsubSubscriptionsProvider struct{ endpoint string }

func (pubsubSubscriptionsProvider) ID() string    { return "pubsub-subscriptions" }
func (pubsubSubscriptionsProvider) Title() string { return "Pub/Sub subscriptions" }

// deletedTopic is the topic Pub/Sub reports for a subscription whose topic
// has been deleted.
const deletedTopic = "_deleted-topic_"

func (p pubsubSubscriptionsProvider) topics() pubsubProvider { return pubsubProvider(p) }

func (p pubsubSubscriptionsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{
		Columns:    []string{"Topic", "Delivery type", "Ack deadline"},
		NameColumn: "Subscription",
		Noun:       "subscriptions",
	}
	if project == "" {
		out.Prompt = "Pub/Sub lists subscriptions per project. Choose one in the toolbar."
		return out, nil
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return console.Listing{}, fmt.Errorf("connect to Pub/Sub: %w", err)
	}
	defer func() { _ = c.Close() }()

	it := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{
		Project: "projects/" + project,
	})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return console.Listing{}, fmt.Errorf("list subscriptions: %w", err)
		}
		out.Items = append(out.Items, console.Resource{Name: s.GetName(), Fields: map[string]string{
			"Topic":         subscriptionTopic(s),
			"Delivery type": deliveryType(s),
			"Ack deadline":  fmt.Sprintf("%ds", s.GetAckDeadlineSeconds()),
		}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// deliveryType names how a subscription's messages leave Pub/Sub, from the
// configuration the emulator returns. Which one it is decides where to look
// when messages are not arriving.
func deliveryType(s *pubsubpb.Subscription) string {
	switch {
	case s.GetBigqueryConfig().GetTable() != "":
		return "BigQuery"
	case s.GetCloudStorageConfig().GetBucket() != "":
		return "Cloud Storage"
	case s.GetBigtableConfig().GetTable() != "":
		return "Bigtable"
	case s.GetPushConfig().GetPushEndpoint() != "":
		return "Push"
	default:
		return "Pull"
	}
}

// subscriptionTopic is the topic column: the topic's name, or a plain
// statement that it was deleted rather than Pub/Sub's placeholder.
func subscriptionTopic(s *pubsubpb.Subscription) string {
	if s.GetTopic() == deletedTopic {
		return "(topic deleted)"
	}
	return s.GetTopic()
}

// Detail implements console.Driller for one subscription: what it is attached
// to, how it delivers, and what happens to a message it cannot deliver.
func (p pubsubSubscriptionsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	name := path[0]
	if !strings.HasPrefix(name, "projects/"+project+"/subscriptions/") {
		return console.Detail{Unavailable: fmt.Sprintf("%s is not a subscription of project %s", name, project)}, nil
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Pub/Sub: " + err.Error()}, nil
	}
	defer func() { _ = c.Close() }()

	// Before the read, which is activity on the subscription (#996).
	activity := readSubscriptionActivity(ctx, p.endpoint, name)
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil {
		return console.Detail{}, err
	}

	summary := []console.Property{
		{Label: "Subscription", Value: s.GetName()},
		{Label: "Topic", Value: subscriptionTopic(s)},
		{Label: "Delivery type", Value: deliveryType(s)},
		{Label: "Ack deadline", Value: fmt.Sprintf("%ds", s.GetAckDeadlineSeconds())},
	}
	if s.GetDetached() {
		summary = append(summary, console.Property{Label: "Detached", Value: "Yes: it receives no messages"})
	}
	if st := s.GetState(); st != pubsubpb.Subscription_STATE_UNSPECIFIED {
		summary = append(summary, console.Property{Label: "State", Value: st.String()})
	}

	return console.Detail{
		// Edit subscription (#786), prefilled from what the emulator returned.
		Edit:    subscriptionEditForm(s),
		Summary: summary,
		Sections: []console.Section{
			subscriptionDeliverySection(s),
			subscriptionDeadLetterSection(s),
			subscriptionConfigSection(s, activity),
		},
	}, nil
}

func subscriptionDeliverySection(s *pubsubpb.Subscription) console.Section {
	sec := console.Section{ID: "delivery", Label: "Delivery", Kind: console.KindProperties}
	switch deliveryType(s) {
	case "BigQuery":
		bq := s.GetBigqueryConfig()
		sec.Groups = []console.PropertyGroup{{Heading: "BigQuery", Properties: []console.Property{
			{Label: "Table", Value: bq.GetTable()},
			{Label: "Use topic schema", Value: yesNo(bq.GetUseTopicSchema())},
			{Label: "Use table schema", Value: yesNo(bq.GetUseTableSchema())},
			{Label: "Write metadata", Value: yesNo(bq.GetWriteMetadata())},
			{Label: "Drop unknown fields", Value: yesNo(bq.GetDropUnknownFields())},
			{Label: "State", Value: bq.GetState().String()},
		}}}
	case "Cloud Storage":
		cs := s.GetCloudStorageConfig()
		sec.Groups = []console.PropertyGroup{{Heading: "Cloud Storage", Properties: []console.Property{
			{Label: "Bucket", Value: cs.GetBucket()},
			{Label: "File name prefix", Value: orDash(cs.GetFilenamePrefix())},
			{Label: "File name suffix", Value: orDash(cs.GetFilenameSuffix())},
			{Label: "Maximum duration", Value: durationOrDash(cs.GetMaxDuration())},
			{Label: "Maximum bytes", Value: countOrDash(cs.GetMaxBytes())},
			{Label: "Maximum messages", Value: countOrDash(cs.GetMaxMessages())},
			{Label: "State", Value: cs.GetState().String()},
		}}}
	case "Bigtable":
		sec.Groups = []console.PropertyGroup{{Heading: "Bigtable", Properties: []console.Property{
			{Label: "Table", Value: s.GetBigtableConfig().GetTable()},
		}}}
	case "Push":
		push := s.GetPushConfig()
		props := []console.Property{{Label: "Endpoint", Value: push.GetPushEndpoint()}}
		switch {
		case push.GetNoWrapper() != nil:
			props = append(props, console.Property{Label: "Payload", Value: "Unwrapped"})
		case push.GetPubsubWrapper() != nil:
			props = append(props, console.Property{Label: "Payload", Value: "Wrapped in a Pub/Sub message"})
		}
		if oidc := push.GetOidcToken(); oidc != nil {
			props = append(props,
				console.Property{Label: "OIDC service account", Value: orDash(oidc.GetServiceAccountEmail())},
				console.Property{Label: "OIDC audience", Value: orDash(oidc.GetAudience())})
		}
		for _, pair := range sortedPairs(push.GetAttributes()) {
			props = append(props, console.Property{Label: "Attribute " + pair.Label, Value: pair.Value})
		}
		sec.Groups = []console.PropertyGroup{{Heading: "Push", Properties: props}}
	default:
		sec.Groups = []console.PropertyGroup{{Heading: "Pull", Properties: []console.Property{
			{Label: "Push endpoint", Value: "None: subscribers pull messages"},
		}}}
	}
	return sec
}

func subscriptionDeadLetterSection(s *pubsubpb.Subscription) console.Section {
	sec := console.Section{ID: "dead-lettering", Label: "Dead lettering", Kind: console.KindProperties}
	dl := s.GetDeadLetterPolicy()
	if dl.GetDeadLetterTopic() == "" {
		sec.Groups = []console.PropertyGroup{{Heading: "Dead-letter policy", Properties: []console.Property{
			{Label: "Dead-letter topic", Value: "None"},
		}}}
		// Said, because it changes what a failing message does: without a
		// policy it is redelivered for as long as it is retained.
		sec.Note = "No dead-letter policy: a message that is never acknowledged is " +
			"redelivered until its retention runs out, and delivery attempts are not counted."
		return sec
	}
	sec.Groups = []console.PropertyGroup{{Heading: "Dead-letter policy", Properties: []console.Property{
		{Label: "Dead-letter topic", Value: dl.GetDeadLetterTopic()},
		{Label: "Maximum delivery attempts", Value: strconv.Itoa(int(dl.GetMaxDeliveryAttempts()))},
	}}}
	return sec
}

func subscriptionConfigSection(s *pubsubpb.Subscription, activity subscriptionActivity) console.Section {
	sub := []console.Property{
		{Label: "Message retention", Value: durationOrDash(s.GetMessageRetentionDuration())},
		{Label: "Retain acknowledged messages", Value: yesNo(s.GetRetainAckedMessages())},
		{Label: "Message ordering", Value: yesNo(s.GetEnableMessageOrdering())},
		{Label: "Exactly-once delivery", Value: yesNo(s.GetEnableExactlyOnceDelivery())},
		{Label: "Filter", Value: orDash(s.GetFilter())},
	}
	if d := s.GetTopicMessageRetentionDuration(); d != nil {
		sub = append(sub, console.Property{Label: "Topic message retention", Value: d.AsDuration().String()})
	}
	groups := []console.PropertyGroup{{Heading: "Subscription", Properties: sub}, subscriptionExpiryGroup(s, activity)}
	if rp := s.GetRetryPolicy(); rp != nil {
		groups = append(groups, console.PropertyGroup{Heading: "Retry policy", Properties: []console.Property{
			{Label: "Minimum backoff", Value: durationOrDash(rp.GetMinimumBackoff())},
			{Label: "Maximum backoff", Value: durationOrDash(rp.GetMaximumBackoff())},
		}})
	}
	if len(s.GetLabels()) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Labels", Properties: sortedPairs(s.GetLabels())})
	}
	return console.Section{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: groups}
}

// Delete implements console.Deleter through the official client's
// DeleteSubscription, the call an application makes.
func (p pubsubSubscriptionsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return deleteSubscription(ctx, c.SubscriptionAdminClient, project, "", name)
}

// deleteSubscription deletes one subscription of a project through the same
// DeleteSubscription call an application makes. A subscription of another
// project is refused before any call: the screen was scoped to this one.
//
// With a topic, the subscription is read first and must belong to it. That is
// the topic page's row, and a request naming a subscription of another topic
// is an operation the page never offered; the read also makes a subscription
// deleted since the page was drawn fail with NOT_FOUND from the emulator
// rather than with a message of the console's own. Without one — the
// subscriptions screen, which lists them all — any subscription of the project
// may go, including one whose topic was deleted.
func deleteSubscription(ctx context.Context, sc *vkit.SubscriptionAdminClient, project, topic, name string) error {
	if !strings.HasPrefix(name, "projects/"+project+"/subscriptions/") {
		return fmt.Errorf("%s is not a subscription of project %s", name, project)
	}
	if topic != "" {
		sub, err := sc.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
		if err != nil {
			return err
		}
		if sub.GetTopic() != topic {
			return fmt.Errorf("%s is not a subscription of %s", name, topic)
		}
	}
	return sc.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name})
}

func durationOrDash(d *durationpb.Duration) string {
	if d == nil {
		return "—"
	}
	return d.AsDuration().String()
}

func countOrDash(n int64) string {
	if n == 0 {
		return "—"
	}
	return strconv.FormatInt(n, 10)
}
