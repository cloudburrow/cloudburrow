package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// kvSnapshotter captures a service record for record from the store it keeps
// its state in (#289).
//
// Record for record rather than through the service's API, because the
// store is the state: a queue's retry configuration, a task's dispatch
// count, a disabled secret version's state all come back exactly, with no
// mapping to fall out of step. The store is whichever backend the instance
// uses (memory, a durable file, or Kubernetes Secrets for Secret Manager),
// so the round trip works in every mode.
type kvSnapshotter struct {
	name   string
	secret bool
	db     func() store.Store
}

type kvRecord struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
}

const recordsEntry = "records.json"

func (k *kvSnapshotter) Name() string { return k.name }
func (k *kvSnapshotter) Secret() bool { return k.secret }

func (k *kvSnapshotter) store() (store.Store, error) {
	st := k.db()
	if st == nil {
		return nil, errors.New(k.name + " has not started")
	}
	return st, nil
}

func (k *kvSnapshotter) Export(_ context.Context, w admin.EntryWriter) error {
	st, err := k.store()
	if err != nil {
		return err
	}
	keys, err := st.List("")
	if err != nil {
		return err
	}
	records := make([]kvRecord, 0, len(keys))
	for _, key := range keys {
		v, err := st.Get(key)
		if err != nil {
			return err
		}
		records = append(records, kvRecord{key, v})
	}
	b, err := json.MarshalIndent(records, "", " ")
	if err != nil {
		return err
	}
	return w.Add(recordsEntry, int64(len(b)), bytes.NewReader(b))
}

// Import replaces the store's contents with the archive's in one atomic
// commit: every current key deleted, every archived one written. Nothing
// created since the snapshot survives, and a failed commit leaves the store
// as it was.
func (k *kvSnapshotter) Import(_ context.Context, r admin.EntryReader) error {
	st, err := k.store()
	if err != nil {
		return err
	}
	f, err := r.Open(recordsEntry)
	if err != nil {
		return err
	}
	defer f.Close()
	var records []kvRecord
	if err := json.NewDecoder(io.LimitReader(f, 1<<30)).Decode(&records); err != nil {
		return err
	}
	current, err := st.List("")
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	var ops []store.Op
	for _, rec := range records {
		keep[rec.Key] = true
		ops = append(ops, store.Op{Kind: store.OpPut, Key: rec.Key, Value: rec.Value})
	}
	for _, key := range current {
		if !keep[key] {
			ops = append(ops, store.Op{Kind: store.OpDelete, Key: key})
		}
	}
	if len(ops) == 0 {
		return nil
	}
	return st.Commit(ops)
}

// notCapturedReasons says why a service is left out of snapshots.
func notCapturedReasons(s config.Service) string {
	switch s {
	case config.ServicePubSub:
		return "Google's Pub/Sub emulator has no export, and keeps nothing across a restart either"
	case config.ServiceRun:
		return "services are Knative objects in the cluster; redeploy them from their images"
	case config.ServiceCloudSQL:
		return "a real PostgreSQL: use pg_dump, which captures what a snapshot here could only approximate"
	case config.ServiceCloudSQLMySQL:
		return "a real MySQL: use mysqldump, which captures what a snapshot here could only approximate"
	case config.ServiceMemorystore:
		return "a real Valkey: use its own BGSAVE or the append-only file on its volume"
	case config.ServiceFirestore, config.ServiceDatastore, config.ServiceBigtable, config.ServiceSpanner, config.ServiceBigQuery:
		return "an in-memory emulator with no export"
	default:
		return "not captured"
	}
}
