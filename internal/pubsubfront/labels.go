package pubsubfront

// Labels updates (#949). The emulator refuses a labels path in an update
// mask, over gRPC and REST alike: UpdateSubscription with "labels is not a
// known Subscription field", UpdateTopic with "labels is not a known Topic
// field". Terraform's google provider changes a google_pubsub_subscription's
// or google_pubsub_topic's labels in place with exactly that update, so the
// front applies it, as it applies expiration_policy (#891): the path is taken
// out of the mask the emulator sees, the labels are kept in the front once
// the rest of the update succeeds, and every topic and subscription read
// back through the front (Get, List and Update, over gRPC and REST) names
// them in place of the emulator's. An update of labels alone never reaches
// the emulator, and is answered with the resource as it now reads. A later
// create or delete of the name drops them, as the emulator then holds the
// labels the create set. With a state file (state.go) they survive a restart
// of the front alone.
//
// The labels are the update's whole map, as Google applies a labels mask:
// an update naming labels with none clears them. The front checks them no
// more than the emulator checks a create's: Google's rules on label keys and
// values are not enforced on either.

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
)

const publisher = "/google.pubsub.v1.Publisher/"

// topicAdmin is the part of the emulator's Publisher service the front
// reads a topic with.
type topicAdmin interface {
	GetTopic(ctx context.Context, in *pubsubpb.GetTopicRequest, opts ...grpc.CallOption) (*pubsubpb.Topic, error)
}

// topic reads a topic from the emulator.
func (f *Front) topic(ctx context.Context, name string) (*pubsubpb.Topic, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return f.topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
}

// updatedLabels is the labels an update of the labels path sets: its whole
// map, never nil, so that none is kept as none.
func updatedLabels(l map[string]string) map[string]string {
	out := make(map[string]string, len(l))
	maps.Copy(out, l)
	return out
}

// setLabels keeps the labels an update set on a topic or subscription, and
// writes them to the state file before the update is answered.
func (f *Front) setLabels(name string, l map[string]string) {
	f.mu.Lock()
	f.labels[name] = updatedLabels(l)
	f.mu.Unlock()
	f.persist()
}

// keptLabels is the labels the front keeps for name, and whether it keeps
// any.
func (f *Front) keptLabels(name string) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.labels[name]
	if !ok {
		return nil, false
	}
	return updatedLabels(l), true
}

// labelsEqual reports whether two label maps are the same, nil and empty
// alike.
func labelsEqual(a, b map[string]string) bool {
	return len(a) == len(b) && maps.Equal(a, b)
}

// withLabels puts the labels an update set, if any, in place of the ones the
// emulator returned for a topic or subscription; it reports whether it
// changed anything.
func (f *Front) withLabels(name string, labels *map[string]string) bool {
	l, ok := f.keptLabels(name)
	if !ok || labelsEqual(l, *labels) {
		return false
	}
	if len(l) == 0 {
		l = nil
	}
	*labels = l
	return true
}

// withTopicLabels is withLabels for a topic.
func (f *Front) withTopicLabels(t *pubsubpb.Topic) bool {
	if t == nil {
		return false
	}
	return f.withLabels(t.GetName(), &t.Labels)
}

// withLabelsJSON is a Topic's or Subscription's JSON with the labels the
// front keeps for it in place of the emulator's, and whether it changed.
func (f *Front) withLabelsJSON(res json.RawMessage) (json.RawMessage, bool) {
	var o jsonObject
	if json.Unmarshal(res, &o) != nil || o == nil {
		return res, false
	}
	var name string
	if raw, key := o.field("name"); key == "" || json.Unmarshal(raw, &name) != nil {
		return res, false
	}
	l, ok := f.keptLabels(name)
	if !ok {
		return res, false
	}
	var cur map[string]string
	if raw, key := o.field("labels"); key != "" && json.Unmarshal(raw, &cur) != nil {
		return res, false
	}
	if labelsEqual(l, cur) {
		return res, false
	}
	delete(o, "labels")
	if len(l) > 0 {
		b, err := encodeJSON(l)
		if err != nil {
			return res, false
		}
		o["labels"] = b
	}
	out, err := encodeJSON(o)
	if err != nil {
		return res, false
	}
	return out, true
}
