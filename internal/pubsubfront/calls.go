package pubsubfront

// The front's call log (#1266, internal/calllog): every Pub/Sub call a
// client makes through the front, gRPC or REST, kept for the CLI's metrics,
// request log and traces.

import (
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
)

// CallsPath serves the call log (calllog.Path).
const CallsPath = calllog.Path

// Call is one completed call through the front.
type Call = calllog.Call

// Calls returns the calls numbered after seq, oldest first.
func (f *Front) Calls(after uint64) []Call { return f.calls.After(after) }

func (f *Front) serveCalls(w http.ResponseWriter, r *http.Request) { f.calls.ServeHTTP(w, r) }

func (f *Front) recordREST(next http.Handler) http.Handler { return calllog.REST(&f.calls, next) }

// firstName reads the resource a request names (calllog.FirstName).
func firstName(m any) string {
	if fr, ok := m.(*frame); ok {
		return calllog.FirstName(*fr)
	}
	return ""
}

func isStreamingPull(method string) bool { return method == subscriber+"StreamingPull" }
