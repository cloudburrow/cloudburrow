package spannerfront

import (
	"net/http"
	"regexp"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A customer-managed database's REST data calls (#1972): the REST bindings
// of the data calls cmekdata.go gates over gRPC. Spanner's REST port is the
// same API as its gRPC one (internal/callfront), so a client on REST
// (gcloud spanner databases execute-sql, the REST client libraries) is
// refused with the same FAILED_PRECONDITION while the database's key
// version cannot unwrap its data key. As AccessGate does for Firestore, the
// front claims only the calls it refuses: any other passes to the
// emulator's REST gateway as before.

// restDataCall is a REST data call on a database or one of its sessions:
//
//	POST   /v1/{database}/sessions                 CreateSession
//	POST   /v1/{database}/sessions:batchCreate     BatchCreateSessions
//	GET    /v1/{database}/sessions[/{session}]     ListSessions, GetSession
//	POST   /v1/{database}/sessions/{session}:{verb} executeSql, read, commit, ...
//
// DeleteSession (DELETE on a session) is never refused, as over gRPC.
var restDataCall = regexp.MustCompile(`^/v1/(` + pDB + `)/sessions(?:/[^/:]+)?(?::[A-Za-z]+)?$`)

// restDataDatabase is the database a REST data call names, or "".
func restDataDatabase(r *http.Request) string {
	if r.Method == http.MethodDelete {
		return ""
	}
	m := restDataCall.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return ""
	}
	return m[1]
}

// restDataRefusal is the refusal a REST data call gets, or nil.
func (s *Instances) restDataRefusal(r *http.Request) error {
	if !s.hasDatabaseKeys() {
		return nil
	}
	db := restDataDatabase(r)
	if db == "" {
		return nil
	}
	return s.databaseAccessible(r.Context(), db)
}

// serveRESTDataRefusal answers a REST data call Claims claimed with its
// refusal.
func (s *Instances) serveRESTDataRefusal(w http.ResponseWriter, r *http.Request) {
	err := s.restDataRefusal(r)
	if err == nil {
		err = status.Error(codes.Unavailable, "the database's key became accessible while the call was answered; retry it")
	}
	writeError(w, err)
}
