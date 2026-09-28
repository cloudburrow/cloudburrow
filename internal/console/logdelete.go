package console

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Deleting a Cloud Logging log from the Logs Explorer (#799).
//
// Only the entries the Cloud Logging API holds can be deleted, because only
// they have a DeleteLog behind them: a pod's lines are read from the cluster
// as they are written, and CloudBurrow's own request log is the admin event
// log, and neither has an API that removes anything. The Explorer offers the
// action on `logging/<log>` rows alone and says so for the rest.

// LogDeleter is Cloud Logging's LoggingServiceV2/DeleteLog, called in process
// on the same server the gRPC and JSON transports serve, so a log deleted here
// is one an SDK no longer lists.
type LogDeleter interface {
	// DeleteLog deletes the log by its full name, projects/<p>/logs/<id>, and
	// returns the API's own error.
	DeleteLog(ctx context.Context, logName string) error
}

// SetLogDeleter attaches Cloud Logging's DeleteLog. Nil, the default, offers
// no Delete log: the instance serves no Logging API.
func (s *Server) SetLogDeleter(d LogDeleter) { s.logDeleter = d }

// LoggingSourcePrefix starts the source of every entry the Cloud Logging API
// holds, and of no other.
const LoggingSourcePrefix = "logging/"

// logOfProject reports whether name is a log of project: projects/<p>/logs/<id>
// with an id. Checked here so a hand-written request cannot reach another
// project's log from the project the screen is scoped to.
func logOfProject(project, name string) bool {
	id, ok := strings.CutPrefix(name, "projects/"+project+"/logs/")
	return ok && id != "" && !strings.Contains(id, "/")
}

// handleDeleteLog is DELETE /api/logs?project=<p>&log=<full log name>.
func (s *Server) handleDeleteLog(w http.ResponseWriter, r *http.Request) {
	if s.logDeleter == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "Cloud Logging is not enabled on this instance, so no log can be deleted",
		})
		return
	}
	project, name := r.URL.Query().Get("project"), r.URL.Query().Get("log")
	if project == "" || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "project and log are required; refusing to delete without both",
		})
		return
	}
	if !logOfProject(project, name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": name + " is not a log of project " + project,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Instrumented as every other console delete: in Activity, and in the
	// Explorer under the plain source "logging", which is not a log the API
	// holds and so offers no Delete log of its own.
	opID := s.logs.StartOperation("delete log", name, project)
	if err := s.logDeleter.DeleteLog(ctx, name); err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		s.logs.Log(Entry{
			Severity: SeverityError, Source: "logging", Project: project, Resource: name,
			OperationID: opID, Message: "delete log failed: " + userMessage(err),
		})
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": userMessage(err), "operation": opID,
		})
		return
	}
	// The Explorer's copy of the log goes with it. Without this the page kept
	// showing entries the API had deleted, which is the one thing the reader
	// asked not to see again.
	removed := s.logs.forgetLog(name)
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	s.logs.Log(Entry{
		Severity: SeverityInfo, Source: "logging", Project: project, Resource: name,
		OperationID: opID, Message: "deleted log " + name,
	})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name, "operation": opID, "removed": removed})
}

// forgetLog drops the entries the Cloud Logging API wrote for one log, and
// returns how many. Entries from any other source are kept, whatever their
// resource says.
func (r *Recorder) forgetLog(name string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.entries[:0]
	removed := 0
	for _, e := range r.entries {
		if strings.HasPrefix(e.Source, LoggingSourcePrefix) && e.Resource == name {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	// Cleared past the new end, so the dropped entries are not held alive by
	// the backing array.
	clear(r.entries[len(kept):])
	r.entries = kept
	return removed
}
