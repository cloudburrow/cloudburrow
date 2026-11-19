package pubsubfront

// Export subscriptions (#1874): BigQuery and Cloud Storage subscriptions,
// delivered by the front. The emulator stores a subscription's
// bigquery_config and cloud_storage_config and delivers nothing; it still
// queues the topic's messages for it, as for a pull subscription. So the
// front pulls them from the emulator itself, writes them to the instance's
// BigQuery (exportbq.go) or Cloud Storage (exportgcs.go), and acknowledges
// each message once it is written, as Google does:
// https://docs.cloud.google.com/pubsub/docs/bigquery and
// https://docs.cloud.google.com/pubsub/docs/cloudstorage.
//
// "When a Pub/Sub message cannot be written to BigQuery, the message cannot
// be acknowledged": a message that fails is negatively acknowledged, so it
// stays in the backlog and is delivered again, and with a dead-letter
// policy it is forwarded to the dead-letter topic after
// max_delivery_attempts, with the attribute
// CloudPubSubDeadLetterSourceDeliveryErrorMessage naming why.
//
// A subscription's state (ACTIVE, PERMISSION_DENIED, NOT_FOUND,
// SCHEMA_MISMATCH) is the front's: the last outcome of a write to its
// destination, and it reads back on every subscription the front answers.
// The destination is checked again every exportCheck while the
// subscription is not ACTIVE, and messages wait in the backlog meanwhile.
//
// Locally nothing is authorized: service_account_email is kept and checked
// for its form, and PERMISSION_DENIED is the state when the instance does
// not serve the destination's service at all, which Google gives when
// "bigquery.googleapis.com API is not enabled for the project".

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

var (
	// exportPoll is how often an idle export subscription is pulled.
	exportPoll = 500 * time.Millisecond
	// exportCheck is how often a destination that failed is looked at again.
	exportCheck = 2 * time.Second
)

// deadLetterErrorAttribute is the attribute Google adds to a message an
// export subscription forwards to its dead-letter topic.
const deadLetterErrorAttribute = "CloudPubSubDeadLetterSourceDeliveryErrorMessage"

// exporter is the front's export subscriptions' delivery.
type exporter struct {
	// bigquery, storage and kms are the base URLs of the instance's
	// BigQuery, Cloud Storage and Cloud KMS, "" when it does not serve one.
	bigquery, storage, kms string
	// project is the one project the local BigQuery serves, "" for any.
	client *http.Client

	mu sync.Mutex
	// workers are the running deliveries, by subscription name, with the
	// hash of the configuration each runs with.
	workers map[string]exportWorker
	// states are each export subscription's state, by name.
	states map[string]int32
	// attempts counts each message's failed deliveries, by subscription
	// and message ID, for subscriptions whose emulator copy reports none.
	attempts map[string]int
	// keys are Cloud KMS keys' states the front looked up, briefly cached.
	keys map[string]keyState
}

type exportWorker struct {
	hash   string
	cancel context.CancelFunc
	done   chan struct{}
}

// UseExports names the instance's BigQuery, Cloud Storage and Cloud KMS
// (base URLs such as http://host:port; "" for one not served) that export
// subscriptions write to and CMEK topics are checked against. Call it
// before Serve.
func (f *Front) UseExports(bigquery, storage, kms string) {
	f.exp.bigquery = strings.TrimRight(bigquery, "/")
	f.exp.storage = strings.TrimRight(storage, "/")
	f.exp.kms = strings.TrimRight(kms, "/")
}

func (e *exporter) init() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.workers == nil {
		e.workers = map[string]exportWorker{}
		e.states = map[string]int32{}
		e.attempts = map[string]int{}
		e.keys = map[string]keyState{}
	}
	if e.client == nil {
		e.client = &http.Client{Timeout: 60 * time.Second}
	}
}

// RunExports delivers every export subscription until ctx ends.
func (f *Front) RunExports(ctx context.Context) {
	f.exp.init()
	t := time.NewTicker(exportPoll)
	defer t.Stop()
	for {
		f.syncExports(ctx)
		select {
		case <-ctx.Done():
			// Stop every delivery and wait for each to return, so none
			// outlives RunExports. Wait without the lock: a delivery
			// records its state under it on the way out.
			f.exp.mu.Lock()
			running := make([]exportWorker, 0, len(f.exp.workers))
			for _, w := range f.exp.workers {
				w.cancel()
				running = append(running, w)
			}
			f.exp.mu.Unlock()
			for _, w := range running {
				<-w.done
			}
			return
		case <-t.C:
		}
	}
}

// exportSubs are the export subscriptions the emulator holds.
func (f *Front) exportSubs() map[string]*pubsubpb.Subscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]*pubsubpb.Subscription{}
	for name, s := range f.defs.subs {
		if s.GetBigqueryConfig() != nil || s.GetCloudStorageConfig() != nil {
			out[name] = proto.Clone(s).(*pubsubpb.Subscription)
		}
	}
	return out
}

func configHash(s *pubsubpb.Subscription) string {
	c := &pubsubpb.Subscription{Topic: s.GetTopic(), BigqueryConfig: s.GetBigqueryConfig(),
		CloudStorageConfig: s.GetCloudStorageConfig(), DeadLetterPolicy: s.GetDeadLetterPolicy()}
	if c.BigqueryConfig != nil {
		c.BigqueryConfig = proto.Clone(c.BigqueryConfig).(*pubsubpb.BigQueryConfig)
		c.BigqueryConfig.State = 0
	}
	if c.CloudStorageConfig != nil {
		c.CloudStorageConfig = proto.Clone(c.CloudStorageConfig).(*pubsubpb.CloudStorageConfig)
		c.CloudStorageConfig.State = 0
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// syncExports starts a delivery for each new or changed export
// subscription and stops those of the ones gone.
func (f *Front) syncExports(ctx context.Context) {
	subs := f.exportSubs()
	e := &f.exp
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, w := range e.workers {
		if s, ok := subs[name]; !ok || configHash(s) != w.hash {
			w.cancel()
			<-w.done
			delete(e.workers, name)
			if !ok {
				delete(e.states, name)
			}
		}
	}
	names := make([]string, 0, len(subs))
	for n := range subs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := e.workers[name]; ok {
			continue
		}
		s := subs[name]
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		e.workers[name] = exportWorker{hash: configHash(s), cancel: cancel, done: done}
		go func() {
			defer close(done)
			if s.GetBigqueryConfig() != nil {
				f.runBigQueryExport(wctx, s)
			} else {
				f.runStorageExport(wctx, s)
			}
		}()
	}
}

// setState records an export subscription's state.
func (e *exporter) setState(sub string, st int32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.states != nil {
		e.states[sub] = st
	}
}

// state is an export subscription's state, 0 when unknown.
func (e *exporter) state(sub string) int32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.states[sub]
}

// withExportState sets an export subscription's state as the front last
// saw it; it reports whether it changed anything.
func (f *Front) withExportState(s *pubsubpb.Subscription) bool {
	if s == nil {
		return false
	}
	switch {
	case s.GetBigqueryConfig() != nil:
		st := pubsubpb.BigQueryConfig_State(f.exp.state(s.GetName()))
		if st == 0 {
			st = pubsubpb.BigQueryConfig_ACTIVE
		}
		if s.BigqueryConfig.State == st {
			return false
		}
		s.BigqueryConfig.State = st
		return true
	case s.GetCloudStorageConfig() != nil:
		st := pubsubpb.CloudStorageConfig_State(f.exp.state(s.GetName()))
		if st == 0 {
			st = pubsubpb.CloudStorageConfig_ACTIVE
		}
		if s.CloudStorageConfig.State == st {
			return false
		}
		s.CloudStorageConfig.State = st
		return true
	}
	return false
}

// pulled is one message an export pulled, with its ack ID.
type pulled struct {
	ackID   string
	msg     *pubsubpb.PubsubMessage
	attempt int32
}

// pull takes up to max messages from the emulator for sub, without
// waiting for more.
func (f *Front) pull(ctx context.Context, sub string, max int32) ([]pulled, error) {
	if err := f.checkKeyEnabled(ctx, f.subscriptionKey(sub)); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	//lint:ignore SA1019 the emulator answers at once with what it has
	r, err := pubsubpb.NewSubscriberClient(f.upstream).Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: max, ReturnImmediately: true}) //nolint:staticcheck
	if err != nil {
		if status.Code(err) == codes.DeadlineExceeded {
			return nil, nil
		}
		return nil, err
	}
	out := make([]pulled, 0, len(r.GetReceivedMessages()))
	for _, m := range r.GetReceivedMessages() {
		out = append(out, pulled{ackID: m.GetAckId(), msg: m.GetMessage(), attempt: m.GetDeliveryAttempt()})
	}
	return out, nil
}

func (f *Front) ack(ctx context.Context, sub string, ids []string) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := pubsubpb.NewSubscriberClient(f.upstream).Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: ids}); err != nil {
		f.logf("pubsub front: export %s: acknowledge: %v", sub, err)
	}
	f.touch(sub)
}

// extend sets the ack deadline of messages a delivery holds.
func (f *Front) extend(ctx context.Context, sub string, ids []string, seconds int32) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = pubsubpb.NewSubscriberClient(f.upstream).ModifyAckDeadline(ctx,
		&pubsubpb.ModifyAckDeadlineRequest{Subscription: sub, AckIds: ids, AckDeadlineSeconds: seconds})
}

// failed is a message a delivery could not write, and why.
type failed struct {
	p      pulled
	reason string
}

// fail negatively acknowledges messages that could not be written, or,
// past the subscription's dead-letter policy's max_delivery_attempts,
// forwards them to its dead-letter topic and acknowledges them.
func (f *Front) fail(ctx context.Context, s *pubsubpb.Subscription, fs []failed) {
	if len(fs) == 0 {
		return
	}
	dl := s.GetDeadLetterPolicy()
	var nack, done []string
	for _, x := range fs {
		key := s.GetName() + "\x00" + x.p.msg.GetMessageId()
		f.exp.mu.Lock()
		f.exp.attempts[key]++
		n := f.exp.attempts[key]
		f.exp.mu.Unlock()
		if a := int(x.p.attempt); a > n {
			n = a
		}
		max := int(dl.GetMaxDeliveryAttempts())
		if dl.GetDeadLetterTopic() == "" {
			nack = append(nack, x.p.ackID)
			continue
		}
		if max == 0 {
			max = 5 // Google's default
		}
		if n < max {
			nack = append(nack, x.p.ackID)
			continue
		}
		m := proto.Clone(x.p.msg).(*pubsubpb.PubsubMessage)
		m.MessageId, m.PublishTime = "", nil
		if m.Attributes == nil {
			m.Attributes = map[string]string{}
		}
		m.Attributes["CloudPubSubDeadLetterSourceSubscription"] = s.GetName()
		m.Attributes["CloudPubSubDeadLetterSourceSubscriptionProject"] = projectOf(s.GetName())
		m.Attributes["CloudPubSubDeadLetterSourceTopicPublishTime"] = x.p.msg.GetPublishTime().AsTime().UTC().Format(time.RFC3339Nano)
		m.Attributes["CloudPubSubDeadLetterSourceDeliveryCount"] = strconv.Itoa(n)
		m.Attributes[deadLetterErrorAttribute] = x.reason
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := pubsubpb.NewPublisherClient(f.upstream).Publish(pctx, &pubsubpb.PublishRequest{Topic: dl.GetDeadLetterTopic(), Messages: []*pubsubpb.PubsubMessage{m}})
		cancel()
		if err != nil {
			f.logf("pubsub front: export %s: dead-letter: %v", s.GetName(), err)
			nack = append(nack, x.p.ackID)
			continue
		}
		f.exp.mu.Lock()
		delete(f.exp.attempts, key)
		f.exp.mu.Unlock()
		done = append(done, x.p.ackID)
	}
	f.ack(ctx, s.GetName(), done)
	f.extend(ctx, s.GetName(), nack, 0)
}

// forgetAttempts drops the failed-delivery counts of messages written.
func (e *exporter) forgetAttempts(sub string, ps []pulled) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range ps {
		delete(e.attempts, sub+"\x00"+p.msg.GetMessageId())
	}
}

// sleep waits d or until ctx ends, and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// checkExport refuses an export subscription Google refuses, as
// CreateSubscription and UpdateSubscription do: an export to a BigQuery
// table that does not exist or whose schema does not fit the
// subscription, or to a Cloud Storage bucket that does not exist, or one
// whose settings break Google's limits.
func (f *Front) checkExport(ctx context.Context, s *pubsubpb.Subscription) error {
	if s.GetBigqueryConfig() != nil && s.GetCloudStorageConfig() != nil {
		return apierror.InvalidArgument("A subscription can have at most one of push_config, bigquery_config, cloud_storage_config and bigtable_config.")
	}
	if s.GetBigqueryConfig() != nil {
		if s.GetPushConfig().GetPushEndpoint() != "" {
			return apierror.InvalidArgument("A subscription can have at most one of push_config, bigquery_config, cloud_storage_config and bigtable_config.")
		}
		return f.checkBigQueryConfig(ctx, s)
	}
	if s.GetCloudStorageConfig() != nil {
		if s.GetPushConfig().GetPushEndpoint() != "" {
			return apierror.InvalidArgument("A subscription can have at most one of push_config, bigquery_config, cloud_storage_config and bigtable_config.")
		}
		return f.checkStorageConfig(ctx, s)
	}
	return nil
}

// errDestination is a destination's failure, with the state it puts the
// subscription in.
type errDestination struct {
	state int32
	msg   string
}

func (e *errDestination) Error() string { return e.msg }

func destinationState(err error) (int32, bool) {
	var d *errDestination
	if errors.As(err, &d) {
		return d.state, true
	}
	return 0, false
}

// checkServiceAccount checks a service_account_email's form.
func checkServiceAccount(email string) error {
	if email == "" {
		return nil
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || !strings.HasSuffix(email, ".gserviceaccount.com") {
		return apierror.InvalidArgument("Invalid service_account_email: %s.", email)
	}
	return nil
}

// withStorageDefaults gives a new Cloud Storage subscription Google's
// default max_duration, so it reads back as from Google.
func withStorageDefaults(s *pubsubpb.Subscription) bool {
	cs := s.GetCloudStorageConfig()
	if cs == nil || cs.MaxDuration != nil {
		return false
	}
	cs.MaxDuration = durationpb.New(defaultMaxDuration)
	return true
}

// withExportStateJSON is a Subscription's JSON with its export state.
func (f *Front) withExportStateJSON(sub json.RawMessage) (json.RawMessage, bool) {
	var o jsonObject
	if json.Unmarshal(sub, &o) != nil || o == nil {
		return sub, false
	}
	var name string
	if raw, key := o.field("name"); key == "" || json.Unmarshal(raw, &name) != nil {
		return sub, false
	}
	raw, key := o.field("bigqueryConfig", "bigquery_config")
	isBQ := key != ""
	if !isBQ {
		raw, key = o.field("cloudStorageConfig", "cloud_storage_config")
	}
	if key == "" {
		return sub, false
	}
	var cfg jsonObject
	if json.Unmarshal(raw, &cfg) != nil || cfg == nil {
		return sub, false
	}
	st := f.exp.state(name)
	var text string
	if isBQ {
		if st == 0 {
			st = int32(pubsubpb.BigQueryConfig_ACTIVE)
		}
		text = pubsubpb.BigQueryConfig_State(st).String()
	} else {
		if st == 0 {
			st = int32(pubsubpb.CloudStorageConfig_ACTIVE)
		}
		text = pubsubpb.CloudStorageConfig_State(st).String()
	}
	q, _ := json.Marshal(text)
	cfg["state"] = q
	b, err := encodeJSON(cfg)
	if err != nil {
		return sub, false
	}
	o[key] = b
	out, err := encodeJSON(o)
	if err != nil {
		return sub, false
	}
	return out, true
}
