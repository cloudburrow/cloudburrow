package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// earlierStorage migrates the Cloud Storage a release from before the
// builtin server (#519) left on a persistent instance (#1606): buckets on
// the storage-data volume in fake-gcs-server's layout, which the builtin
// server cannot read and refuses to start over (#780), and notification
// configurations in <instance>/storage-notify.
//
// Before the backends are applied, while the earlier server still runs on
// the volume, it reads every bucket and object through that server's JSON
// API into a spool in the instance directory, in the layout of that
// release's state archive, and moves the earlier server's files aside on
// the volume, into earlier-storage-519/. Once the builtin server is ready it
// loads the spool through the archive loader (#1605), then removes it. A
// spool that is complete survives an interrupted `up`, and the next one
// loads it.
type earlierStorage struct {
	kubeconfig string
	namespace  string
	instance   string // the instance directory
	project    string
	out        io.Writer
}

const (
	// earlierStorageService is the Deployment and Service both servers run
	// as.
	earlierStorageService = "storage"
	earlierStorageImage   = "fake-gcs-server"
	earlierStorageAside   = "earlier-storage-519"
	earlierSpoolDir       = "storage-before-519"
	earlierSpoolDone      = "complete"
)

func (e *earlierStorage) spool() string { return filepath.Join(e.instance, earlierSpoolDir) }

func (e *earlierStorage) logf(format string, args ...any) {
	if e.out != nil {
		fmt.Fprintf(e.out, format, args...)
	}
}

// Export spools the earlier server's buckets, and reports whether there is
// a spool to load.
func (e *earlierStorage) Export(ctx context.Context) (bool, error) {
	if _, err := os.Stat(filepath.Join(e.spool(), earlierSpoolDone)); err == nil {
		return true, nil
	}
	kube := k8s.New(e.kubeconfig, "", e.namespace)
	image, err := kube.Get(ctx, "deployment", earlierStorageService,
		"jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil || !strings.Contains(image, earlierStorageImage) {
		return false, nil // none, or the builtin server's own
	}
	e.logf("  storage: migrating the buckets of the server an earlier release ran (#519, #1606)...\n")
	if _, err := kube.Do(ctx, "", "rollout", "status", "deployment/"+earlierStorageService,
		"--watch=true", "--timeout=300s"); err != nil {
		return false, fmt.Errorf("the earlier Cloud Storage server did not start, so its buckets cannot be migrated: %w", err)
	}
	fwd := netfwd.New(netfwd.Target{Name: earlierStorageService, Namespace: e.namespace,
		ServicePort: components.StoragePort}, e.kubeconfig, "127.0.0.1")
	if err := fwd.Start(ctx); err != nil {
		return false, fmt.Errorf("reach the earlier Cloud Storage server: %w", err)
	}
	defer func() { _ = fwd.Stop(context.Background()) }()

	tmp := e.spool() + ".partial"
	_ = os.RemoveAll(tmp) // what an interrupted export of ours left
	if err := os.MkdirAll(filepath.Join(tmp, "objects"), 0o700); err != nil {
		return false, err
	}
	n, err := e.download(ctx, "http://"+fwd.HostAddr(), tmp)
	if err != nil {
		return false, fmt.Errorf("read the earlier Cloud Storage server's buckets: %w", err)
	}
	if err := e.spoolNotifications(tmp); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(tmp, earlierSpoolDone), []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, e.spool()); err != nil {
		return false, err
	}
	// The earlier server's files go aside on the volume, so the builtin
	// server starts; they stay there until `cloudburrow delete`.
	script := `set -e; cd /data; mkdir -p ` + earlierStorageAside + `; for f in * .[!.]*; do ` +
		`case "$f" in ` + earlierStorageAside + `|meta|objects|'*'|'.[!.]*') ;; *) mv -- "$f" ` + earlierStorageAside + `/ ;; esac; done`
	if _, err := kube.Do(ctx, "", "exec", "deployment/"+earlierStorageService, "--", "sh", "-c", script); err != nil {
		return false, fmt.Errorf("move the earlier Cloud Storage server's files aside: %w", err)
	}
	e.logf("  storage: %d object(s) read from the earlier server; its files are in %s on the volume\n", n, earlierStorageAside)
	return true, nil
}

// download writes every bucket and object into dir, as an archive of the
// earlier releases holds them: buckets.json, objects.json, objects/N.bin.
func (e *earlierStorage) download(ctx context.Context, base, dir string) (int, error) {
	c, err := storage.NewClient(ctx, option.WithEndpoint(base+"/storage/v1/"), option.WithoutAuthentication(),
		// The JSON media path, which the earlier server serves at any Host.
		storage.WithJSONReads())
	if err != nil {
		return 0, err
	}
	defer c.Close()
	var buckets []legacyBucket
	var objects []legacyObject
	it := c.Buckets(ctx, e.project)
	for {
		b, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("list buckets: %w", err)
		}
		buckets = append(buckets, legacyBucket{b.Name, b.Location, b.StorageClass, b.Labels})
		oit := c.Bucket(b.Name).Objects(ctx, nil)
		for {
			o, err := oit.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				return 0, fmt.Errorf("list gs://%s: %w", b.Name, err)
			}
			file := fmt.Sprintf("objects/%06d.bin", len(objects))
			if err := e.save(ctx, c, b.Name, o.Name, filepath.Join(dir, file)); err != nil {
				return 0, err
			}
			objects = append(objects, legacyObject{b.Name, o.Name, file, o.Size, o.ContentType,
				o.ContentEncoding, o.CacheControl, o.Metadata, o.CRC32C})
		}
	}
	for name, v := range map[string]any{legacyBucketsEntry: buckets, legacyObjectsEntry: objects} {
		b, err := json.Marshal(v)
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return 0, err
		}
	}
	return len(objects), nil
}

func (e *earlierStorage) save(ctx context.Context, c *storage.Client, bucket, name, path string) error {
	r, err := c.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("read gs://%s/%s: %w", bucket, name, err)
	}
	defer r.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("read gs://%s/%s: %w", bucket, name, err)
	}
	return f.Close()
}

// spoolNotifications copies the notification configurations the earlier
// releases kept in <instance>/storage-notify.
func (e *earlierStorage) spoolNotifications(dir string) error {
	src := filepath.Join(e.instance, "storage-notify")
	if _, err := os.Stat(filepath.Join(src, "metadata.json")); err != nil {
		return nil
	}
	st, err := store.OpenDurable(src)
	if err != nil {
		return fmt.Errorf("open the earlier notification configurations: %w", err)
	}
	defer st.Close()
	keys, err := st.List("notify/")
	if err != nil {
		return err
	}
	records := make([]kvRecord, 0, len(keys))
	for _, k := range keys {
		v, err := st.Get(k)
		if err != nil {
			return err
		}
		records = append(records, kvRecord{Key: k, Value: v})
	}
	b, err := json.Marshal(records)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "notifications"), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, legacyNotifyEntry), b, 0o600)
}

// Import loads the spool into the builtin server, which is ready.
func (e *earlierStorage) Import(ctx context.Context) error {
	fwd := netfwd.New(netfwd.Target{Name: earlierStorageService, Namespace: e.namespace,
		ServicePort: components.StoragePort}, e.kubeconfig, "127.0.0.1")
	if err := fwd.Start(ctx); err != nil {
		return fmt.Errorf("reach the Cloud Storage server: %w", err)
	}
	defer func() { _ = fwd.Stop(context.Background()) }()
	s := &builtinStorageSnapshotter{tunnel: fwd, project: e.project}
	if err := s.importLegacy(ctx, dirEntries(e.spool())); err != nil {
		return fmt.Errorf("load the earlier Cloud Storage server's buckets (kept in %s): %w", e.spool(), err)
	}
	e.logf("  storage: the earlier server's buckets are loaded\n")
	return os.RemoveAll(e.spool())
}
