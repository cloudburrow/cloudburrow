package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// builtinStorageSnapshotter captures the builtin Cloud Storage server (#511)
// through its state endpoint, which reads and writes the store directly:
// every object version with its generation, holds and retention, bucket
// fields, notification configurations and IAM policies, and the bytes. HMAC
// key secrets are left out by the server, so the archive holds no secret.
type builtinStorageSnapshotter struct {
	tunnel *netfwd.Forwarder
}

func (s *builtinStorageSnapshotter) Name() string { return "storage" }

// Secret is false: the server excludes HMAC key secrets from its state.
func (s *builtinStorageSnapshotter) Secret() bool { return false }

func (s *builtinStorageSnapshotter) url() (string, error) {
	if s.tunnel == nil || s.tunnel.HostAddr() == "" {
		return "", errors.New("the storage tunnel is not running")
	}
	return "http://" + s.tunnel.HostAddr() + "/_cloudburrow/state", nil
}

func (s *builtinStorageSnapshotter) Export(ctx context.Context, w admin.EntryWriter) error {
	u, err := s.url()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("export the builtin storage state: %s", resp.Status)
	}
	tr := tar.NewReader(resp.Body)
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the builtin storage state: %w", err)
		}
		if err := w.Add(hdr.Name, hdr.Size, tr); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("the builtin storage state was empty")
	}
	return nil
}

func (s *builtinStorageSnapshotter) Import(ctx context.Context, r admin.EntryReader) error {
	u, err := s.url()
	if err != nil {
		return err
	}
	// state.json first, then the blobs, as the server reads them.
	names := r.List("")
	sort.SliceStable(names, func(i, j int) bool { return names[i] == "state.json" && names[j] != "state.json" })
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, name := range names {
			if err := copyEntry(tw, r, name); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		_ = pw.CloseWithError(tw.Close())
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, pr)
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
		return fmt.Errorf("restore the builtin storage state: %s %s", resp.Status, msg)
	}
	return nil
}

// copyEntry writes one archive entry into tw. Its size is found by spooling
// it to a temporary file, since an entry reader does not report it.
func copyEntry(tw *tar.Writer, r admin.EntryReader, name string) error {
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp, err := os.CreateTemp("", "cb-storage-entry-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, f)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, tmp)
	return err
}
