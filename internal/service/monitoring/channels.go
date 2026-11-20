package monitoring

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	labelpb "google.golang.org/genproto/googleapis/api/label"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// NotificationChannelService (#1964). Channels are kept in the instance's
// state; delivery stays on the machine (notify.go): webhooks are posted
// only to local URLs and Pub/Sub channels publish to the instance's own
// Pub/Sub. Email, SMS, Slack, PagerDuty and Google Chat channels are
// stored and their notifications recorded, never sent.

// MaxChannels is "Notification channels per metrics scope: 4,000"
// (docs.cloud.google.com/monitoring/quotas, read 2026-11-19).
const MaxChannels = 4000

type channelType struct {
	t, display, desc string
	labels           []string
	sensitive        []string
	// verify is whether the channel is verified with a code, as email and
	// SMS channels are.
	verify bool
	// local is how its notifications are delivered: "webhook", "pubsub",
	// or "" for recorded only.
	local string
}

// channelTypes are the types Google documents
// (docs.cloud.google.com/monitoring/support/notification-options and
// /alerts/using-channels-api, read 2026-11-19) with their labels.
var channelTypes = []channelType{
	{t: "email", display: "Email", desc: "A channel that sends notifications via email.", labels: []string{"email_address"}, verify: true},
	{t: "google_chat", display: "Google Chat", desc: "A channel that sends notifications to a Google Chat space.", labels: []string{"space"}},
	{t: "pagerduty", display: "PagerDuty Services", desc: "A channel that sends notifications to PagerDuty.", labels: []string{"service_key"}, sensitive: []string{"service_key"}},
	{t: "pubsub", display: "Pub/Sub", desc: "A channel that sends notifications to a Pub/Sub topic.", labels: []string{"topic"}, local: "pubsub"},
	{t: "slack", display: "Slack", desc: "A channel that sends notifications to a Slack channel.", labels: []string{"channel_name", "team", "auth_token"}, sensitive: []string{"auth_token"}},
	{t: "sms", display: "SMS", desc: "A channel that sends notifications via SMS.", labels: []string{"number"}, verify: true},
	{t: "webhook_basicauth", display: "Webhook with basic auth", desc: "A channel that sends notifications to a webhook with HTTP basic authentication.", labels: []string{"url", "username", "password"}, sensitive: []string{"password"}, local: "webhook"},
	{t: "webhook_tokenauth", display: "Webhooks", desc: "A channel that sends notifications to a webhook, authenticated by a token in its URL.", labels: []string{"url"}, local: "webhook"},
}

func channelTypeOf(t string) *channelType {
	for i := range channelTypes {
		if channelTypes[i].t == t {
			return &channelTypes[i]
		}
	}
	return nil
}

func (c channelType) descriptor(project string) *monitoringpb.NotificationChannelDescriptor {
	d := &monitoringpb.NotificationChannelDescriptor{Name: "projects/" + project + "/notificationChannelDescriptors/" + c.t,
		Type: c.t, DisplayName: c.display, Description: c.desc}
	for _, l := range c.labels {
		d.Labels = append(d.Labels, &labelpb.LabelDescriptor{Key: l, ValueType: labelpb.LabelDescriptor_STRING})
	}
	return d
}

func (s *Server) ListNotificationChannelDescriptors(_ context.Context, req *monitoringpb.ListNotificationChannelDescriptorsRequest) (*monitoringpb.ListNotificationChannelDescriptorsResponse, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	var all []*monitoringpb.NotificationChannelDescriptor
	for _, c := range channelTypes {
		all = append(all, c.descriptor(project))
	}
	page, next, err := pageOf("channelDescriptors|"+req.GetName(), all, (*monitoringpb.NotificationChannelDescriptor).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListNotificationChannelDescriptorsResponse{ChannelDescriptors: page, NextPageToken: next}, nil
}

func (s *Server) GetNotificationChannelDescriptor(_ context.Context, req *monitoringpb.GetNotificationChannelDescriptorRequest) (*monitoringpb.NotificationChannelDescriptor, error) {
	project, err := childName("name", req.GetName(), "notificationChannelDescriptors")
	if err != nil {
		return nil, err
	}
	c := channelTypeOf(req.GetName()[strings.LastIndex(req.GetName(), "/")+1:])
	if c == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return c.descriptor(project), nil
}

func (s *Server) ListNotificationChannels(_ context.Context, req *monitoringpb.ListNotificationChannelsRequest) (*monitoringpb.ListNotificationChannelsResponse, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	terms, err := parseSimpleFilter("filter", req.GetFilter())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	var items []*monitoringpb.NotificationChannel
	for _, m := range s.under(kindChannels, "projects/"+project+"/") {
		if matchSimple(terms, m) {
			items = append(items, masked(m.(*monitoringpb.NotificationChannel)))
		}
	}
	s.mu.Unlock()
	page, next, err := pageOf("channels|"+req.GetName()+"|"+req.GetFilter(), items, (*monitoringpb.NotificationChannel).GetName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &monitoringpb.ListNotificationChannelsResponse{NotificationChannels: page, NextPageToken: next, TotalSize: int32(len(items))}, nil
}

// masked hides a channel's sensitive labels, as Google returns them.
func masked(c *monitoringpb.NotificationChannel) *monitoringpb.NotificationChannel {
	out := clone(c)
	if t := channelTypeOf(c.GetType()); t != nil {
		for _, k := range t.sensitive {
			if v, ok := out.Labels[k]; ok && v != "" {
				out.Labels[k] = strings.Repeat("*", len(v))
			}
		}
	}
	return out
}

func (s *Server) GetNotificationChannel(_ context.Context, req *monitoringpb.GetNotificationChannelRequest) (*monitoringpb.NotificationChannel, error) {
	if _, err := childName("name", req.GetName(), "notificationChannels"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.get(kindChannels, req.GetName()).(*monitoringpb.NotificationChannel)
	if c == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	return masked(c), nil
}

func (s *Server) CreateNotificationChannel(_ context.Context, req *monitoringpb.CreateNotificationChannelRequest) (*monitoringpb.NotificationChannel, error) {
	project, err := parseProject("name", req.GetName())
	if err != nil {
		return nil, err
	}
	if req.GetNotificationChannel() == nil {
		return nil, apierror.InvalidArgument("Request was missing field notificationChannel")
	}
	c := clone(req.GetNotificationChannel())
	c.Name = "projects/" + project + "/notificationChannels/" + newID()
	if err := checkChannel(c); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.under(kindChannels, "projects/"+project+"/")) >= MaxChannels {
		return nil, apierror.FailedPrecondition("The project %s has reached its limit of %d notification channels", project, MaxChannels)
	}
	if channelTypeOf(c.GetType()).verify {
		c.VerificationStatus = monitoringpb.NotificationChannel_UNVERIFIED
	} else {
		c.VerificationStatus = monitoringpb.NotificationChannel_VERIFICATION_STATUS_UNSPECIFIED
	}
	c.CreationRecord = s.record()
	c.MutationRecords = []*monitoringpb.MutationRecord{s.record()}
	s.put(kindChannels, c.Name, c)
	if err := s.save(); err != nil {
		return nil, err
	}
	return masked(c), nil
}

func checkChannel(c *monitoringpb.NotificationChannel) error {
	t := channelTypeOf(c.GetType())
	if c.GetType() == "" {
		return apierror.InvalidArgument("Field notificationChannel.type is required")
	}
	if t == nil {
		return apierror.InvalidArgument("Field notificationChannel.type had an invalid value of %q: unknown notification channel type", c.GetType())
	}
	known := map[string]bool{}
	for _, l := range t.labels {
		known[l] = true
	}
	for k := range c.GetLabels() {
		if !known[k] {
			return apierror.InvalidArgument("Field notificationChannel.labels had an invalid value: %q is not a label of channel type %s", k, t.t)
		}
	}
	required := map[string]string{"email": "email_address", "sms": "number", "pubsub": "topic", "webhook_tokenauth": "url",
		"webhook_basicauth": "url", "slack": "channel_name", "pagerduty": "service_key", "google_chat": "space"}
	if k := required[t.t]; c.GetLabels()[k] == "" {
		return apierror.InvalidArgument("Field notificationChannel.labels had an invalid value: the label %q is required for a %s channel", k, t.t)
	}
	if t.t == "pubsub" {
		if _, err := childName("notificationChannel.labels.topic", c.GetLabels()["topic"], "topics"); err != nil {
			return err
		}
	}
	if len(c.GetDisplayName()) > 512 {
		return apierror.InvalidArgument("Field notificationChannel.displayName had an invalid value: at most 512 characters")
	}
	if c.Enabled == nil {
		c.Enabled = wrapperspb.Bool(true)
	}
	return checkUserLabels("notificationChannel.userLabels", c.GetUserLabels())
}

func (s *Server) UpdateNotificationChannel(_ context.Context, req *monitoringpb.UpdateNotificationChannelRequest) (*monitoringpb.NotificationChannel, error) {
	in := req.GetNotificationChannel()
	if _, err := childName("notificationChannel.name", in.GetName(), "notificationChannels"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, _ := s.get(kindChannels, in.GetName()).(*monitoringpb.NotificationChannel)
	if old == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	next := clone(in)
	if len(req.GetUpdateMask().GetPaths()) > 0 {
		next = clone(old)
		if err := applyMask(next, in, req.GetUpdateMask()); err != nil {
			return nil, err
		}
	}
	// A masked sensitive value sent back is the stored one.
	if t := channelTypeOf(old.GetType()); t != nil {
		for _, k := range t.sensitive {
			if v := next.GetLabels()[k]; v != "" && strings.Trim(v, "*") == "" {
				next.Labels[k] = old.GetLabels()[k]
			}
		}
	}
	if next.GetType() != old.GetType() {
		return nil, apierror.InvalidArgument("Field notificationChannel.type had an invalid value of %q: a channel's type cannot change", next.GetType())
	}
	if err := checkChannel(next); err != nil {
		return nil, err
	}
	next.Name, next.CreationRecord = old.GetName(), old.GetCreationRecord()
	next.VerificationStatus = old.GetVerificationStatus()
	if channelTypeOf(old.GetType()).verify && !sameMap(next.GetLabels(), old.GetLabels()) {
		next.VerificationStatus = monitoringpb.NotificationChannel_UNVERIFIED
	}
	next.MutationRecords = append(append([]*monitoringpb.MutationRecord(nil), old.GetMutationRecords()...), s.record())
	s.put(kindChannels, next.Name, next)
	if err := s.save(); err != nil {
		return nil, err
	}
	return masked(next), nil
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (s *Server) DeleteNotificationChannel(_ context.Context, req *monitoringpb.DeleteNotificationChannelRequest) (*emptypb.Empty, error) {
	if _, err := childName("name", req.GetName(), "notificationChannels"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.get(kindChannels, req.GetName()) == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	var users []string
	for _, m := range s.under(kindPolicies, "") {
		p := m.(*monitoringpb.AlertPolicy)
		for _, c := range p.GetNotificationChannels() {
			if c == req.GetName() {
				users = append(users, p.GetName())
			}
		}
	}
	if len(users) > 0 && !req.GetForce() {
		return nil, apierror.FailedPrecondition("Notification channel %s is referenced by alerting policies %s; delete it with force to remove it from them", req.GetName(), strings.Join(users, ", "))
	}
	for _, n := range users {
		p := s.get(kindPolicies, n).(*monitoringpb.AlertPolicy)
		var keep []string
		for _, c := range p.GetNotificationChannels() {
			if c != req.GetName() {
				keep = append(keep, c)
			}
		}
		p.NotificationChannels = keep
	}
	delete(s.objects[kindChannels], req.GetName())
	delete(s.codes, req.GetName())
	return &emptypb.Empty{}, s.save()
}

// verificationCode is a code sent for a channel: CloudBurrow sends
// nothing, so it is recorded with the channel's notifications instead,
// where the console shows it.
type verificationCode struct {
	code   string
	expire time.Time
}

func newCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("G-%06d", n.Int64())
}

func (s *Server) SendNotificationChannelVerificationCode(_ context.Context, req *monitoringpb.SendNotificationChannelVerificationCodeRequest) (*emptypb.Empty, error) {
	if _, err := childName("name", req.GetName(), "notificationChannels"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.get(kindChannels, req.GetName()).(*monitoringpb.NotificationChannel)
	if c == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	if !channelTypeOf(c.GetType()).verify {
		return nil, apierror.InvalidArgument("Notification channel %s of type %s does not need verification", c.GetName(), c.GetType())
	}
	code := verificationCode{code: newCode(), expire: s.now().Add(time.Hour)}
	s.codes[c.GetName()] = code
	s.recordDelivery(Delivery{Channel: c.GetName(), Type: c.GetType(), Kind: "verification",
		State: DeliveryRecorded, Detail: "verification code " + code.code + " (not sent: CloudBurrow never sends email or SMS)"})
	return &emptypb.Empty{}, nil
}

func (s *Server) GetNotificationChannelVerificationCode(_ context.Context, req *monitoringpb.GetNotificationChannelVerificationCodeRequest) (*monitoringpb.GetNotificationChannelVerificationCodeResponse, error) {
	if _, err := childName("name", req.GetName(), "notificationChannels"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.get(kindChannels, req.GetName()).(*monitoringpb.NotificationChannel)
	if c == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	if c.GetVerificationStatus() != monitoringpb.NotificationChannel_VERIFIED {
		return nil, apierror.FailedPrecondition("Notification channel %s is not verified", c.GetName())
	}
	exp := s.now().Add(time.Hour)
	if req.GetExpireTime() != nil && req.GetExpireTime().AsTime().Before(exp) {
		exp = req.GetExpireTime().AsTime()
	}
	code := verificationCode{code: newCode(), expire: exp}
	s.codes[c.GetName()] = code
	return &monitoringpb.GetNotificationChannelVerificationCodeResponse{Code: code.code, ExpireTime: timestamppb.New(exp)}, nil
}

func (s *Server) VerifyNotificationChannel(_ context.Context, req *monitoringpb.VerifyNotificationChannelRequest) (*monitoringpb.NotificationChannel, error) {
	if _, err := childName("name", req.GetName(), "notificationChannels"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, _ := s.get(kindChannels, req.GetName()).(*monitoringpb.NotificationChannel)
	if c == nil {
		return nil, apierror.NotFound("Requested entity was not found.")
	}
	code, ok := s.codes[c.GetName()]
	if req.GetCode() == "" {
		return nil, apierror.InvalidArgument("Field code is required")
	}
	if !ok || code.code != req.GetCode() || s.now().After(code.expire) {
		return nil, apierror.InvalidArgument("Field code had an invalid value of %q: the verification code is wrong or has expired", req.GetCode())
	}
	delete(s.codes, c.GetName())
	c.VerificationStatus = monitoringpb.NotificationChannel_VERIFIED
	if err := s.save(); err != nil {
		return nil, err
	}
	return masked(c), nil
}
