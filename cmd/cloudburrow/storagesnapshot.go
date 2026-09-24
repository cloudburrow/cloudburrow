package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// storageSnapshotter captures Cloud Storage (#290): buckets, objects with
// their bytes and metadata, and notification configurations.
//
// It goes through the official Go client against the bare backend tunnel,
// not the notification front, so restoring an object does not announce it
// as a new upload. Object bytes stream: each is read straight into the
// archive, and on load uploaded straight from the extracted file, so a large
// object is never held in memory. A restored object must come back with the
// CRC32C it was saved with, or the load fails rather than restore it
// differently.
type storageSnapshotter struct {
	tunnel *netfwd.Forwarder
	notify *notifyService
	// project is the instance's: the client needs one to list and create
	// buckets, though the backend lists every bucket whatever it is.
	project string
}

type bucketSnap struct {
	Name         string            `json:"name"`
	Location     string            `json:"location,omitempty"`
	StorageClass string            `json:"storageClass,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

type objectSnap struct {
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

func (s *storageSnapshotter) Name() string { return "storage" }
func (s *storageSnapshotter) Secret() bool { return false }

func (s *storageSnapshotter) client(ctx context.Context) (*storage.Client, error) {
	if s.tunnel == nil || s.tunnel.HostAddr() == "" {
		return nil, errors.New("the storage tunnel is not running")
	}
	return storage.NewClient(ctx,
		option.WithEndpoint("http://"+s.tunnel.HostAddr()+"/storage/v1/"),
		option.WithoutAuthentication(),
		// The JSON media path, which the backend serves at any Host. Its XML
		// download path matches only the host it advertises to clients.
		storage.WithJSONReads())
}

func (s *storageSnapshotter) notifyKV() *kvSnapshotter {
	return &kvSnapshotter{name: "storage-notifications", db: func() store.Store {
		if c := s.notify.Configs(); c != nil {
			return c.Backing()
		}
		return nil
	}}
}

type prefixedWriter struct {
	w      admin.EntryWriter
	prefix string
}

func (p prefixedWriter) Add(name string, size int64, r io.Reader) error {
	return p.w.Add(p.prefix+name, size, r)
}

type prefixedReader struct {
	r      admin.EntryReader
	prefix string
}

func (p prefixedReader) Open(name string) (io.ReadCloser, error) { return p.r.Open(p.prefix + name) }
func (p prefixedReader) List(prefix string) []string             { return p.r.List(p.prefix + prefix) }

func (s *storageSnapshotter) Export(ctx context.Context, w admin.EntryWriter) error {
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	var buckets []bucketSnap
	var objects []objectSnap
	it := c.Buckets(ctx, s.project)
	for {
		b, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("list buckets: %w", err)
		}
		buckets = append(buckets, bucketSnap{b.Name, b.Location, b.StorageClass, b.Labels})
		oit := c.Bucket(b.Name).Objects(ctx, nil)
		for {
			o, err := oit.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return fmt.Errorf("list gs://%s: %w", b.Name, err)
			}
			file := fmt.Sprintf("objects/%06d.bin", len(objects))
			r, err := c.Bucket(b.Name).Object(o.Name).NewReader(ctx)
			if err != nil {
				return fmt.Errorf("read gs://%s/%s: %w", b.Name, o.Name, err)
			}
			err = w.Add(file, o.Size, r)
			_ = r.Close()
			if err != nil {
				return fmt.Errorf("save gs://%s/%s: %w", b.Name, o.Name, err)
			}
			objects = append(objects, objectSnap{b.Name, o.Name, file, o.Size, o.ContentType,
				o.ContentEncoding, o.CacheControl, o.Metadata, o.CRC32C})
		}
	}
	for name, v := range map[string]any{"buckets.json": buckets, "objects.json": objects} {
		b, _ := json.MarshalIndent(v, "", " ")
		if err := w.Add(name, int64(len(b)), jsonReader(b)); err != nil {
			return err
		}
	}
	return s.notifyKV().Export(ctx, prefixedWriter{w, "notifications/"})
}

func (s *storageSnapshotter) Import(ctx context.Context, r admin.EntryReader) error {
	var buckets []bucketSnap
	var objects []objectSnap
	for name, into := range map[string]any{"buckets.json": &buckets, "objects.json": &objects} {
		f, err := r.Open(name)
		if err != nil {
			return err
		}
		err = json.NewDecoder(f).Decode(into)
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	// Everything that is there now goes first, notification configurations
	// included, so nothing survives from before the load and no restored
	// object is routed anywhere as it is written.
	if err := (&storageResetter{tunnel: s.tunnel, notify: s.notify}).Reset(ctx); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	for _, b := range buckets {
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
		obj := c.Bucket(o.Bucket).Object(o.Name)
		w := obj.NewWriter(ctx)
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
	return s.notifyKV().Import(ctx, prefixedReader{r, "notifications/"})
}

func jsonReader(b []byte) io.Reader { return bytes.NewReader(b) }
