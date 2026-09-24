package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Seeders for Cloud Storage, Pub/Sub and Secret Manager (#275).
//
// Each creates resources through the service's own API — the JSON API a
// storage client uses, the Pub/Sub admin API, the Secret Manager store the gRPC
// server calls — so a seeded resource is one an application could have made,
// and a seed cannot reach a state the API itself would refuse.
//
// Every seeder validates its whole document before the admin API seeds
// anything. Unknown fields are refused: a misspelt field that was silently
// ignored would seed something other than what was asked for. So is any field
// the backend is not known to honour, by name, for the same reason.
//
// Re-seeding a resource that exists is ALREADY_EXISTS, so a seed cannot quietly
// merge into state left from an earlier run. `ifNotExists: true` skips existing
// resources instead, which makes a seed safe to repeat.

// strictDecode decodes a component's document, refusing unknown fields and
// trailing data.
func strictDecode(spec json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the document")
	}
	return nil
}

// refuse reports fields that were set but that the backend is not known to
// honour. Accepting them would claim behaviour nothing here provides.
func refuse(where string, fields map[string]bool) error {
	var set []string
	for name, isSet := range fields {
		if isSet {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	sort.Strings(set)
	return fmt.Errorf("%s: %s not supported: the emulator is not known to honour it, "+
		"so seeding it would create a resource that silently ignores it", where, strings.Join(set, ", "))
}

// ---------------------------------------------------------------- storage

// refuseStorage is refuse for bucket fields, whose reason is measured rather
// than unknown: the backend discards them.
func refuseStorage(where string, fields map[string]bool) error {
	var set []string
	for name, isSet := range fields {
		if isSet {
			set = append(set, name)
		}
	}
	if len(set) == 0 {
		return nil
	}
	sort.Strings(set)
	return fmt.Errorf("%s: %s not supported: the storage backend does not keep it, "+
		"so the bucket would read back without it", where, strings.Join(set, ", "))
}

type storageSeed struct {
	IfNotExists bool         `json:"ifNotExists"`
	Buckets     []bucketSeed `json:"buckets"`
}

type bucketSeed struct {
	Name    string       `json:"name"`
	Objects []objectSeed `json:"objects,omitempty"`

	// Refused by name: the storage backend does not keep them. A bucket
	// created with labels reads back with none (measured through the official
	// client in CI), so seeding them would report state that is not there.
	Location     string            `json:"location,omitempty"`
	StorageClass string            `json:"storageClass,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

type objectSeed struct {
	Name string `json:"name"`
	// Exactly one of Content and ContentBase64 may be set; neither is an
	// empty object.
	Content       *string           `json:"content,omitempty"`
	ContentBase64 *string           `json:"contentBase64,omitempty"`
	ContentType   string            `json:"contentType,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

func (o objectSeed) bytes() ([]byte, error) {
	switch {
	case o.Content != nil && o.ContentBase64 != nil:
		return nil, errors.New("content and contentBase64 are exclusive")
	case o.ContentBase64 != nil:
		b, err := base64.StdEncoding.DecodeString(*o.ContentBase64)
		if err != nil {
			return nil, fmt.Errorf("contentBase64: %w", err)
		}
		return b, nil
	case o.Content != nil:
		return []byte(*o.Content), nil
	default:
		return nil, nil
	}
}

// bucketName is Cloud Storage's naming rule, less the dotted-name and
// IP-address refinements a local bucket does not need.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$`)

type storageSeeder struct {
	// front is the fronted storage endpoint, the one clients use, so a seeded
	// upload is seen by the notification router exactly as a client's is.
	front   func() string
	project string
}

func (s *storageSeeder) Name() string { return "storage" }

func (s *storageSeeder) parse(spec json.RawMessage) (storageSeed, error) {
	var doc storageSeed
	if err := strictDecode(spec, &doc); err != nil {
		return doc, err
	}
	seen := map[string]bool{}
	for i, b := range doc.Buckets {
		where := fmt.Sprintf("buckets[%d]", i)
		if !bucketName.MatchString(b.Name) {
			return doc, fmt.Errorf("%s.name %q is not a valid bucket name", where, b.Name)
		}
		if seen[b.Name] {
			return doc, fmt.Errorf("%s.name %q appears twice", where, b.Name)
		}
		seen[b.Name] = true
		if err := refuseStorage(where, map[string]bool{
			"labels": len(b.Labels) > 0, "location": b.Location != "", "storageClass": b.StorageClass != "",
		}); err != nil {
			return doc, err
		}
		objects := map[string]bool{}
		for j, o := range b.Objects {
			where := fmt.Sprintf("%s.objects[%d]", where, j)
			if o.Name == "" {
				return doc, fmt.Errorf("%s.name is required", where)
			}
			if objects[o.Name] {
				return doc, fmt.Errorf("%s.name %q appears twice", where, o.Name)
			}
			objects[o.Name] = true
			if _, err := o.bytes(); err != nil {
				return doc, fmt.Errorf("%s: %w", where, err)
			}
		}
	}
	return doc, nil
}

func (s *storageSeeder) Validate(spec json.RawMessage) error {
	_, err := s.parse(spec)
	return err
}

func (s *storageSeeder) Seed(ctx context.Context, spec json.RawMessage) error {
	doc, err := s.parse(spec)
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	addr := s.front()
	if addr == "" {
		return errors.New("Cloud Storage is not running")
	}
	base := "http://" + addr
	c := &http.Client{Timeout: 60 * time.Second}

	for _, b := range doc.Buckets {
		body, _ := json.Marshal(map[string]any{"name": b.Name})
		code, msg, err := gcsCall(ctx, c, http.MethodPost,
			base+"/storage/v1/b?project="+url.QueryEscape(s.project), "application/json", body)
		if err != nil {
			return fmt.Errorf("create bucket %s: %w", b.Name, err)
		}
		switch {
		case code == http.StatusConflict && doc.IfNotExists:
		case code == http.StatusConflict:
			return apierror.AlreadyExists("bucket %s already exists; set ifNotExists to skip it", b.Name)
		case code/100 != 2:
			return fmt.Errorf("create bucket %s: %d %s", b.Name, code, msg)
		}

		for _, o := range b.Objects {
			if err := s.upload(ctx, c, base, b.Name, o, doc.IfNotExists); err != nil {
				return err
			}
		}
	}
	return nil
}

// upload writes one object with its metadata in a single multipart request,
// conditional on the object not existing (ifGenerationMatch=0). The condition
// is what makes a repeated seed fail, or skip, rather than overwrite.
func (s *storageSeeder) upload(ctx context.Context, c *http.Client, base, bucket string, o objectSeed, ifNotExists bool) error {
	content, _ := o.bytes()
	contentType := o.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	meta, _ := json.Marshal(map[string]any{
		"name": o.Name, "contentType": contentType, "metadata": o.Metadata,
	})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	_, _ = part.Write(meta)
	part, _ = mw.CreatePart(textproto.MIMEHeader{"Content-Type": {contentType}})
	_, _ = part.Write(content)
	_ = mw.Close()

	u := base + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=multipart&ifGenerationMatch=0"
	code, msg, err := gcsCall(ctx, c, http.MethodPost, u, "multipart/related; boundary="+mw.Boundary(), buf.Bytes())
	if err != nil {
		return fmt.Errorf("upload gs://%s/%s: %w", bucket, o.Name, err)
	}
	switch {
	case code == http.StatusPreconditionFailed && ifNotExists:
		return nil
	case code == http.StatusPreconditionFailed:
		return apierror.AlreadyExists("object gs://%s/%s already exists; set ifNotExists to skip it", bucket, o.Name)
	case code/100 != 2:
		return fmt.Errorf("upload gs://%s/%s: %d %s", bucket, o.Name, code, msg)
	}
	return nil
}

func gcsCall(ctx context.Context, c *http.Client, method, u, contentType string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, strings.TrimSpace(string(msg)), nil
}

// ----------------------------------------------------------------- pubsub

type pubsubSeed struct {
	IfNotExists   bool               `json:"ifNotExists"`
	Topics        []topicSeed        `json:"topics"`
	Subscriptions []subscriptionSeed `json:"subscriptions"`
}

type topicSeed struct {
	Name                     string            `json:"name"`
	Labels                   map[string]string `json:"labels,omitempty"`
	MessageRetentionDuration string            `json:"messageRetentionDuration,omitempty"`

	// Refused by name; see refuse.
	SchemaSettings json.RawMessage `json:"schemaSettings,omitempty"`
	KmsKeyName     string          `json:"kmsKeyName,omitempty"`
}

type subscriptionSeed struct {
	Name                  string            `json:"name"`
	Topic                 string            `json:"topic"`
	AckDeadlineSeconds    int32             `json:"ackDeadlineSeconds,omitempty"`
	Filter                string            `json:"filter,omitempty"`
	Labels                map[string]string `json:"labels,omitempty"`
	EnableMessageOrdering bool              `json:"enableMessageOrdering,omitempty"`
	PushConfig            *struct {
		PushEndpoint string            `json:"pushEndpoint"`
		Attributes   map[string]string `json:"attributes,omitempty"`
	} `json:"pushConfig,omitempty"`
	DeadLetterPolicy *struct {
		DeadLetterTopic     string `json:"deadLetterTopic"`
		MaxDeliveryAttempts int32  `json:"maxDeliveryAttempts,omitempty"`
	} `json:"deadLetterPolicy,omitempty"`
	RetryPolicy *struct {
		MinimumBackoff string `json:"minimumBackoff,omitempty"`
		MaximumBackoff string `json:"maximumBackoff,omitempty"`
	} `json:"retryPolicy,omitempty"`

	// Refused by name; see refuse.
	BigqueryConfig            json.RawMessage `json:"bigqueryConfig,omitempty"`
	CloudStorageConfig        json.RawMessage `json:"cloudStorageConfig,omitempty"`
	EnableExactlyOnceDelivery bool            `json:"enableExactlyOnceDelivery,omitempty"`
}

var (
	topicName        = regexp.MustCompile(`^projects/([a-z][a-z0-9-]{4,28}[a-z0-9])/topics/([A-Za-z][A-Za-z0-9._~+%-]{2,254})$`)
	subscriptionName = regexp.MustCompile(`^projects/([a-z][a-z0-9-]{4,28}[a-z0-9])/subscriptions/([A-Za-z][A-Za-z0-9._~+%-]{2,254})$`)
)

// protoDuration parses the JSON form of a protobuf Duration, such as "600s"
// or "3.5s".
func protoDuration(s string) (*durationpb.Duration, error) {
	if s == "" {
		return nil, nil
	}
	secs, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil || !strings.HasSuffix(s, "s") || secs < 0 {
		return nil, fmt.Errorf("%q is not a duration such as \"600s\"", s)
	}
	return durationpb.New(time.Duration(secs * float64(time.Second))), nil
}

type pubsubSeeder struct{ tunnel *netfwd.Forwarder }

func (p *pubsubSeeder) Name() string { return "pubsub" }

func (p *pubsubSeeder) parse(spec json.RawMessage) (pubsubSeed, []*pubsubpb.Topic, []*pubsubpb.Subscription, error) {
	var doc pubsubSeed
	if err := strictDecode(spec, &doc); err != nil {
		return doc, nil, nil, err
	}
	var topics []*pubsubpb.Topic
	seen := map[string]bool{}
	for i, t := range doc.Topics {
		where := fmt.Sprintf("topics[%d]", i)
		if !topicName.MatchString(t.Name) {
			return doc, nil, nil, fmt.Errorf("%s.name %q is not projects/{project}/topics/{topic}", where, t.Name)
		}
		if seen[t.Name] {
			return doc, nil, nil, fmt.Errorf("%s.name %q appears twice", where, t.Name)
		}
		seen[t.Name] = true
		if err := refuse(where, map[string]bool{
			"schemaSettings": len(t.SchemaSettings) > 0, "kmsKeyName": t.KmsKeyName != "",
		}); err != nil {
			return doc, nil, nil, err
		}
		retention, err := protoDuration(t.MessageRetentionDuration)
		if err != nil {
			return doc, nil, nil, fmt.Errorf("%s.messageRetentionDuration: %w", where, err)
		}
		topics = append(topics, &pubsubpb.Topic{Name: t.Name, Labels: t.Labels, MessageRetentionDuration: retention})
	}

	var subs []*pubsubpb.Subscription
	seen = map[string]bool{}
	for i, s := range doc.Subscriptions {
		where := fmt.Sprintf("subscriptions[%d]", i)
		if !subscriptionName.MatchString(s.Name) {
			return doc, nil, nil, fmt.Errorf("%s.name %q is not projects/{project}/subscriptions/{subscription}", where, s.Name)
		}
		if seen[s.Name] {
			return doc, nil, nil, fmt.Errorf("%s.name %q appears twice", where, s.Name)
		}
		seen[s.Name] = true
		if !topicName.MatchString(s.Topic) {
			return doc, nil, nil, fmt.Errorf("%s.topic %q is not projects/{project}/topics/{topic}", where, s.Topic)
		}
		if err := refuse(where, map[string]bool{
			"bigqueryConfig":            len(s.BigqueryConfig) > 0,
			"cloudStorageConfig":        len(s.CloudStorageConfig) > 0,
			"enableExactlyOnceDelivery": s.EnableExactlyOnceDelivery,
		}); err != nil {
			return doc, nil, nil, err
		}
		if s.AckDeadlineSeconds != 0 && (s.AckDeadlineSeconds < 10 || s.AckDeadlineSeconds > 600) {
			return doc, nil, nil, fmt.Errorf("%s.ackDeadlineSeconds must be between 10 and 600", where)
		}
		sub := &pubsubpb.Subscription{
			Name: s.Name, Topic: s.Topic, AckDeadlineSeconds: s.AckDeadlineSeconds,
			Filter: s.Filter, Labels: s.Labels, EnableMessageOrdering: s.EnableMessageOrdering,
		}
		if s.PushConfig != nil {
			if u, err := url.Parse(s.PushConfig.PushEndpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return doc, nil, nil, fmt.Errorf("%s.pushConfig.pushEndpoint %q is not an http(s) URL", where, s.PushConfig.PushEndpoint)
			}
			sub.PushConfig = &pubsubpb.PushConfig{PushEndpoint: s.PushConfig.PushEndpoint, Attributes: s.PushConfig.Attributes}
		}
		if d := s.DeadLetterPolicy; d != nil {
			if !topicName.MatchString(d.DeadLetterTopic) {
				return doc, nil, nil, fmt.Errorf("%s.deadLetterPolicy.deadLetterTopic %q is not projects/{project}/topics/{topic}", where, d.DeadLetterTopic)
			}
			if d.MaxDeliveryAttempts != 0 && (d.MaxDeliveryAttempts < 5 || d.MaxDeliveryAttempts > 100) {
				return doc, nil, nil, fmt.Errorf("%s.deadLetterPolicy.maxDeliveryAttempts must be between 5 and 100", where)
			}
			sub.DeadLetterPolicy = &pubsubpb.DeadLetterPolicy{DeadLetterTopic: d.DeadLetterTopic, MaxDeliveryAttempts: d.MaxDeliveryAttempts}
		}
		if r := s.RetryPolicy; r != nil {
			minB, err := protoDuration(r.MinimumBackoff)
			if err != nil {
				return doc, nil, nil, fmt.Errorf("%s.retryPolicy.minimumBackoff: %w", where, err)
			}
			maxB, err := protoDuration(r.MaximumBackoff)
			if err != nil {
				return doc, nil, nil, fmt.Errorf("%s.retryPolicy.maximumBackoff: %w", where, err)
			}
			sub.RetryPolicy = &pubsubpb.RetryPolicy{MinimumBackoff: minB, MaximumBackoff: maxB}
		}
		subs = append(subs, sub)
	}
	return doc, topics, subs, nil
}

func (p *pubsubSeeder) Validate(spec json.RawMessage) error {
	_, _, _, err := p.parse(spec)
	return err
}

func (p *pubsubSeeder) Seed(ctx context.Context, spec json.RawMessage) error {
	doc, topics, subs, err := p.parse(spec)
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	// The project a client is built for only supplies defaults; every name
	// here is fully qualified, so one client serves them all.
	c, err := pubsubAdmin(ctx, p.tunnel, "cloudburrow-seed")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	exists := func(what, name string, err error) error {
		if status.Code(err) == codes.AlreadyExists {
			if doc.IfNotExists {
				return nil
			}
			return apierror.AlreadyExists("%s %s already exists; set ifNotExists to skip it", what, name)
		}
		if err != nil {
			return fmt.Errorf("create %s %s: %w", what, name, err)
		}
		return nil
	}
	// Topics first: a subscription, and a dead-letter policy, name one.
	for _, t := range topics {
		_, err := c.TopicAdminClient.CreateTopic(ctx, t)
		if err := exists("topic", t.Name, err); err != nil {
			return err
		}
	}
	for _, s := range subs {
		_, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s)
		if err := exists("subscription", s.Name, err); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------- secretmanager

type secretsSeed struct {
	IfNotExists bool         `json:"ifNotExists"`
	Secrets     []secretSeed `json:"secrets"`
}

type secretSeed struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// Versions are added in order, so the last is "latest".
	Versions []versionSeed `json:"versions,omitempty"`
}

type versionSeed struct {
	Data       *string `json:"data,omitempty"`
	DataBase64 *string `json:"dataBase64,omitempty"`
}

func (v versionSeed) bytes() ([]byte, error) {
	switch {
	case v.Data != nil && v.DataBase64 != nil:
		return nil, errors.New("data and dataBase64 are exclusive")
	case v.DataBase64 != nil:
		b, err := base64.StdEncoding.DecodeString(*v.DataBase64)
		if err != nil {
			return nil, fmt.Errorf("dataBase64: %w", err)
		}
		return b, nil
	case v.Data != nil:
		return []byte(*v.Data), nil
	default:
		return nil, errors.New("one of data or dataBase64 is required")
	}
}

var secretName = regexp.MustCompile(`^projects/([^/]+)/secrets/([A-Za-z0-9_-]{1,255})$`)

type secretsSeeder struct{ svc *secretsService }

func (s *secretsSeeder) Name() string { return "secretmanager" }

func (s *secretsSeeder) parse(spec json.RawMessage) (secretsSeed, error) {
	var doc secretsSeed
	if err := strictDecode(spec, &doc); err != nil {
		return doc, err
	}
	seen := map[string]bool{}
	for i, sec := range doc.Secrets {
		where := fmt.Sprintf("secrets[%d]", i)
		if !secretName.MatchString(sec.Name) {
			return doc, fmt.Errorf("%s.name %q is not projects/{project}/secrets/{secret}", where, sec.Name)
		}
		if seen[sec.Name] {
			return doc, fmt.Errorf("%s.name %q appears twice", where, sec.Name)
		}
		seen[sec.Name] = true
		for j, v := range sec.Versions {
			if _, err := v.bytes(); err != nil {
				return doc, fmt.Errorf("%s.versions[%d]: %w", where, j, err)
			}
		}
	}
	return doc, nil
}

func (s *secretsSeeder) Validate(spec json.RawMessage) error {
	_, err := s.parse(spec)
	return err
}

// Seed creates each secret and adds its versions.
//
// With ifNotExists, a secret that exists is skipped whole, versions included.
// Versions have no names to match against, so adding them to an existing
// secret would duplicate them on every repeat, which is what ifNotExists
// promises not to do.
func (s *secretsSeeder) Seed(_ context.Context, spec json.RawMessage) error {
	doc, err := s.parse(spec)
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	st := s.svc.Store()
	if st == nil {
		return errors.New("Secret Manager is not running")
	}
	for _, sec := range doc.Secrets {
		m := secretName.FindStringSubmatch(sec.Name)
		project, id := m[1], m[2]
		_, err := st.CreateSecret(project, id, sec.Labels, sec.Annotations, "")
		if status.Code(err) == codes.AlreadyExists {
			if doc.IfNotExists {
				continue
			}
			return apierror.AlreadyExists("secret %s already exists; set ifNotExists to skip it", sec.Name)
		}
		if err != nil {
			return fmt.Errorf("create secret %s: %w", sec.Name, err)
		}
		for _, v := range sec.Versions {
			data, _ := v.bytes()
			if _, err := st.AddVersion(project, id, data); err != nil {
				return fmt.Errorf("add a version to %s: %w", sec.Name, err)
			}
		}
	}
	return nil
}
