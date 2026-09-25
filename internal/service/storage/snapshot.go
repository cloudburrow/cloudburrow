package storage

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// State snapshots (#511) of the builtin server, straight from its store:
// every bucket with all its fields, soft-deleted buckets, every object
// version (live, noncurrent and soft-deleted) with its generation,
// metageneration, holds, retention and customTime, notification
// configurations and undelivered events, IAM policies, sessions, rewrite
// tokens and multipart uploads, and the bytes they reference. It is a tar:
// state.json holds the store's records, and blobs/<id> each blob, streamed.
//
// HMAC key secrets are excluded (state.json says so): a restored key keeps
// its metadata and cannot sign, so a signed URL made with it is refused
// (#509) rather than an archive carrying a credential. On import every
// blob must hash back to its content address, and every object version's
// CRC32C must match its bytes, or nothing is restored.

const (
	statePath       = "/_cloudburrow/state"
	stateFormat     = "cloudburrow-storage-state"
	stateVersion    = 1
	stateRecordFile = "state.json"
)

// stateDoc is state.json.
type stateDoc struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	// SecretsExcluded says HMAC key secrets were left out.
	SecretsExcluded bool                       `json:"secretsExcluded"`
	Records         map[string]json.RawMessage `json:"records"`
}

type blobRef struct {
	id   string
	size int64
}

// referencedBlobs are the blobs the records name, each once.
func referencedBlobs(records map[string]json.RawMessage) ([]blobRef, map[string]uint32, error) {
	seen := map[string]int64{}
	crcs := map[string]uint32{} // an object version's blob → its CRC32C
	addObject := func(o objectRecord) {
		if o.Blob != "" {
			seen[o.Blob] = o.Size
			crcs[o.Blob] = o.CRC32C
		}
	}
	for k, raw := range records {
		switch {
		case strings.HasPrefix(k, objectPrefix):
			var o objectRecord
			if err := json.Unmarshal(raw, &o); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			addObject(o)
		case strings.HasPrefix(k, noncurrentPrefix), strings.HasPrefix(k, softObjectPrefix):
			var vs []objectRecord
			if err := json.Unmarshal(raw, &vs); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			for _, v := range vs {
				addObject(v)
			}
		case strings.HasPrefix(k, sessionPrefix):
			var u uploadSession
			if err := json.Unmarshal(raw, &u); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			for _, c := range u.Chunks {
				seen[c.Blob] = c.Size
			}
		case strings.HasPrefix(k, mpuPrefix):
			var u multipartUpload
			if err := json.Unmarshal(raw, &u); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			for _, p := range u.Parts {
				seen[p.Blob] = p.Size
			}
		case strings.HasPrefix(k, rewritePrefix):
			var st rewriteState
			if err := json.Unmarshal(raw, &st); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			addObject(st.Source)
		}
	}
	out := make([]blobRef, 0, len(seen))
	for id, size := range seen {
		out = append(out, blobRef{id, size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, crcs, nil
}

// ExportState writes the server's state as a tar to w.
func (s *Server) ExportState(w io.Writer) error {
	doc := stateDoc{Format: stateFormat, Version: stateVersion, SecretsExcluded: true, Records: map[string]json.RawMessage{}}
	err := s.meta.View(func(tx Tx) error {
		for _, p := range everyPrefix {
			for _, k := range tx.List(p) {
				if strings.HasSuffix(k, ".seq") {
					continue // below
				}
				raw, _ := tx.Get(k)
				if strings.HasPrefix(k, hmacPrefix) {
					var hk hmacKey
					if err := json.Unmarshal(raw, &hk); err != nil {
						return err
					}
					hk.Secret = ""
					raw, _ = json.Marshal(hk)
				}
				doc.Records[k] = append(json.RawMessage(nil), raw...)
			}
		}
		// The per-bucket notification ID counters sit beside the prefixes.
		for _, k := range tx.List(notificationPrefix) {
			if strings.HasSuffix(k, ".seq") {
				raw, _ := tx.Get(k)
				doc.Records[k], _ = json.Marshal(string(raw))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	blobs, _, err := referencedBlobs(doc.Records)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := writeTarEntry(tw, stateRecordFile, int64(len(body)), strings.NewReader(string(body))); err != nil {
		return err
	}
	for _, b := range blobs {
		f, err := s.blobs.Open(b.id)
		if err != nil {
			return fmt.Errorf("blob %s: %w", b.id, err)
		}
		err = writeTarEntry(tw, "blobs/"+b.id, b.size, f)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func writeTarEntry(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	n, err := io.Copy(tw, r)
	if err == nil && n != size {
		err = fmt.Errorf("%s: %d bytes, declared %d", name, n, size)
	}
	return err
}

// ImportState replaces the server's state with a tar ExportState wrote.
// It verifies everything before it changes anything: a blob that does not
// hash to its ID, or an object version whose bytes do not have its CRC32C,
// fails the import with the state untouched.
func (s *Server) ImportState(r io.Reader) error {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != stateRecordFile {
		return fmt.Errorf("a storage state archive begins with %s", stateRecordFile)
	}
	var doc stateDoc
	if err := json.NewDecoder(io.LimitReader(tr, 1<<30)).Decode(&doc); err != nil {
		return fmt.Errorf("%s: %w", stateRecordFile, err)
	}
	if doc.Format != stateFormat || doc.Version != stateVersion {
		return fmt.Errorf("unknown storage state %s version %d", doc.Format, doc.Version)
	}
	want, crcs, err := referencedBlobs(doc.Records)
	if err != nil {
		return err
	}
	got := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		id, ok := strings.CutPrefix(hdr.Name, "blobs/")
		if !ok {
			return fmt.Errorf("unexpected entry %s", hdr.Name)
		}
		b, err := s.blobs.Write(tr)
		if err != nil {
			return fmt.Errorf("blob %s: %w", id, err)
		}
		if b.ID != id {
			return fmt.Errorf("blob %s: its bytes hash to %s", id, b.ID)
		}
		if crc, isObject := crcs[id]; isObject && crc != b.CRC32C {
			return fmt.Errorf("blob %s: CRC32C %08x, recorded as %08x", id, b.CRC32C, crc)
		}
		got[id] = true
	}
	for _, b := range want {
		if !got[b.id] {
			return fmt.Errorf("the archive is missing blob %s", b.id)
		}
	}
	// The old state goes and the new arrives in one transaction.
	return s.meta.Update(func(tx Tx) error {
		for _, p := range everyPrefix {
			for _, k := range tx.List(p) {
				tx.Delete(k)
			}
		}
		for k, raw := range doc.Records {
			if strings.HasSuffix(k, ".seq") && strings.HasPrefix(k, notificationPrefix) {
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				tx.Put(k, []byte(v))
				continue
			}
			tx.Put(k, raw)
		}
		return nil
	})
}

// serveState is GET /_cloudburrow/state (export) and PUT (import).
func (s *Server) serveState(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/x-tar")
		// A failure after the first byte truncates the tar, which an import
		// refuses as unreadable rather than half-load.
		_ = s.ExportState(w)
	case http.MethodPut:
		if err := s.ImportState(r.Body); err != nil {
			writeError(w, badRequest("The state archive was not restored: %v", err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, errorf(http.StatusMethodNotAllowed, "methodNotAllowed", "GET %s exports the state and PUT restores it", statePath))
	}
}
