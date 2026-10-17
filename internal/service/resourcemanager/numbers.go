package resourcemanager

// Project numbers (#1544).
//
// Google gives every project a number when it is created, and a project
// created again under a purged project's ID gets a new one: the old number
// is never reused. The number was derived from the ID, so a re-created
// project took the old one's, and anything that named the old project by
// number (tag binding parents, Service Usage operations, service agents,
// bucket project numbers) named the new one.
//
// A project's number is now kept in its record. A project gets
// resource.DerivedProjectNumber the first time its ID is used, as every
// project had before, so the numbers of existing instances and of pods that
// cannot read the registry stay what they were; a number already retired or
// held by another project is never handed out, so a re-created project
// gets a random one. A purge retires the purged project's number for good.

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

const (
	// retiredNumberPrefix + number holds the ID of the purged project that
	// had the number. Never reset: a purged number stays retired.
	retiredNumberPrefix = "resourcemanager/retired-numbers/"
	// retiredIDPrefix + ID holds the number of the last project purged
	// under the ID, which the purge hooks and the next project made under
	// it use to find what the purged project left.
	retiredIDPrefix = "resourcemanager/retired-ids/"
)

// numberOf is a project record's number: the one assigned at its creation,
// or, for a record written before #1544, the derived one it always had.
func numberOf(p Project) int64 {
	if p.Number != 0 {
		return p.Number
	}
	return resource.DerivedProjectNumber(p.ProjectID)
}

// projectNumber is the number of the project with this ID: its record's,
// the last purged one's when there is no record, or the derived number.
// It takes no lock, so code holding r.mu may call it.
func (r *Registry) projectNumber(id string) int64 {
	if n, ok := r.NumberOf(id); ok {
		return n
	}
	return resource.DerivedProjectNumber(id)
}

// NumberOf is the number of the project with this ID: its record's, or the
// last purged project's under the ID when there is none. It reports false
// for an ID never registered. It takes no lock, and is what `cloudburrow
// up` installs as resource.SetProjectNumberResolver.
func (r *Registry) NumberOf(id string) (int64, bool) {
	if id == "" || strings.ContainsAny(id, "/\\") {
		return 0, false
	}
	if b, err := r.st.Get(key(id)); err == nil {
		var p Project
		if json.Unmarshal(b, &p) == nil {
			return numberOf(p), true
		}
	}
	if b, err := r.st.Get(retiredIDPrefix + id); err == nil {
		if n, err := strconv.ParseInt(string(b), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// NumberOverrides is every registered project whose number is not the
// derived one, by ID: what a builtin server that cannot read the registry
// must be told (the storage server's project numbers).
func (r *Registry) NumberOverrides() (map[string]int64, error) {
	keys, err := r.st.List(keyPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list projects")
	}
	out := map[string]int64{}
	for _, k := range keys {
		id := strings.TrimPrefix(k, keyPrefix)
		if n, ok := r.NumberOf(id); ok && n != resource.DerivedProjectNumber(id) {
			out[id] = n
		}
	}
	return out, nil
}

// assignNumberLocked picks a new project's number: the derived one, unless
// it is retired or another project holds it, else a random free one. The
// caller holds r.mu.
func (r *Registry) assignNumberLocked(id string) (int64, error) {
	used := map[int64]bool{}
	keys, err := r.st.List(keyPrefix)
	if err != nil {
		return 0, apierror.Internal(err, "list projects")
	}
	for _, k := range keys {
		if n, ok := r.NumberOf(strings.TrimPrefix(k, keyPrefix)); ok {
			used[n] = true
		}
	}
	free := func(n int64) bool {
		if used[n] {
			return false
		}
		_, err := r.st.Get(retiredNumberPrefix + strconv.FormatInt(n, 10))
		return err != nil
	}
	if n := resource.DerivedProjectNumber(id); free(n) {
		return n, nil
	}
	for {
		v, err := rand.Int(rand.Reader, big.NewInt(900000000000))
		if err != nil {
			return 0, apierror.Internal(err, "pick a project number")
		}
		if n := v.Int64() + 100000000000; free(n) {
			return n, nil
		}
	}
}

// retireNumberLocked records that a purged project's number is never to be
// given out again, and that it was the last one under its ID. The caller
// holds r.mu, before the record is deleted.
func (r *Registry) retireNumberLocked(p Project) {
	n := strconv.FormatInt(numberOf(p), 10)
	_ = r.st.Put(retiredNumberPrefix+n, []byte(p.ProjectID))
	_ = r.st.Put(retiredIDPrefix+p.ProjectID, []byte(n))
}

// OnNumbersChanged registers fn to be called, without the registry's lock
// held, after a project is created or purged: what `cloudburrow up` uses to
// send NumberOverrides to the builtin storage server.
func (r *Registry) OnNumbersChanged(fn func()) {
	r.mu.Lock()
	defer r.unlock()
	r.onNumbers = append(r.onNumbers, fn)
}
