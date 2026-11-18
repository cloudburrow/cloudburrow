package firestoreadmin

// The console's Query Explain (#1738): Google's console runs Query Explain
// on an Enterprise edition database's MongoDB queries; the instance's
// console reaches the same explain command through the front, which runs it
// as an administrator (the console is the instance's), with no driver.

import (
	"encoding/json"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// MongoExplainPath is where the console posts a MongoExplainRequest.
const MongoExplainPath = "/cloudburrow/v1/firestoreMongoExplain"

// MongoExplainRequest is an explain of a find or an aggregate on database
// (projects/{p}/databases/{d}): Command is the explained command's body as
// MongoDB Extended JSON, such as {"find": "books", "filter": {"year": 1965}}.
type MongoExplainRequest struct {
	Database  string `json:"database"`
	Command   string `json:"command"`
	Verbosity string `json:"verbosity"`
}

// MongoExplainResponse is the explain command's answer, as relaxed
// Extended JSON, or the error it gave.
type MongoExplainResponse struct {
	Explain string `json:"explain,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (a *Admin) serveMongoExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a MongoExplainRequest", http.StatusMethodNotAllowed)
		return
	}
	var req MongoExplainRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp := a.mongoExplain(r, req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *Admin) mongoExplain(r *http.Request, req MongoExplainRequest) MongoExplainResponse {
	a.mu.Lock()
	s := a.mongoServer
	a.mu.Unlock()
	if s == nil {
		return MongoExplainResponse{Error: "Firestore with MongoDB compatibility is not served by this front"}
	}
	a.mu.Lock()
	_, err := a.enterprise(req.Database)
	a.mu.Unlock()
	if err != nil {
		return MongoExplainResponse{Error: err.Error()}
	}
	var inner bson.D
	if err := bson.UnmarshalExtJSON([]byte(req.Command), false, &inner); err != nil || len(inner) == 0 {
		return MongoExplainResponse{Error: "the command is not a MongoDB Extended JSON document naming a command"}
	}
	id := req.Database[strings.LastIndex(req.Database, "/")+1:]
	cmd := bson.D{{Key: "explain", Value: inner}}
	if req.Verbosity != "" {
		cmd = append(cmd, bson.E{Key: "verbosity", Value: req.Verbosity})
	}
	cmd = append(cmd, bson.E{Key: "$db", Value: id})
	mc := &mongoConn{db: &mongoDB{name: req.Database, id: id}}
	out, err := s.explain(r.Context(), mc, cmd)
	if err != nil {
		return MongoExplainResponse{Error: err.Error()}
	}
	raw, err := bson.MarshalExtJSON(out, false, false)
	if err != nil {
		return MongoExplainResponse{Error: err.Error()}
	}
	return MongoExplainResponse{Explain: string(raw)}
}
