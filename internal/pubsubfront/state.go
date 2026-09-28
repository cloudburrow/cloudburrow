package pubsubfront

// The front's own state, kept in a file (#898). What the front knows and the
// emulator does not (the expiration policies updates set, the clock's
// offset, when each subscription was last active, and the projects calls
// have named) would otherwise live only in the front's memory, so a restart
// of the front's container alone, with the emulator still running and
// holding every subscription, would give the subscriptions back their
// emulator-stored policy, forget how long they had been idle, and stop
// expiring them. `cloudburrow up` keeps the file on an emptyDir volume of the
// Pub/Sub pod: it outlives a restart of the front's container, and goes with
// the pod, as the emulator's resources do, so the two never disagree on
// whether a pod restart kept anything.
//
// A change to a policy, the clock or an import of activity is written before
// the call that made it is answered. Activity and projects, which nearly
// every call changes, are written at most every flushInterval, and when the
// front stops; a front killed outright loses at most that much activity,
// which only makes a subscription look idle a moment early against a ttl of
// at least a day.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/encoding/protojson"
)

// flushInterval is how often activity is written to the state file.
const flushInterval = time.Second

// savedState is the state file's content.
type savedState struct {
	// ClockOffsetNanos is how far the front's clock is ahead of the wall
	// clock.
	ClockOffsetNanos int64 `json:"clockOffsetNanos"`
	// Policies are the kept expiration policies, as protojson, by
	// subscription name.
	Policies map[string]json.RawMessage `json:"policies,omitempty"`
	// LastActive is when each subscription was last active, by the front's
	// clock; one with a streaming pull open is active as of the write.
	LastActive map[string]time.Time `json:"lastActive,omitempty"`
	// Projects is every project a call has named.
	Projects []string `json:"projects,omitempty"`
}

// KeepState restores the front's state from path, when the file exists, and
// from then on keeps it there. It must be called before the front serves
// its first call. The file is written once at once, so a path that cannot
// be written fails here rather than on the first update. A file that cannot
// be read as state is logged and replaced: the front starts as if it had
// none, as it did before #898.
func (f *Front) KeepState(path string) error {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("read the front's state %s: %w", path, err)
	default:
		if err := f.restore(b); err != nil {
			f.logf("pubsub front: the state in %s is unreadable, starting without it: %v", path, err)
		} else {
			f.logf("pubsub front: restored %s", path)
		}
	}
	f.mu.Lock()
	f.statePath = path
	f.mu.Unlock()
	return f.save()
}

// restore sets the front's state from a state file's content.
func (f *Front) restore(b []byte) error {
	var s savedState
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	policies := make(map[string]*pubsubpb.ExpirationPolicy, len(s.Policies))
	for name, raw := range s.Policies {
		p := &pubsubpb.ExpirationPolicy{}
		if err := protojson.Unmarshal(raw, p); err != nil {
			return fmt.Errorf("the policy of %s: %w", name, err)
		}
		policies[name] = p
	}
	if s.ClockOffsetNanos < 0 {
		return fmt.Errorf("a negative clock offset, %d", s.ClockOffsetNanos)
	}
	f.clock.raise(time.Duration(s.ClockOffsetNanos))
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, p := range policies {
		f.policies[name] = p
	}
	for name, last := range s.LastActive {
		f.subs[name] = &subState{last: last}
	}
	for _, p := range s.Projects {
		f.projects[p] = true
	}
	return nil
}

// persist writes the state now, when the front keeps it; a failure is
// logged, since the call that changed the state has succeeded.
func (f *Front) persist() {
	if err := f.save(); err != nil {
		f.logf("pubsub front: %v", err)
	}
}

// markDirty notes a change to be written by the next flush.
func (f *Front) markDirty() { f.dirty.Store(true) }

// flush writes the state if it changed since it was last written.
func (f *Front) flush() {
	if f.dirty.Load() {
		f.persist()
	}
}

// save writes the state to the file, replacing it whole, so a reader never
// sees half of one. saveMu orders the writes: the snapshot is taken under
// it, so a later write always holds a later state.
func (f *Front) save() error {
	f.saveMu.Lock()
	defer f.saveMu.Unlock()
	f.dirty.Store(false)
	now := f.clock.Now()
	s := savedState{ClockOffsetNanos: int64(f.clock.Offset())}
	f.mu.Lock()
	path := f.statePath
	if path == "" {
		f.mu.Unlock()
		return nil
	}
	if len(f.policies) > 0 {
		s.Policies = make(map[string]json.RawMessage, len(f.policies))
	}
	var err error
	for name, p := range f.policies {
		if s.Policies[name], err = protojson.Marshal(p); err != nil {
			f.mu.Unlock()
			return fmt.Errorf("encode the policy of %s: %w", name, err)
		}
	}
	if len(f.subs) > 0 {
		s.LastActive = make(map[string]time.Time, len(f.subs))
	}
	for name, st := range f.subs {
		last := st.last
		if st.streams > 0 {
			last = now
		}
		s.LastActive[name] = last
	}
	for p := range f.projects {
		s.Projects = append(s.Projects, p)
	}
	f.mu.Unlock()
	sort.Strings(s.Projects)
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode the front's state: %w", err)
	}
	return writeFileAtomic(path, b)
}

// writeFileAtomic replaces path with b by a rename, so the file is always
// whole.
func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write the front's state: %w", err)
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write the front's state %s: %w", path, werr)
	}
	return nil
}
