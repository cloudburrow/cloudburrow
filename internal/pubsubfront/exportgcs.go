package pubsubfront

// Cloud Storage subscriptions (#1874), to
// https://docs.cloud.google.com/pubsub/docs/cloudstorage and
// https://docs.cloud.google.com/pubsub/docs/create-cloudstorage-subscription:
//
//   - messages are batched, and each batch is one object, written once the
//     batch reaches max_messages (at least 1000), max_bytes (1 KB to 10
//     GiB) or max_duration (1 to 10 minutes, 5 by default) since it began,
//     whichever comes first; max_duration runs on the front's clock, which
//     ClockMethod advances;
//   - an object is named "<file-prefix><UTC-date-time>_<uuid><file-suffix>",
//     the date-time in filename_datetime_format (YYYY-MM-DDThh:mm:ss+00:00
//     by default), and a format that ends in a character that is not a
//     matcher has that character in place of the "_";
//   - text output is the messages' data, each separated from the one
//     before by a newline; Avro output is an Avro object container file of
//     com.google.pubsub.PubsubMessage records (data), or, with
//     write_metadata, PubsubMessageWithMetadata records (subscription_name,
//     message_id, publish_time, attributes, data);
//   - a message is acknowledged once the object holding it is written.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/linkedin/goavro/v2"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	defaultMaxDuration = 5 * time.Minute
	minMaxDuration     = time.Minute
	maxMaxDuration     = 10 * time.Minute
	minMaxBytes        = 1000
	maxMaxBytes        = 10 << 30
	minMaxMessages     = 1000
	// defaultDatetimeFormat is the date-time of Google's default names,
	// such as 2023-09-25T04:10:00+00:00.
	defaultDatetimeFormat = "YYYY-MM-DDThh:mm:ss+00:00"
)

const avroMessageSchema = `{"type":"record","namespace":"com.google.pubsub","name":"PubsubMessage",` +
	`"fields":[{"name":"data","type":"bytes"}]}`

const avroMetadataSchema = `{"type":"record","namespace":"com.google.pubsub","name":"PubsubMessageWithMetadata",` +
	`"fields":[{"name":"subscription_name","type":"string"},{"name":"message_id","type":"string"},` +
	`{"name":"publish_time","type":{"type":"long","logicalType":"timestamp-micros"}},` +
	`{"name":"attributes","type":{"type":"map","values":"string"}},{"name":"data","type":"bytes"}]}`

// datetimeToken is one element of a filename_datetime_format.
type datetimeToken struct {
	text   string
	layout string // Go time layout
	// group is the matcher it is; "" for a separator, usable any number
	// of times.
	group string
}

var datetimeTokens = []datetimeToken{
	{"YYYY", "2006", "year"}, {"YY", "06", "year"}, {"MM", "01", "month"}, {"DD", "02", "day"},
	{"hh", "15", "hour"}, {"mm", "04", "minute"}, {"ss", "05", "second"},
	{"+00:00", "+00:00", "zone"}, {"T", "T", "T"}, {"Z", "Z", "zone"},
	{"-", "-", ""}, {"_", "_", ""}, {":", ":", ""}, {"/", "/", ""},
}

// parseDatetimeFormat checks a filename_datetime_format and returns its Go
// layout and whether it ends in a separator.
func parseDatetimeFormat(f string) (string, bool, error) {
	seen := map[string]bool{}
	var layout strings.Builder
	endsInSeparator := false
	for i := 0; i < len(f); {
		matched := false
		for _, t := range datetimeTokens {
			if !strings.HasPrefix(f[i:], t.text) {
				continue
			}
			if t.group != "" {
				if seen[t.group] {
					return "", false, apierror.InvalidArgument("Invalid filename_datetime_format %q: each matcher can be used only once.", f)
				}
				seen[t.group] = true
			}
			layout.WriteString(t.layout)
			endsInSeparator = t.group == ""
			i += len(t.text)
			matched = true
			break
		}
		if !matched {
			return "", false, apierror.InvalidArgument("Invalid filename_datetime_format %q: %q is not a matcher or a separator.", f, f[i:i+1])
		}
	}
	for _, g := range []string{"year", "month", "day", "hour", "minute", "second"} {
		if !seen[g] {
			return "", false, apierror.InvalidArgument("Invalid filename_datetime_format %q: it must have year, month, day, hour, minute and second matchers.", f)
		}
	}
	return layout.String(), endsInSeparator, nil
}

// objectName names a batch begun at t.
func objectName(cfg *pubsubpb.CloudStorageConfig, t time.Time) string {
	format := cfg.GetFilenameDatetimeFormat()
	if format == "" {
		format = defaultDatetimeFormat
	}
	layout, endsInSeparator, err := parseDatetimeFormat(format)
	if err != nil {
		layout, endsInSeparator, _ = parseDatetimeFormat(defaultDatetimeFormat)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	uuid := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	sep := "_"
	if endsInSeparator {
		sep = ""
	}
	return cfg.GetFilenamePrefix() + t.UTC().Format(layout) + sep + uuid + cfg.GetFilenameSuffix()
}

func storageMaxDuration(cfg *pubsubpb.CloudStorageConfig) time.Duration {
	if cfg.GetMaxDuration() == nil {
		return defaultMaxDuration
	}
	return cfg.GetMaxDuration().AsDuration()
}

// checkStorageConfig is CreateSubscription's and UpdateSubscription's
// check of a cloud_storage_config.
func (f *Front) checkStorageConfig(ctx context.Context, s *pubsubpb.Subscription) error {
	cfg := s.GetCloudStorageConfig()
	if cfg.GetBucket() == "" || strings.HasPrefix(cfg.GetBucket(), "gs://") || strings.Contains(cfg.GetBucket(), "/") {
		return apierror.InvalidArgument("Invalid Cloud Storage bucket name %q: name the bucket alone, without gs:// or a path.", cfg.GetBucket())
	}
	if strings.HasSuffix(cfg.GetFilenameSuffix(), "/") {
		return apierror.InvalidArgument("The filename suffix cannot end in \"/\".")
	}
	for _, p := range []string{cfg.GetFilenamePrefix(), cfg.GetFilenameSuffix()} {
		if strings.ContainsAny(p, "\r\n") || strings.HasPrefix(p, ".well-known/acme-challenge/") || p == "." || p == ".." {
			return apierror.InvalidArgument("The filename prefix and suffix must follow Cloud Storage's object naming requirements.")
		}
	}
	if cfg.GetFilenameDatetimeFormat() != "" {
		if _, _, err := parseDatetimeFormat(cfg.GetFilenameDatetimeFormat()); err != nil {
			return err
		}
	}
	if d := cfg.GetMaxDuration(); d != nil && (d.AsDuration() < minMaxDuration || d.AsDuration() > maxMaxDuration) {
		return apierror.InvalidArgument("max_duration must be between 1 minute and 10 minutes.")
	}
	if b := cfg.GetMaxBytes(); b != 0 && (b < minMaxBytes || b > maxMaxBytes) {
		return apierror.InvalidArgument("max_bytes must be between 1 KB and 10 GiB.")
	}
	if m := cfg.GetMaxMessages(); m != 0 && m < minMaxMessages {
		return apierror.InvalidArgument("max_messages must be at least 1000.")
	}
	if cfg.GetAvroConfig().GetUseTopicSchema() {
		_, err := f.storageTopicSchema(ctx, s)
		if _, ok := destinationState(err); ok {
			return apierror.InvalidArgument("%s", err.Error())
		}
		// Any other error: the emulator answers for the topic.
	}
	f.exp.init()
	_, err := f.exp.bucket(forProject(ctx, projectOf(s.GetName())), cfg.GetBucket())
	if st, ok := destinationState(err); ok {
		if stateLoad(ctx) {
			return nil // restored as saved; its delivery reports the state
		}
		if st == int32(pubsubpb.CloudStorageConfig_NOT_FOUND) {
			return apierror.NotFound("Cannot find the Cloud Storage bucket %s.", cfg.GetBucket())
		}
		return apierror.FailedPrecondition("%s", err.Error())
	}
	if err != nil {
		return apierror.Internal(err, "Cannot read the Cloud Storage bucket %s.", cfg.GetBucket())
	}
	return nil
}

// bucket checks a bucket exists, and returns its location.
func (e *exporter) bucket(ctx context.Context, name string) (string, error) {
	if e.storage == "" {
		return "", &errDestination{int32(pubsubpb.CloudStorageConfig_PERMISSION_DENIED),
			"Cannot write to the Cloud Storage bucket: storage.googleapis.com is not enabled for the project."}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.storage+"/storage/v1/b/"+url.PathEscape(name), nil)
	if err != nil {
		return "", err
	}
	resp, err := e.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", &errDestination{int32(pubsubpb.CloudStorageConfig_NOT_FOUND), "Cannot write to the Cloud Storage bucket " + name + ": it does not exist."}
	case resp.StatusCode == http.StatusForbidden:
		return "", &errDestination{int32(pubsubpb.CloudStorageConfig_PERMISSION_DENIED), "Cannot write to the Cloud Storage bucket " + name + ": permission denied."}
	case resp.StatusCode/100 != 2:
		return "", fmt.Errorf("Cloud Storage buckets.get %s: %s", name, resp.Status)
	}
	var b struct {
		Location string `json:"location"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Location, nil
}

// storageTopicSchema is the topic's schema an Avro subscription with
// use_topic_schema writes in. Its errors with a state are the
// subscription's SCHEMA_MISMATCH: the topic has no schema, or one that
// is not Avro, or, with write_metadata, one whose root is not a record or
// has a field of a metadata name.
func (f *Front) storageTopicSchema(ctx context.Context, s *pubsubpb.Subscription) (*topicMsgSchema, error) {
	bad := func(format string, a ...any) error {
		return &errDestination{int32(pubsubpb.CloudStorageConfig_SCHEMA_MISMATCH), fmt.Sprintf(format, a...)}
	}
	schema, err := f.topicMessageSchema(ctx, s.GetTopic())
	if err == errNoTopicSchema {
		return nil, bad("avro_config.use_topic_schema is set, and the topic %s has no schema.", s.GetTopic())
	}
	if err != nil {
		return nil, err
	}
	meta := s.GetCloudStorageConfig().GetAvroConfig().GetWriteMetadata()
	for _, r := range schema.revisions {
		if r.codec == nil {
			return nil, bad("avro_config.use_topic_schema needs the topic's schema to be in Avro format.")
		}
		if !meta {
			continue
		}
		if r.root.kind != "record" {
			return nil, bad("With avro_config.write_metadata, the topic's schema must have a Record at its root.")
		}
		for _, m := range r.root.fields {
			for _, md := range metadataColumns {
				if m.name == md.name {
					return nil, bad("With avro_config.write_metadata, the topic's schema cannot contain a field named %s.", m.name)
				}
			}
		}
	}
	return schema, nil
}

// avroWriterSchema is the schema of the objects a revision's messages are
// written in: the topic's, with write_metadata's fields first in its root
// record.
func avroWriterSchema(r schemaRevision, meta bool) (string, error) {
	if !meta {
		return r.codec.Schema(), nil
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(r.codec.Schema()), &root); err != nil {
		return "", err
	}
	fields, _ := root["fields"].([]any)
	var withMeta []any
	if err := json.Unmarshal([]byte(`[{"name":"subscription_name","type":"string"},{"name":"message_id","type":"string"},`+
		`{"name":"publish_time","type":{"type":"long","logicalType":"timestamp-micros"}},`+
		`{"name":"attributes","type":{"type":"map","values":"string"}}]`), &withMeta); err != nil {
		return "", err
	}
	root["fields"] = append(withMeta, fields...)
	b, err := json.Marshal(root)
	return string(b), err
}

// upload writes an object.
func (e *exporter) upload(ctx context.Context, bucket, name, contentType string, body []byte) error {
	u := e.storage + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := e.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &errDestination{int32(pubsubpb.CloudStorageConfig_NOT_FOUND), "Cannot write to the Cloud Storage bucket " + bucket + ": it does not exist."}
	case resp.StatusCode == http.StatusForbidden:
		return &errDestination{int32(pubsubpb.CloudStorageConfig_PERMISSION_DENIED), "Cannot write to the Cloud Storage bucket " + bucket + ": permission denied."}
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("Cloud Storage upload %s/%s: %s: %s", bucket, name, resp.Status, strings.TrimSpace(string(rb)))
	}
	return nil
}

// batchObject is one object of a batch, and the messages it holds.
type batchObject struct {
	body        []byte
	contentType string
	msgs        []pulled
}

// metadataRecord adds write_metadata's fields to an Avro record.
func metadataRecord(s *pubsubpb.Subscription, r map[string]any, m *pubsubpb.PubsubMessage) {
	attrs := map[string]any{}
	for k, v := range m.GetAttributes() {
		attrs[k] = v
	}
	r["subscription_name"] = s.GetName()
	r["message_id"] = m.GetMessageId()
	r["publish_time"] = m.GetPublishTime().AsTime().UTC()
	r["attributes"] = attrs
}

// encodeBatch is a batch's objects, and the messages that could not be
// written in any (in the topic's schema, for use_topic_schema).
func encodeBatch(s *pubsubpb.Subscription, schema *topicMsgSchema, ps []pulled) ([]batchObject, []failed, error) {
	cfg := s.GetCloudStorageConfig()
	if cfg.GetAvroConfig() == nil {
		parts := make([][]byte, len(ps))
		for i, p := range ps {
			parts[i] = p.msg.GetData()
		}
		return []batchObject{{bytes.Join(parts, []byte("\n")), "text/plain", ps}}, nil, nil
	}
	meta := cfg.GetAvroConfig().GetWriteMetadata()
	if cfg.GetAvroConfig().GetUseTopicSchema() && schema != nil {
		return encodeTopicSchemaBatch(s, schema, ps)
	}
	schemaText := avroMessageSchema
	if meta {
		schemaText = avroMetadataSchema
	}
	recs := make([]any, 0, len(ps))
	for _, p := range ps {
		r := map[string]any{"data": p.msg.GetData()}
		if meta {
			metadataRecord(s, r, p.msg)
		}
		recs = append(recs, r)
	}
	body, err := avroFile(schemaText, recs)
	if err != nil {
		return nil, nil, err
	}
	return []batchObject{{body, "application/octet-stream", ps}}, nil, nil
}

func avroFile(schema string, recs []any) ([]byte, error) {
	var buf bytes.Buffer
	w, err := goavro.NewOCFWriter(goavro.OCFConfig{W: &buf, Schema: schema})
	if err != nil {
		return nil, err
	}
	if err := w.Append(recs); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeTopicSchemaBatch writes each message in the newest revision of
// the topic's schema it decodes in, one object per revision.
func encodeTopicSchemaBatch(s *pubsubpb.Subscription, schema *topicMsgSchema, ps []pulled) ([]batchObject, []failed, error) {
	meta := s.GetCloudStorageConfig().GetAvroConfig().GetWriteMetadata()
	recs := make([][]any, len(schema.revisions))
	msgs := make([][]pulled, len(schema.revisions))
	var bad []failed
	for _, p := range ps {
		var last error
		done := false
		for i, r := range schema.revisions {
			var native any
			var err error
			if schema.enc == pubsubpb.Encoding_JSON {
				native, _, err = r.codec.NativeFromTextual(p.msg.GetData())
			} else {
				native, _, err = r.codec.NativeFromBinary(p.msg.GetData())
			}
			if err != nil {
				last = err
				continue
			}
			if meta {
				rec, _ := native.(map[string]any)
				if rec == nil {
					rec = map[string]any{}
				}
				metadataRecord(s, rec, p.msg)
				native = rec
			}
			recs[i] = append(recs[i], native)
			msgs[i] = append(msgs[i], p)
			done = true
			break
		}
		if !done {
			bad = append(bad, failed{p, fmt.Sprintf("the message does not match the topic's schema: %v", last)})
		}
	}
	var out []batchObject
	for i, r := range schema.revisions {
		if len(recs[i]) == 0 {
			continue
		}
		text, err := avroWriterSchema(r, meta)
		if err != nil {
			return nil, nil, err
		}
		body, err := avroFile(text, recs[i])
		if err != nil {
			return nil, nil, err
		}
		out = append(out, batchObject{body, "application/octet-stream", msgs[i]})
	}
	return out, bad, nil
}

// runStorageExport delivers s until ctx ends.
func (f *Front) runStorageExport(ctx context.Context, s *pubsubpb.Subscription) {
	ctx = forProject(ctx, projectOf(s.GetName()))
	cfg := s.GetCloudStorageConfig()
	var batch []pulled
	var size int64
	var began time.Time
	lastExtend := time.Now()
	release := func() {
		var ids []string
		for _, p := range batch {
			ids = append(ids, p.ackID)
		}
		// The messages are delivered again later, as a failed write's are.
		f.extend(context.Background(), s.GetName(), ids, 0)
		batch, size = nil, 0
	}
	defer func() { release() }()
	for ctx.Err() == nil {
		var schema *topicMsgSchema
		loc, err := f.exp.bucket(ctx, cfg.GetBucket())
		if err == nil && !f.inTransitAllowed(s.GetTopic(), loc) {
			err = &errDestination{int32(pubsubpb.CloudStorageConfig_IN_TRANSIT_LOCATION_RESTRICTION),
				"Cannot write to the Cloud Storage bucket " + cfg.GetBucket() + ": enforce_in_transit is set and its location " + loc + " is not one of the topic's allowed regions."}
		}
		if err == nil && cfg.GetAvroConfig().GetUseTopicSchema() {
			schema, err = f.storageTopicSchema(ctx, s)
		}
		if err != nil {
			if st, ok := destinationState(err); ok {
				f.exp.setState(s.GetName(), st)
			}
			release()
			if !sleep(ctx, exportCheck) {
				return
			}
			continue
		}
		f.exp.setState(s.GetName(), int32(pubsubpb.CloudStorageConfig_ACTIVE))
		max := int32(500)
		if m := cfg.GetMaxMessages(); m > 0 && m-int64(len(batch)) < int64(max) {
			max = int32(m - int64(len(batch)))
		}
		ps, err := f.pull(ctx, s.GetName(), max)
		if err == nil && len(ps) > 0 {
			var ids []string
			for _, p := range ps {
				ids = append(ids, p.ackID)
				size += int64(len(p.msg.GetData()))
			}
			// Held until the batch is written.
			f.extend(ctx, s.GetName(), ids, 600)
			if len(batch) == 0 {
				began = f.clock.Now()
			}
			batch = append(batch, ps...)
		}
		if len(batch) > 0 && time.Since(lastExtend) > time.Minute {
			var ids []string
			for _, p := range batch {
				ids = append(ids, p.ackID)
			}
			f.extend(ctx, s.GetName(), ids, 600)
			lastExtend = time.Now()
		}
		full := len(batch) > 0 && ((cfg.GetMaxMessages() > 0 && int64(len(batch)) >= cfg.GetMaxMessages()) ||
			(cfg.GetMaxBytes() > 0 && size >= cfg.GetMaxBytes()) ||
			f.clock.Now().Sub(began) >= storageMaxDuration(cfg))
		if full {
			f.writeBatch(ctx, s, schema, batch, began)
			batch, size = nil, 0
			continue
		}
		if len(ps) == 0 && !sleep(ctx, exportPoll) {
			return
		}
	}
}

// writeBatch writes a batch's objects and acknowledges their messages,
// and fails the messages it could not write. When the bucket itself
// fails, an object's messages are left in the backlog, not dead-lettered,
// until it is back.
func (f *Front) writeBatch(ctx context.Context, s *pubsubpb.Subscription, schema *topicMsgSchema, batch []pulled, began time.Time) {
	cfg := s.GetCloudStorageConfig()
	objs, bad, err := encodeBatch(s, schema, batch)
	var fs []failed
	if err != nil {
		for _, p := range batch {
			fs = append(fs, failed{p, err.Error()})
		}
		f.fail(ctx, s, fs)
		return
	}
	fs = bad
	for _, o := range objs {
		err := f.exp.upload(ctx, cfg.GetBucket(), objectName(cfg, began), o.contentType, o.body)
		if err != nil {
			f.logf("pubsub front: export %s: %v", s.GetName(), err)
			if st, ok := destinationState(err); ok {
				f.exp.setState(s.GetName(), st)
				var ids []string
				for _, p := range o.msgs {
					ids = append(ids, p.ackID)
				}
				f.extend(ctx, s.GetName(), ids, 0)
				continue
			}
			for _, p := range o.msgs {
				fs = append(fs, failed{p, err.Error()})
			}
			continue
		}
		ids := make([]string, len(o.msgs))
		for i, p := range o.msgs {
			ids[i] = p.ackID
		}
		f.ack(ctx, s.GetName(), ids)
		f.exp.forgetAttempts(s.GetName(), o.msgs)
	}
	f.fail(ctx, s, fs)
}
