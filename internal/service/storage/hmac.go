package storage

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// HMAC keys and the project's service agent (#505), to
// docs.cloud.google.com/storage/docs/authentication/hmackeys. A key's
// access ID is 61 characters, and its secret 40 base64 characters shown
// once, in the create response, and never again: get, list and update
// return metadata only. A service account has at most 10 keys, deleted
// ones not counted. A key is ACTIVE or INACTIVE; it can be deleted only
// when INACTIVE, after which it is DELETED, listed only with
// showDeletedKeys=true, and cannot change again.
//
// The secret is kept, because signing with it (#509) needs it, the way
// Secret Manager keeps payloads: in the server's store, never in a log or
// an event (which record the method, path and status only). A state
// capture that includes the store holds it and must be marked as holding
// secrets (#511).

const (
	hmacPrefix       = "hmac/"
	maxHMACKeys      = 10
	hmacListMax      = 250
	hmacAccessIDSize = 61
)

type hmacKey struct {
	AccessID string    `json:"accessId"`
	Project  string    `json:"project"`
	Email    string    `json:"email"`
	State    string    `json:"state"`
	Secret   string    `json:"secret"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Seq      uint64    `json:"seq"`
}

func hmacKeyKey(project, id string) string { return hmacPrefix + project + "/" + id }

// newAccessID is "GOOG" and 57 base32 characters: 61 in all, the length of
// a service account's access ID (hmackeys docs, whose example starts GOOG).
func newAccessID() string {
	b := make([]byte, 40)
	_, _ = rand.Read(b)
	return "GOOG" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:hmacAccessIDSize-4]
}

// newHMACSecret is 30 random bytes, which is 40 base64 characters.
func newHMACSecret() string {
	b := make([]byte, 30)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func (s *Server) hmacJSON(r *http.Request, k hmacKey) map[string]any {
	return map[string]any{
		"kind": "storage#hmacKeyMetadata", "accessId": k.AccessID, "id": k.Project + "/" + k.AccessID,
		"projectId": k.Project, "serviceAccountEmail": k.Email, "state": k.State,
		"timeCreated": rfc3339(k.Created), "updated": rfc3339(k.Updated), "etag": policyETag(k.Seq),
		"selfLink": baseURL(r) + jsonPrefix + "projects/" + escape(k.Project) + "/hmacKeys/" + escape(k.AccessID),
	}
}

func getHMACKey(tx Tx, project, id string) (hmacKey, bool, error) {
	var k hmacKey
	raw, ok := tx.Get(hmacKeyKey(project, id))
	if !ok {
		return k, false, nil
	}
	return k, true, json.Unmarshal(raw, &k)
}

func putHMACKey(tx Tx, k hmacKey) error {
	raw, err := json.Marshal(k)
	if err != nil {
		return err
	}
	tx.Put(hmacKeyKey(k.Project, k.AccessID), raw)
	return nil
}

func (s *Server) hmacKeysCreate(w http.ResponseWriter, r *http.Request) {
	project := pathVar(r, jsonPrefix, 1)
	email := r.URL.Query().Get("serviceAccountEmail")
	if email == "" {
		writeError(w, required("serviceAccountEmail"))
		return
	}
	if !strings.Contains(email, "@") {
		writeError(w, badRequest("Invalid argument: serviceAccountEmail %q is not an email address", email))
		return
	}
	now := s.now()
	k := hmacKey{AccessID: newAccessID(), Project: project, Email: email, State: "ACTIVE", Secret: newHMACSecret(), Created: now, Updated: now}
	err := s.meta.Update(func(tx Tx) error {
		n := 0
		for _, key := range tx.List(hmacPrefix + project + "/") {
			raw, _ := tx.Get(key)
			var e hmacKey
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			if e.Email == email && e.State != "DELETED" {
				n++
			}
		}
		if n >= maxHMACKeys {
			// UNVERIFIED: the docs state the limit of 10, not the code
			// that refuses the 11th.
			return badRequest("The service account %s already has %d HMAC keys, the maximum; delete one first.", email, maxHMACKeys)
		}
		return putHMACKey(tx, k)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, map[string]any{"kind": "storage#hmacKey", "metadata": s.hmacJSON(r, k), "secret": k.Secret})
}

func (s *Server) hmacKeysGet(w http.ResponseWriter, r *http.Request) {
	project, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	var k hmacKey
	err := s.meta.View(func(tx Tx) error {
		var ok bool
		var err error
		if k, ok, err = getHMACKey(tx, project, id); err != nil {
			return err
		} else if !ok {
			return notFound("Access ID not found in project %s.", project)
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.hmacJSON(r, k))
}

// hmacKeysUpdate changes a key's state between ACTIVE and INACTIVE; an etag
// in the body must match (412 otherwise, UNVERIFIED).
func (s *Server) hmacKeysUpdate(w http.ResponseWriter, r *http.Request) {
	project, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	body, err := readBody(r)
	if err != nil {
		writeError(w, err)
		return
	}
	state, _ := body["state"].(string)
	if state != "ACTIVE" && state != "INACTIVE" {
		writeError(w, badRequest("Invalid argument: state %v must be ACTIVE or INACTIVE; delete a key to remove it", body["state"]))
		return
	}
	etag, _ := body["etag"].(string)
	var k hmacKey
	err = s.meta.Update(func(tx Tx) error {
		var ok bool
		var gerr error
		if k, ok, gerr = getHMACKey(tx, project, id); gerr != nil {
			return gerr
		} else if !ok {
			return notFound("Access ID not found in project %s.", project)
		}
		if k.State == "DELETED" {
			return badRequest("A deleted HMAC key cannot be updated.")
		}
		if etag != "" && etag != policyETag(k.Seq) {
			return preconditionFailed("The HMAC key's etag %q does not match the current one.", etag)
		}
		if k.State != state {
			k.State, k.Updated = state, s.now()
			k.Seq++
		}
		return putHMACKey(tx, k)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeResponse(w, r, http.StatusOK, s.hmacJSON(r, k))
}

func (s *Server) hmacKeysDelete(w http.ResponseWriter, r *http.Request) {
	project, id := pathVar(r, jsonPrefix, 1), pathVar(r, jsonPrefix, 3)
	err := s.meta.Update(func(tx Tx) error {
		k, ok, err := getHMACKey(tx, project, id)
		if err != nil {
			return err
		} else if !ok || k.State == "DELETED" {
			return notFound("Access ID not found in project %s.", project)
		}
		if k.State != "INACTIVE" {
			return badRequest("Cannot delete keys in 'ACTIVE' state; set the key INACTIVE first.")
		}
		// A deleted key keeps its metadata, listed with showDeletedKeys;
		// its secret is gone.
		k.State, k.Secret, k.Updated = "DELETED", "", s.now()
		k.Seq++
		return putHMACKey(tx, k)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) hmacKeysList(w http.ResponseWriter, r *http.Request) {
	project := pathVar(r, jsonPrefix, 1)
	q := r.URL.Query()
	max := hmacListMax
	if v := q.Get("maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, badRequest("Invalid argument: maxResults=%q", v))
			return
		}
		if n > 0 && n < max {
			max = n
		}
	}
	after := ""
	if tok := q.Get("pageToken"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			writeError(w, badRequest("Invalid argument: pageToken"))
			return
		}
		after = string(b)
	}
	email, deleted := q.Get("serviceAccountEmail"), q.Get("showDeletedKeys") == "true"
	var items []any
	next, last := "", ""
	err := s.meta.View(func(tx Tx) error {
		for _, key := range tx.List(hmacPrefix + project + "/") {
			raw, _ := tx.Get(key)
			var k hmacKey
			if err := json.Unmarshal(raw, &k); err != nil {
				return err
			}
			if k.AccessID <= after || email != "" && k.Email != email || k.State == "DELETED" && !deleted {
				continue
			}
			if len(items) == max {
				next = base64.RawURLEncoding.EncodeToString([]byte(last))
				break
			}
			items, last = append(items, s.hmacJSON(r, k)), k.AccessID
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{"kind": "storage#hmacKeysMetadata"}
	if len(items) > 0 {
		resp["items"] = items
	}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeResponse(w, r, http.StatusOK, resp)
}

// serviceAccountGet is the project's Cloud Storage service agent,
// service-PROJECT_NUMBER@gs-project-accounts.iam.gserviceaccount.com, with
// the project number CloudBurrow derives for every project.
func (s *Server) serviceAccountGet(w http.ResponseWriter, r *http.Request) {
	project := pathVar(r, jsonPrefix, 1)
	writeResponse(w, r, http.StatusOK, map[string]any{
		"kind":          "storage#serviceAccount",
		"email_address": "service-" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "@gs-project-accounts.iam.gserviceaccount.com",
	})
}
