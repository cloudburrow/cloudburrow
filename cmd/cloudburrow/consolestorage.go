package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Cloud Storage objects in the console (#295): upload, download, preview and
// delete, through the official client against the storage server, the calls
// an application makes. The console's routes do the streaming, the limits and
// the headers; this is only the storage side.

var _ console.ObjectStore = storageProvider{}

func (p storageProvider) storageClient(ctx context.Context) (*storage.Client, error) {
	return storage.NewClient(ctx,
		option.WithEndpoint("http://"+p.endpoint+"/storage/v1/"),
		option.WithoutAuthentication(),
		// The JSON media path, which the backend serves at any Host. Its XML
		// download path matches only the host it advertises to clients.
		storage.WithJSONReads())
}

// objectName joins a path's segments after the bucket into an object name.
func objectName(path []string) (bucket, name string, err error) {
	if len(path) < 2 || path[0] == "" {
		return "", "", errors.New("name a bucket and an object")
	}
	return path[0], strings.Join(path[1:], "/"), nil
}

func (p storageProvider) Upload(ctx context.Context, _ string, prefix []string, filename, contentType string, r io.Reader) (string, error) {
	if len(prefix) == 0 || prefix[0] == "" {
		return "", errors.New("name the bucket to upload into")
	}
	bucket := prefix[0]
	name := filename
	if dir := strings.Trim(strings.Join(prefix[1:], "/"), "/"); dir != "" {
		name = dir + "/" + filename
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()

	// Cancelled, not closed, on failure: closing a writer commits what it
	// has, and a truncated object that looks like the file is worse than no
	// object.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := c.Bucket(bucket).Object(name).NewWriter(wctx)
	w.ContentType = contentType
	// One streamed request. A resumable upload buffers each 16 MiB chunk in
	// memory, which is what a console route that streams exists to avoid.
	w.ChunkSize = 0
	if _, err := io.Copy(w, r); err != nil {
		cancel()
		_ = w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("upload %s: %w", name, err)
	}
	return bucket + "/" + name, nil
}

func (p storageProvider) OpenObject(ctx context.Context, _ string, path []string) (console.ObjectReader, error) {
	bucket, name, err := objectName(path)
	if err != nil {
		return console.ObjectReader{}, err
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return console.ObjectReader{}, err
	}
	rd, err := c.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		_ = c.Close()
		return console.ObjectReader{}, fmt.Errorf("open %s/%s: %w", bucket, name, err)
	}
	return console.ObjectReader{
		ReadCloser:  closeBoth{rd, c},
		Name:        name,
		Size:        rd.Attrs.Size,
		ContentType: rd.Attrs.ContentType,
	}, nil
}

// closeBoth closes the reader and then the client that opened it.
type closeBoth struct {
	*storage.Reader
	c *storage.Client
}

func (cb closeBoth) Close() error {
	err := cb.Reader.Close()
	_ = cb.c.Close()
	return err
}

func (p storageProvider) DeleteObject(ctx context.Context, _ string, path []string) error {
	bucket, name, err := objectName(path)
	if err != nil {
		return err
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.Bucket(bucket).Object(name).Delete(ctx)
}
