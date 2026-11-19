package kms

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Custom constraints (#1895) on Cloud KMS's AutokeyConfig, CryptoKey,
// CryptoKeyVersion, EkmConfig, EkmConnection, ImportJob and KeyHandle,
// CREATE and UPDATE, over gRPC and REST (which calls the same methods):
// the condition reads the resource as the request would leave it, by its
// REST field names and enum names (resource.protectionLevel == 'HSM',
// resource.rotationPeriod <= duration('7776000s'), the page's examples),
// limited to the fields the page lists ("Create custom organization
// policy constraints for Cloud KMS", read 2026-11-19). A violation is
// FAILED_PRECONDITION "Operation denied by custom org policies: [...]",
// the wording every custom constraint page shows.

// constraintParent is the project or folder a KMS resource name is under.
func constraintParent(name string) string {
	if p := orgpolicy.ProjectOf(name); p != "" {
		return p
	}
	if rest, ok := strings.CutPrefix(name, "folders/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		if id != "" {
			return "folders/" + id
		}
	}
	return ""
}

// customResource is m's REST JSON with its unset scalars and enums at
// their defaults, as a condition reads a proto3 resource
// (resource.importOnly is false on a key that did not set it); unset
// messages stay absent.
func customResource(m proto.Message) map[string]any {
	raw, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	dropNulls(out)
	return out
}

func dropNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if x == nil {
				delete(t, k)
				continue
			}
			dropNulls(x)
		}
	case []any:
		for _, x := range t {
			dropNulls(x)
		}
	}
}

// checkCustom refuses method leaving m, a resource of resourceType named
// name, when a custom constraint enforced on its project or folder is
// violated.
func checkCustom(name, resourceType, method string, m proto.Message) error {
	under := constraintParent(name)
	if under == "" || !orgpolicy.SourceSet() {
		return nil
	}
	err := orgpolicy.CheckCustom(context.Background(), under, resourceType, method, customResource(m))
	var v *orgpolicy.CustomViolation
	if errors.As(err, &v) {
		return apierror.Wrap(apierror.FailedPrecondition("%s", v.Error()))
	}
	return nil
}

// errChanged reports that a record a request read changed while s.mu was
// released for its custom-constraint check (#1902); the request is then
// run again from the start (retryChanged).
var errChanged = errors.New("kms: a record changed during the custom constraint check")

// checkUnlocked runs checkCustom with s.mu released (#1902): the caller
// holds s.mu, and holds it again on return. The check may call the host's
// evaluator, which is slow and may call back into this service, so it
// never runs under the lock. keys are the records the request read and
// will write; if any of them differs once the lock is re-taken (changed,
// created or deleted), it returns errChanged, and the caller's request is
// run again on what is there now.
func (s *Server) checkUnlocked(keys []string, name, resourceType, method string, m proto.Message) error {
	if constraintParent(name) == "" || !orgpolicy.SourceSet() {
		return nil
	}
	before := s.rawRecords(keys)
	s.mu.Unlock()
	err := checkCustom(name, resourceType, method, m)
	s.mu.Lock()
	if err != nil {
		return err
	}
	after := s.rawRecords(keys)
	for i := range before {
		if before[i] != after[i] {
			return errChanged
		}
	}
	return nil
}

// rawRecords is the stored bytes of each key, "\x00absent" for a missing
// one, or "\x00error" for one that could not be read (which counts as a
// change, so the request is run again and reports the read's error).
func (s *Server) rawRecords(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		b, err := s.db.Get(k)
		switch {
		case errors.Is(err, store.ErrNotFound):
			out[i] = "\x00absent"
		case err != nil:
			out[i] = "\x00error" + strconv.Itoa(i)
		default:
			out[i] = string(b)
		}
	}
	return out
}

// changedRetries is how many times a request is run again after a
// concurrent change before it is refused ABORTED, the code Cloud KMS
// gives a concurrent edit (an etag that "has changed since it was
// read").
const changedRetries = 8

// retryChanged runs f until it does not report errChanged.
func retryChanged[T any](f func() (T, error)) (T, error) {
	for range changedRetries {
		out, err := f()
		if !errors.Is(err, errChanged) {
			return out, err
		}
	}
	var zero T
	return zero, apierror.Wrap(apierror.Aborted("the resource was changed by another request while this one was checked against custom organization policy constraints; retry"))
}
