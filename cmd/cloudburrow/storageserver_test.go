package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// storage-server binds loopback unless told otherwise (ADR-0004), and up
// refuses the builtin backend until the cut-over rather than silently
// running fake-gcs-server (#488).
func TestStorageServerRefusesARemoteListenAddress(t *testing.T) {
	var out, errb bytes.Buffer
	err := runStorageServer(context.Background(), []string{"--listen", "0.0.0.0:0"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Errorf("a non-loopback --listen = %v; want a refusal naming --allow-remote", err)
	}
}

func TestStorageServerServesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runStorageServer(ctx, []string{"--listen", "127.0.0.1:0"}, &out, &errb) }()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("storage-server after cancel = %v", err)
	}
}

func TestUpRefusesTheBuiltinStorageBackendForNow(t *testing.T) {
	var out, errb bytes.Buffer
	err := runUp(context.Background(), []string{"--state-dir", t.TempDir(), "--storage-backend", "builtin"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "#485") {
		t.Errorf("up --storage-backend builtin = %v; want a refusal pointing at #485", err)
	}
}
