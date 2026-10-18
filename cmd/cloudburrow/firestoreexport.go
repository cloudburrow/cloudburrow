package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

// exportFirestoreDocuments has the Firestore front export what was written
// since its last export, before `stop` stops the cluster (#1615). The
// front's own export as its pod stops is bounded (8 seconds, about 600,000
// documents of 1 KB) and the node's stop more tightly still; this one is
// bounded by the shutdown timeout alone, so a clean stop keeps every write
// that reached Firestore before it, whatever the number of documents.
// addr is the front's host address, from the runtime file.
func exportFirestoreDocuments(addr string, timeout time.Duration, stdout io.Writer) {
	if addr == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	err := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+firestoreadmin.ExportPath, nil)
		if err != nil {
			return err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		var body struct {
			Exported bool   `json:"exported"`
			Error    string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&body)
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d %s", res.StatusCode, body.Error)
		}
		if body.Exported {
			fmt.Fprintf(stdout, "exported the Firestore documents in %s\n", time.Since(start).Round(time.Millisecond))
		}
		return nil
	}()
	if err != nil {
		fmt.Fprintf(stdout, "warning: the Firestore documents were not exported before the stop (%v): the writes since the front's last export, at most 30 seconds' worth, may be lost unless its own export as it stops finishes within 8 seconds\n", err)
	}
}
