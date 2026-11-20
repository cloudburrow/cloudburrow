package logging

// Customer-managed encryption keys for log buckets (#1950), as
// https://docs.cloud.google.com/logging/docs/routing/managed-encryption-storage
// and .../routing/troubleshoot-cmek-orgs describe them:
//
//   - a bucket's cmek_settings.kms_key_name is a CryptoKey of the
//     instance's Cloud KMS (internal/cmek), set on CreateBucket or an
//     UpdateBucket naming cmek_settings; "the log bucket's location must
//     match the key's location" (INVALID_ARGUMENT, "The KMS key location
//     must match the storage location. Received KMS key location: ...,
//     storage location: ..."), and CMEK "can't be enabled for buckets in
//     the global region";
//   - the key must be usable and the project's Logging service account
//     (cmek_settings.service_account_id, GetCmekSettings'
//     service_account_id) needs roles/cloudkms.cryptoKeyEncrypterDecrypter
//     on it: with `up --iam-enforcement=on` the host's evaluator checks it,
//     and a refusal is FAILED_PRECONDITION "service account ... must have
//     both encrypt and decrypt access to the CMEK KMS key";
//   - the bucket keeps the key version it was created (or switched) with,
//     cmek_settings.kms_key_version_name: "rotating a key doesn't affect
//     existing buckets";
//   - while the key cannot be used, "all logs are unavailable for query":
//     ListLogEntries, TailLogEntries and CopyLogEntries over the bucket are
//     FAILED_PRECONDITION ("Cryptographic key is disabled", "Cryptographic
//     key was destroyed", the notifications' wording), and writes keep
//     landing.
//
// What the bucket holds is really encrypted: each entry routed to it is
// kept, for the bucket, as its wire form sealed with a fresh data key that
// the bucket's key wrapped (cmek.Keys.Seal, the bucket's name as additional
// data), and a read of the bucket opens each through Cloud KMS. An entry
// only such buckets hold is not kept in the clear at all (atRest, #1972). The bucket
// also keeps a data key wrapped when its key was set, which every read
// opens first, so a disabled version refuses the read whatever the entries.
// An entry written while the key cannot encrypt is kept unsealed until it
// can (Google "buffers ... the most recent three hours"), and is unreadable
// meanwhile like the rest.
//
// Choices Google does not document, pinned by unit tests (UNVERIFIED):
// removing a bucket's key is FAILED_PRECONDITION; a key that does not exist
// or is not ENABLED at create or update is FAILED_PRECONDITION naming it;
// an instance without Cloud KMS is FAILED_PRECONDITION. A parent's default
// key (Settings.kms_key_name, UpdateCmekSettings) is settingscmek.go's
// (#1972).
// Docs re-read 2026-11-19, still unanswered:
// https://docs.cloud.google.com/logging/docs/routing/managed-encryption-storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// UseKeys gives log buckets customer-managed keys, through the instance's
// Cloud KMS.
func (a *Admin) UseKeys(k cmek.Keys) { a.keys.Store(&k) }

// cmekKeys is the keys UseKeys gave, or none.
func (a *Admin) cmekKeys() cmek.Keys {
	if k := a.keys.Load(); k != nil {
		return *k
	}
	return cmek.Keys{}
}

// bucketKeyKey is where a bucket's wrapped probe key is kept.
func bucketKeyKey(ref bucketRef) string {
	return "logging/bucketcmek/" + strings.TrimPrefix(ref.key(), "logging/buckets/")
}

// cmekPaths are the update_mask paths that set a bucket's key.
var cmekPaths = map[string]bool{"cmek_settings": true, "cmek_settings.kms_key_name": true, "cmekSettings": true, "cmekSettings.kmsKeyName": true}

// keyError is Logging's answer for a key that cannot be used.
func keyError(agent string, err error) error {
	e := cmek.As(err)
	if e == nil {
		return apierror.Internal(err, "Cloud KMS")
	}
	switch e.Kind {
	case cmek.Invalid:
		return apierror.InvalidArgument("cmek_settings.kms_key_name %s: %s", e.Key, e.Msg)
	case cmek.PermissionDenied:
		key := e.Key
		if k := cmek.KeyOf(key); k != "" {
			key = k
		}
		return apierror.FailedPrecondition("service account %s must have both encrypt and decrypt access to the CMEK KMS key %s", agent, key)
	case cmek.Unavailable:
		return status.Errorf(codes.Unavailable, "Cloud KMS did not answer for %s: %s", e.Key, e.Msg)
	case cmek.Unusable:
		switch {
		case strings.Contains(e.Msg, "DESTROY"):
			return apierror.FailedPrecondition("Cryptographic key was destroyed: %s (%s)", e.Key, e.Msg)
		case strings.Contains(e.Msg, "DISABLED"):
			return apierror.FailedPrecondition("Cryptographic key is disabled: %s (%s)", e.Key, e.Msg)
		}
	}
	return apierror.FailedPrecondition("The CMEK KMS key %s cannot be used: %s", e.Key, e.Msg)
}

// setBucketKey gives b (at ref) key: checked, its probe wrapped and kept,
// and b's cmek_settings filled. The caller holds a.mu.
func (a *Admin) setBucketKey(ctx context.Context, ref bucketRef, b *loggingpb.LogBucket, key string) error {
	if key == "" {
		return apierror.InvalidArgument("cmek_settings.kms_key_name is required")
	}
	if ref.location == "global" {
		return apierror.InvalidArgument("CMEK can't be enabled for log buckets in the global location: create the bucket in a region")
	}
	loc, ok := cmek.Location(key)
	if !ok {
		return keyError("", cmek.CheckName("cmek_settings.kms_key_name", key, ""))
	}
	if loc != ref.location {
		return apierror.InvalidArgument("The KMS key location must match the storage location. Received KMS key location: %s, storage location: %s", loc, ref.location)
	}
	agent := serviceAccount(ref.parent)
	if err := a.cmekKeys().Usable(ctx, ref.parent.id, agent, key); err != nil {
		return keyError(agent, err)
	}
	probe, err := a.cmekKeys().Seal(ctx, ref.parent.id, agent, key, nil, []byte(ref.name()))
	if err != nil {
		return keyError(agent, err)
	}
	raw, err := json.Marshal(probe)
	if err != nil {
		return apierror.Internal(err, "encoding the bucket's key")
	}
	if err := a.db.Put(bucketKeyKey(ref), raw); err != nil {
		return apierror.Internal(err, "writing the bucket's key")
	}
	b.CmekSettings = &loggingpb.CmekSettings{KmsKeyName: key, KmsKeyVersionName: probe.KeyVersion, ServiceAccountId: agent}
	return nil
}

// bucketProbe is ref's wrapped probe key, and false for a bucket without
// a customer-managed key. It takes no lock (the store is safe).
func (a *Admin) bucketProbe(ref bucketRef) (cmek.Sealed, bool) {
	raw, err := a.db.Get(bucketKeyKey(ref))
	if err != nil {
		return cmek.Sealed{}, false
	}
	var p cmek.Sealed
	if json.Unmarshal(raw, &p) != nil || p.KeyVersion == "" {
		return cmek.Sealed{}, false
	}
	return p, true
}

// bucketKey is the key of the bucket named name ("" for none), for a
// write's sealing.
func (a *Admin) bucketKey(name string) string {
	ref, err := parseBucket("bucket", name)
	if err != nil {
		return ""
	}
	raw, err := a.db.Get(ref.key())
	if err != nil {
		return ""
	}
	b := &loggingpb.LogBucket{}
	if protojson.Unmarshal(raw, b) != nil {
		return ""
	}
	return b.GetCmekSettings().GetKmsKeyName()
}

// readable checks ref's key opens its probe: nil for a bucket without one.
func (a *Admin) readable(ctx context.Context, ref bucketRef) error {
	if a == nil {
		return nil
	}
	probe, ok := a.bucketProbe(ref)
	keys := a.cmekKeys()
	if !ok {
		return nil
	}
	agent := serviceAccount(ref.parent)
	if _, err := keys.Open(ctx, ref.parent.id, agent, probe, []byte(ref.name())); err != nil {
		return keyError(agent, err)
	}
	return nil
}

// sealRoutes seals, for each customer-managed bucket an entry is routed
// to, its copy there. An entry the key cannot seal now stays unsealed.
func (s *Server) sealRoutes(ctx context.Context, routes map[*loggingpb.LogEntry]*route) {
	if s.admin == nil {
		return
	}
	keys := map[string]string{}
	for e, r := range routes {
		for _, name := range r.buckets {
			key, ok := keys[name]
			if !ok {
				key = s.admin.bucketKey(name)
				keys[name] = key
			}
			if key == "" {
				continue
			}
			if sealed, ok := s.seal(ctx, e, name, key); ok {
				if r.sealed == nil {
					r.sealed = map[string]cmek.Sealed{}
				}
				r.sealed[name] = sealed
			}
		}
	}
}

// seal is e's copy in the bucket name sealed with key, and false when the
// key cannot seal it now.
func (s *Server) seal(ctx context.Context, e *loggingpb.LogEntry, name, key string) (cmek.Sealed, bool) {
	raw, err := proto.Marshal(e)
	if err != nil {
		return cmek.Sealed{}, false
	}
	ref, err := parseBucket("bucket", name)
	if err != nil {
		return cmek.Sealed{}, false
	}
	keys := s.admin.cmekKeys()
	sealed, err := keys.Seal(ctx, ref.parent.id, serviceAccount(ref.parent), key, raw, []byte(name))
	return sealed, err == nil
}

// readBucket is what ref holds, as a read sees it: for a bucket with a
// customer-managed key, each entry opened from its sealed copy, and a
// refusal while the key cannot be used.
func (s *Server) readBucket(ctx context.Context, ref bucketRef) ([]*loggingpb.LogEntry, error) {
	es := s.store.bucketEntries(ref)
	if s.admin == nil {
		return es, nil
	}
	if err := s.admin.readable(ctx, ref); err != nil {
		return nil, err
	}
	_, ok := s.admin.bucketProbe(ref)
	keys := s.admin.cmekKeys()
	if !ok {
		return es, nil
	}
	name := ref.name()
	agent := serviceAccount(ref.parent)
	out := make([]*loggingpb.LogEntry, 0, len(es))
	for _, e := range es {
		s.store.mu.Lock()
		r := s.store.routed[e]
		var sealed cmek.Sealed
		has := false
		if r != nil {
			sealed, has = r.sealed[name]
		}
		s.store.mu.Unlock()
		if !has && r != nil {
			// Written while the key could not encrypt: sealed now.
			if key := s.admin.bucketKey(name); key != "" {
				if sealed, has = s.seal(ctx, e, name, key); has {
					s.store.mu.Lock()
					if r.sealed == nil {
						r.sealed = map[string]cmek.Sealed{}
					}
					r.sealed[name] = sealed
					s.store.toRest(e)
					s.store.mu.Unlock()
				}
			}
		}
		if !has {
			out = append(out, e)
			continue
		}
		raw, err := keys.Open(ctx, ref.parent.id, agent, sealed, []byte(name))
		if err != nil {
			return nil, keyError(agent, err)
		}
		opened := &loggingpb.LogEntry{}
		if err := proto.Unmarshal(raw, opened); err != nil {
			return nil, apierror.Internal(err, "an entry of %s", name)
		}
		out = append(out, opened)
	}
	return out, nil
}

// At rest (#1972): an entry that only customer-managed buckets hold (an
// exclusion or an intercepting sink kept it out of its own _Default, and
// every bucket it was routed to sealed its copy) is not kept in the clear:
// the store holds a stub of it, with what the store indexes it by (log
// name, timestamps, insert ID), and its sealed copies. A read of one of its
// buckets opens the copy through Cloud KMS, so while the key cannot unwrap
// the data key nothing of the entry's payload can be read. An entry that is
// also in its own _Default bucket, which is Google-managed, is kept as
// written there.

// stub is what the store keeps of a sealed-only entry.
func stub(e *loggingpb.LogEntry) *loggingpb.LogEntry {
	return &loggingpb.LogEntry{LogName: e.GetLogName(), Timestamp: e.GetTimestamp(), ReceiveTimestamp: e.GetReceiveTimestamp(),
		InsertId: e.GetInsertId()}
}

// sealedOnly is whether r keeps its entry only in sealed copies.
func sealedOnly(r *route) bool {
	if r == nil || !r.hidden || len(r.buckets) == 0 {
		return false
	}
	for _, b := range r.buckets {
		if _, ok := r.sealed[b]; !ok {
			return false
		}
	}
	return true
}

// atRest is what the store keeps of es: each sealed-only entry as its stub,
// with its route under the stub.
func atRest(es []*loggingpb.LogEntry, routes map[*loggingpb.LogEntry]*route) ([]*loggingpb.LogEntry, map[*loggingpb.LogEntry]*route) {
	var out []*loggingpb.LogEntry
	var outRoutes map[*loggingpb.LogEntry]*route
	for i, e := range es {
		r := routes[e]
		if !sealedOnly(r) {
			if out != nil {
				out = append(out, e)
				if r != nil {
					outRoutes[e] = r
				}
			}
			continue
		}
		if out == nil {
			out = append(make([]*loggingpb.LogEntry, 0, len(es)), es[:i]...)
			outRoutes = make(map[*loggingpb.LogEntry]*route, len(routes))
			for _, p := range es[:i] {
				if r := routes[p]; r != nil {
					outRoutes[p] = r
				}
			}
		}
		st := stub(e)
		out = append(out, st)
		outRoutes[st] = r
	}
	if out == nil {
		return es, routes
	}
	return out, outRoutes
}

// toRest replaces e in the store by its stub once its route is sealed-only
// (an entry written while the key could not encrypt, sealed on a later
// read). s.mu is held.
func (s *Store) toRest(e *loggingpb.LogEntry) {
	r := s.routed[e]
	if !sealedOnly(r) {
		return
	}
	for i, x := range s.entries {
		if x == e {
			st := stub(e)
			s.entries[i] = st
			delete(s.routed, e)
			s.routed[st] = r
			return
		}
	}
}

// readableSources checks each source's bucket can be read.
func (s *Server) readableSources(ctx context.Context, sources []source) error {
	for _, src := range sources {
		if err := s.admin.readable(ctx, src.bucket); err != nil {
			return err
		}
	}
	return nil
}

// dropBucketKey forgets ref's key, with the bucket.
func (a *Admin) dropBucketKey(ref bucketRef) {
	if err := a.db.Delete(bucketKeyKey(ref)); err != nil && !errors.Is(err, store.ErrNotFound) {
		return
	}
}
