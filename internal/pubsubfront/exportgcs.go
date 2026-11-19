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
		return apierror.InvalidArgument("avro_config.use_topic_schema is not supported by CloudBurrow yet (#1874).")
	}
	f.exp.init()
	err := f.exp.bucket(ctx, cfg.GetBucket())
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

// bucket checks a bucket exists.
func (e *exporter) bucket(ctx context.Context, name string) error {
	if e.storage == "" {
		return &errDestination{int32(pubsubpb.CloudStorageConfig_PERMISSION_DENIED),
			"Cannot write to the Cloud Storage bucket: storage.googleapis.com is not enabled for the project."}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.storage+"/storage/v1/b/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &errDestination{int32(pubsubpb.CloudStorageConfig_NOT_FOUND), "Cannot write to the Cloud Storage bucket " + name + ": it does not exist."}
	case resp.StatusCode == http.StatusForbidden:
		return &errDestination{int32(pubsubpb.CloudStorageConfig_PERMISSION_DENIED), "Cannot write to the Cloud Storage bucket " + name + ": permission denied."}
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("Cloud Storage buckets.get %s: %s", name, resp.Status)
	}
	return nil
}

// upload writes an object.
func (e *exporter) upload(ctx context.Context, bucket, name, contentType string, body []byte) error {
	u := e.storage + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := e.client.Do(req)
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

// encodeBatch is a batch's object and its content type.
func encodeBatch(s *pubsubpb.Subscription, ps []pulled) ([]byte, string, error) {
	cfg := s.GetCloudStorageConfig()
	if cfg.GetAvroConfig() == nil {
		parts := make([][]byte, len(ps))
		for i, p := range ps {
			parts[i] = p.msg.GetData()
		}
		return bytes.Join(parts, []byte("\n")), "text/plain", nil
	}
	meta := cfg.GetAvroConfig().GetWriteMetadata()
	schema := avroMessageSchema
	if meta {
		schema = avroMetadataSchema
	}
	var buf bytes.Buffer
	w, err := goavro.NewOCFWriter(goavro.OCFConfig{W: &buf, Schema: schema})
	if err != nil {
		return nil, "", err
	}
	recs := make([]any, 0, len(ps))
	for _, p := range ps {
		r := map[string]any{"data": p.msg.GetData()}
		if meta {
			attrs := map[string]any{}
			for k, v := range p.msg.GetAttributes() {
				attrs[k] = v
			}
			r["subscription_name"] = s.GetName()
			r["message_id"] = p.msg.GetMessageId()
			r["publish_time"] = p.msg.GetPublishTime().AsTime().UTC()
			r["attributes"] = attrs
		}
		recs = append(recs, r)
	}
	if err := w.Append(recs); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "application/octet-stream", nil
}

// runStorageExport delivers s until ctx ends.
func (f *Front) runStorageExport(ctx context.Context, s *pubsubpb.Subscription) {
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
		if err := f.exp.bucket(ctx, cfg.GetBucket()); err != nil {
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
			f.writeBatch(ctx, s, batch, began)
			batch, size = nil, 0
			continue
		}
		if len(ps) == 0 && !sleep(ctx, exportPoll) {
			return
		}
	}
}

// writeBatch writes one object and acknowledges its messages, or fails
// them.
func (f *Front) writeBatch(ctx context.Context, s *pubsubpb.Subscription, batch []pulled, began time.Time) {
	cfg := s.GetCloudStorageConfig()
	body, ct, err := encodeBatch(s, batch)
	if err == nil {
		err = f.exp.upload(ctx, cfg.GetBucket(), objectName(cfg, began), ct, body)
	}
	if err != nil {
		f.logf("pubsub front: export %s: %v", s.GetName(), err)
		if st, ok := destinationState(err); ok {
			f.exp.setState(s.GetName(), st)
		}
		fs := make([]failed, len(batch))
		for i, p := range batch {
			fs[i] = failed{p, err.Error()}
		}
		f.fail(ctx, s, fs)
		return
	}
	ids := make([]string, len(batch))
	for i, p := range batch {
		ids[i] = p.ackID
	}
	f.ack(ctx, s.GetName(), ids)
	f.exp.forgetAttempts(s.GetName(), batch)
}
