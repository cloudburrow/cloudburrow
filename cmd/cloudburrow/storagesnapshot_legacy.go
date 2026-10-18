package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/admin"
)

// An archive from before the builtin storage server (#519, v0.1.0 and the
// releases around it) holds Cloud Storage as buckets.json, objects.json, a
// file of bytes for each object and the notification configurations under
// notifications/, written through the official client (#290). A load of
// one (#1605) empties the builtin server, then creates each bucket, object
// and notification configuration through its JSON API, as that release
// restored them, and fails if an object comes back with another CRC32C.
const (
	legacyBucketsEntry = "buckets.json"
	legacyObjectsEntry = "objects.json"
	legacyNotifyEntry  = "notifications/records.json"
)

type legacyBucket struct {
	Name         string            `json:"name"`
	Location     string            `json:"location,omitempty"`
	StorageClass string            `json:"storageClass,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

type legacyObject struct {
	Bucket          string            `json:"bucket"`
	Name            string            `json:"name"`
	File            string            `json:"file"`
	Size            int64             `json:"size"`
	ContentType     string            `json:"contentType,omitempty"`
	ContentEncoding string            `json:"contentEncoding,omitempty"`
	CacheControl    string            `json:"cacheControl,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	CRC32C          uint32            `json:"crc32c"`
}

// legacyNotification is the storagenotify configuration record of those
// releases.
type legacyNotification struct {
	ID               string            `json:"id"`
	Bucket           string            `json:"bucket"`
	Topic            string            `json:"topic"`
	EventTypes       []string          `json:"event_types,omitempty"`
	ObjectNamePrefix string            `json:"object_name_prefix,omitempty"`
	CustomAttributes map[string]string `json:"custom_attributes,omitempty"`
	PayloadFormat    string            `json:"payload_format"`
}

func readJSONEntry(r admin.EntryReader, name string, into any) error {
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(into); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (s *builtinStorageSnapshotter) importLegacy(ctx context.Context, r admin.EntryReader) error {
	var buckets []legacyBucket
	var objects []legacyObject
	if err := readJSONEntry(r, legacyBucketsEntry, &buckets); err != nil {
		return err
	}
	if err := readJSONEntry(r, legacyObjectsEntry, &objects); err != nil {
		return err
	}
	var notifications []legacyNotification
	if names := r.List(legacyNotifyEntry); len(names) > 0 {
		var records []kvRecord
		if err := readJSONEntry(r, legacyNotifyEntry, &records); err != nil {
			return err
		}
		for _, rec := range records {
			var n legacyNotification
			if err := json.Unmarshal(rec.Value, &n); err != nil {
				return fmt.Errorf("%s %s: %w", legacyNotifyEntry, rec.Key, err)
			}
			notifications = append(notifications, n)
		}
	}
	// Every object's bytes are there before anything is replaced.
	for _, o := range objects {
		if len(r.List(o.File)) == 0 {
			return fmt.Errorf("the archive is missing %s, the bytes of gs://%s/%s", o.File, o.Bucket, o.Name)
		}
	}
	if err := s.putEmptyState(ctx); err != nil {
		return err
	}
	u, err := s.url()
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(u, "/_cloudburrow/state")
	c, err := storage.NewClient(ctx, option.WithEndpoint(base+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		return err
	}
	defer c.Close()
	for _, b := range buckets {
		// The bucket's project is not in the archive: it is the instance's.
		if err := c.Bucket(b.Name).Create(ctx, s.project, &storage.BucketAttrs{
			Location: b.Location, StorageClass: b.StorageClass, Labels: b.Labels}); err != nil {
			return fmt.Errorf("create bucket %s: %w", b.Name, err)
		}
	}
	for _, o := range objects {
		f, err := r.Open(o.File)
		if err != nil {
			return err
		}
		w := c.Bucket(o.Bucket).Object(o.Name).NewWriter(ctx)
		w.ContentType, w.ContentEncoding, w.CacheControl, w.Metadata = o.ContentType, o.ContentEncoding, o.CacheControl, o.Metadata
		_, err = io.Copy(w, f)
		_ = f.Close()
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("restore gs://%s/%s: %w", o.Bucket, o.Name, err)
		}
		if got := w.Attrs().CRC32C; got != o.CRC32C {
			return fmt.Errorf("gs://%s/%s came back with CRC32C %08x, saved as %08x", o.Bucket, o.Name, got, o.CRC32C)
		}
	}
	for _, n := range notifications {
		project, topic, ok := strings.Cut(strings.TrimPrefix(n.Topic, "projects/"), "/topics/")
		if !ok {
			return fmt.Errorf("notification %s of gs://%s: topic %q", n.ID, n.Bucket, n.Topic)
		}
		format := storage.JSONPayload
		if n.PayloadFormat == "NONE" {
			format = storage.NoPayload
		}
		if _, err := c.Bucket(n.Bucket).AddNotification(ctx, &storage.Notification{
			TopicProjectID: project, TopicID: topic, EventTypes: n.EventTypes, ObjectNamePrefix: n.ObjectNamePrefix,
			CustomAttributes: n.CustomAttributes, PayloadFormat: format}); err != nil {
			return fmt.Errorf("notification %s of gs://%s: %w", n.ID, n.Bucket, err)
		}
	}
	return nil
}

// putEmptyState replaces the builtin server's state with an empty one.
func (s *builtinStorageSnapshotter) putEmptyState(ctx context.Context) error {
	u, err := s.url()
	if err != nil {
		return err
	}
	doc, _ := json.Marshal(map[string]any{"format": "cloudburrow-storage-state", "version": 1, "secretsExcluded": true,
		"records": map[string]any{}})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "state.json", Mode: 0o600, Size: int64(len(doc)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, _ = tw.Write(doc)
	if err := tw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("empty the builtin storage state: %s %s", resp.Status, msg)
	}
	return nil
}
