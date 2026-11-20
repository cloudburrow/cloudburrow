// Package quotalimit is where a service that enforces one of Google's
// quotas asks for the project's value (#2030): the documented default, or
// what a Cloud Quotas preference granted. The Cloud Quotas API
// (internal/service/cloudquotas) is the Source; with none set, every quota
// is its default, as before the API was served.
package quotalimit

import (
	"sync"
	"sync/atomic"
)

// Source answers a project's value of a quota.
type Source interface {
	// Value is the project's value of service's quotaID in the dimensions
	// (a region, or none), and false when no preference changes it.
	Value(project, service, quotaID string, dims map[string]string) (int64, bool)
}

var source atomic.Value // holder

type holder struct{ s Source }

// Use sets the source; nil removes it.
func Use(s Source) { source.Store(holder{s}) }

// Limit is the value of service's quotaID for project in dims: what a
// preference granted, or def.
func Limit(project, service, quotaID string, dims map[string]string, def int64) int64 {
	h, _ := source.Load().(holder)
	if h.s == nil {
		return def
	}
	if v, ok := h.s.Value(project, service, quotaID, dims); ok {
		return v
	}
	return def
}

// UsageFunc counts a project's current usage of a quota in dims.
type UsageFunc func(project string, dims map[string]string) (int64, error)

var (
	usageMu sync.RWMutex
	usage   = map[string]UsageFunc{}
)

// RegisterUsage records how to count service's quotaID, for the Cloud
// Quotas safety check that refuses a decrease below current usage.
func RegisterUsage(service, quotaID string, f UsageFunc) {
	usageMu.Lock()
	defer usageMu.Unlock()
	if f == nil {
		delete(usage, service+"/"+quotaID)
		return
	}
	usage[service+"/"+quotaID] = f
}

// Usage is the project's usage of service's quotaID, and false when no
// count is registered for it.
func Usage(project, service, quotaID string, dims map[string]string) (int64, bool, error) {
	usageMu.RLock()
	f := usage[service+"/"+quotaID]
	usageMu.RUnlock()
	if f == nil {
		return 0, false, nil
	}
	n, err := f(project, dims)
	return n, err == nil, err
}
