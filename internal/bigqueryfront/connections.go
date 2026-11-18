package bigqueryfront

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The BigQuery Connection API (#1755), bigqueryconnection.googleapis.com
// v1, served on the BigQuery front's own port, as
// https://docs.cloud.google.com/bigquery/docs/reference/bigqueryconnection/rest/v1/projects.locations.connections
// documents it (read 2026-11-17): create (connectionId optional), get,
// list (pageSize, pageToken), patch (updateMask required), delete,
// getIamPolicy, setIamPolicy and testIamPermissions.
//
// A connection is kept as the client wrote it, with its output-only
// fields: name (projects/{project number}/locations/{location}/
// connections/{id}), creationTime and lastModifiedTime (milliseconds),
// hasCredential (a cloudSql credential was given; the credential itself
// is input only and never returned), and the service account BigQuery
// gives a cloudResource, spark or cloudSql connection
// (bqcx-{project number}-{4 chars}@gcp-sa-bigquery-condel.iam.gserviceaccount.com,
// the form Google's docs show). Nothing is dialled: a connection is what
// CREATE MODEL … REMOTE WITH CONNECTION, object tables and BigLake tables
// name, and CloudBurrow resolves those itself.
//
// UNVERIFIED (docs re-read 2026-11-17: <https://docs.cloud.google.com/bigquery/docs/reference/bigqueryconnection/rest/v1/projects.locations.connections/list>,
// which marks pageSize required but gives no default): a pageSize of 0 is
// 100; the service account's random suffix.

var connectionRoute = regexp.MustCompile(`^/v1/projects/([^/]+)/locations/([^/]+)/connections(?:/([^/:]+))?(?::(getIamPolicy|setIamPolicy|testIamPermissions))?$`)

// connectionIDPattern is the documented connection ID: letters, digits
// and underscores (dashes too, as the console accepts), at most 1,024.
var connectionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// connectionTypes are the Connection's oneof properties.
var connectionTypes = []string{"cloudSql", "aws", "azure", "cloudSpanner", "cloudResource", "spark", "salesforceDataCloud"}

type bqConnection struct {
	Project  string                     `json:"project"` // its number
	Location string                     `json:"location"`
	ID       string                     `json:"id"`
	Fields   map[string]json.RawMessage `json:"fields"`
	Created  int64                      `json:"created"`
	Modified int64                      `json:"modified"`
	Policy   json.RawMessage            `json:"policy,omitempty"`
	Etag     int64                      `json:"etag"`
}

// bqConnections are every project's connections.
type bqConnections struct {
	mu    sync.Mutex
	conns map[string]*bqConnection
	path  string
	logf  func(string, ...any)
	now   func() time.Time
	// resourceManager is the instance's Resource Manager, whose project
	// policies testIamPermissions reads for bigquery.connections.use
	// (#1770), as the remote models' check does (connectionauth.go).
	resourceManager string
}

func connectionsStateFile(dir string) string { return filepath.Join(dir, "connections.json") }

func connKey(project, location, id string) string {
	return project + "\x00" + strings.ToLower(location) + "\x00" + id
}

// keep restores the connections from path, and keeps them there.
func (c *bqConnections) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var saved []bqConnection
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the connections it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the connections it kept", path)
		saved = nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns = map[string]*bqConnection{}
	for i := range saved {
		cn := saved[i]
		c.conns[connKey(cn.Project, cn.Location, cn.ID)] = &cn
	}
	c.path, c.logf = path, logf
}

func (c *bqConnections) saveLocked() {
	if c.path == "" {
		return
	}
	out := make([]bqConnection, 0, len(c.conns))
	for _, cn := range c.conns {
		out = append(out, *cn)
	}
	sort.Slice(out, func(a, b int) bool {
		return connKey(out[a].Project, out[a].Location, out[a].ID) < connKey(out[b].Project, out[b].Location, out[b].ID)
	})
	b, err := json.Marshal(out)
	if err == nil {
		err = writeFileAtomic(c.path, b)
	}
	if err != nil && c.logf != nil {
		c.logf("bigquery front: keep the connections: %v", err)
	}
}

// lookup is a connection, by project number and location and ID.
func (c *bqConnections) lookup(number int64, location, id string) (*bqConnection, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cn, ok := c.conns[connKey(strconv.FormatInt(number, 10), location, id)]
	if !ok {
		return nil, false
	}
	cp := *cn
	return &cp, true
}

func (c *bqConnections) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// resource is the Connection as the API returns it.
func (cn *bqConnection) resource(number int64) map[string]any {
	res := map[string]any{}
	for k, v := range cn.Fields {
		res[k] = v
	}
	res["name"] = fmt.Sprintf("projects/%d/locations/%s/connections/%s", number, cn.Location, cn.ID)
	res["creationTime"] = strconv.FormatInt(cn.Created, 10)
	res["lastModifiedTime"] = strconv.FormatInt(cn.Modified, 10)
	return res
}

// connectionProject is a path's project as its number, which keys the
// connections: a project ID and its number name the same project.
func connectionProject(p string, numbers *projectNumbers) int64 {
	if n, err := strconv.ParseInt(p, 10, 64); err == nil && n > 0 {
		return n
	}
	return numbers.of(p)
}

// prepare checks a Connection body and fills its output-only fields:
// fields is the client's, old the connection's before a patch.
func prepareConnection(fields map[string]json.RawMessage, number int64) (int, string) {
	for _, k := range []string{"name", "creationTime", "lastModifiedTime", "hasCredential"} {
		delete(fields, k)
	}
	n := 0
	for _, t := range connectionTypes {
		raw, ok := fields[t]
		if !ok {
			continue
		}
		n++
		var props map[string]json.RawMessage
		if err := json.Unmarshal(raw, &props); err != nil {
			return 400, "Invalid value for " + t + ": " + err.Error()
		}
		if props == nil {
			props = map[string]json.RawMessage{}
		}
		switch t {
		case "cloudSql":
			if _, ok := props["credential"]; ok {
				delete(props, "credential")
				fields["hasCredential"] = json.RawMessage("true")
			}
			fallthrough
		case "cloudResource", "spark":
			if _, ok := props["serviceAccountId"]; !ok {
				suffix := make([]byte, 2)
				_, _ = rand.Read(suffix)
				sa, _ := json.Marshal(fmt.Sprintf("bqcx-%d-%s@gcp-sa-bigquery-condel.iam.gserviceaccount.com", number, hex.EncodeToString(suffix)))
				props["serviceAccountId"] = sa
			}
		}
		b, _ := json.Marshal(props)
		fields[t] = b
	}
	if n > 1 {
		return 400, "A connection has exactly one of cloudSql, aws, azure, cloudSpanner, cloudResource, spark and salesforceDataCloud"
	}
	return 0, ""
}

// serve answers the Connection API's paths; it reports whether path was
// one.
func (c *bqConnections) serve(w http.ResponseWriter, r *http.Request, numbers *projectNumbers) bool {
	m := connectionRoute.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return false
	}
	number, location, id, verb := connectionProject(m[1], numbers), strings.ToLower(m[2]), m[3], m[4]
	project := strconv.FormatInt(number, 10)
	parent := fmt.Sprintf("projects/%d/locations/%s", number, location)
	fail := func(code int, msg string) { writeError(w, code, connReason(code), msg) }
	notFound := func() {
		fail(404, "Not found: Connection "+parent+"/connections/"+id)
	}
	body := func() (map[string]json.RawMessage, bool) {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			fail(400, "Invalid request body: "+err.Error())
			return nil, false
		}
		out := map[string]json.RawMessage{}
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &out); err != nil {
				fail(400, "Invalid JSON payload received. "+err.Error())
				return nil, false
			}
		}
		return out, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns == nil {
		c.conns = map[string]*bqConnection{}
	}
	switch {
	case id == "" && verb == "" && r.Method == http.MethodPost:
		fields, ok := body()
		if !ok {
			return true
		}
		id = r.URL.Query().Get("connectionId")
		if id == "" {
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			h := hex.EncodeToString(b)
			id = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		}
		if len(id) > 1024 || !connectionIDPattern.MatchString(id) {
			fail(400, "Invalid connection ID "+strconv.Quote(id)+": it may contain only letters, numbers and underscores, up to 1,024 characters")
			return true
		}
		if _, had := c.conns[connKey(project, location, id)]; had {
			fail(409, "Already Exists: Connection "+parent+"/connections/"+id)
			return true
		}
		if code, msg := prepareConnection(fields, number); code != 0 {
			fail(code, msg)
			return true
		}
		now := c.clock().UnixMilli()
		cn := &bqConnection{Project: project, Location: location, ID: id, Fields: fields, Created: now, Modified: now}
		c.conns[connKey(project, location, id)] = cn
		c.saveLocked()
		writeJSON(w, 200, cn.resource(number))
	case id == "" && verb == "" && r.Method == http.MethodGet:
		size := 100
		if s := r.URL.Query().Get("pageSize"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				fail(400, "Invalid page size "+s)
				return true
			}
			if n > 0 {
				size = n
			}
		}
		var ids []string
		for _, cn := range c.conns {
			if cn.Project == project && cn.Location == location {
				ids = append(ids, cn.ID)
			}
		}
		sort.Strings(ids)
		start := 0
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			start = sort.SearchStrings(ids, tok)
		}
		res := map[string]any{}
		var list []map[string]any
		for i := start; i < len(ids); i++ {
			if len(list) == size {
				res["nextPageToken"] = ids[i]
				break
			}
			list = append(list, c.conns[connKey(project, location, ids[i])].resource(number))
		}
		if list != nil {
			res["connections"] = list
		}
		writeJSON(w, 200, res)
	case id == "":
		fail(405, "Method "+r.Method+" is not allowed on "+r.URL.Path)
	case verb != "":
		if r.Method != http.MethodPost {
			fail(405, "Method "+r.Method+" is not allowed on "+r.URL.Path)
			return true
		}
		cn, ok := c.conns[connKey(project, location, id)]
		if !ok {
			notFound()
			return true
		}
		req, ok := body()
		if !ok {
			return true
		}
		switch verb {
		case "getIamPolicy":
			writeJSON(w, 200, cn.policy())
		case "setIamPolicy":
			var p map[string]any
			if raw, ok := req["policy"]; !ok || json.Unmarshal(raw, &p) != nil {
				fail(400, "setIamPolicy requires a policy")
				return true
			}
			if et, _ := p["etag"].(string); et != "" && et != cn.etagString() {
				fail(409, "The policy's etag does not match the connection's current policy: read it again with getIamPolicy")
				return true
			}
			cn.Etag++
			p["etag"] = cn.etagString()
			if _, ok := p["version"]; !ok {
				p["version"] = 1
			}
			cn.Policy, _ = json.Marshal(p)
			c.saveLocked()
			writeJSON(w, 200, p)
		case "testIamPermissions":
			var perms []string
			if raw, ok := req["permissions"]; ok {
				_ = json.Unmarshal(raw, &perms)
			}
			// bigquery.connections.use is the one permission the
			// instance checks (connectionauth.go, #1765): it is held
			// as that check finds it, through the connection's policy
			// or its project's, by the identity the front takes every
			// caller to be (#1770). The other bigquery.connections.*
			// permissions are checked nowhere, so are held.
			var held []string
			for _, p := range perms {
				if !strings.HasPrefix(p, "bigquery.connections.") {
					continue
				}
				if p == permConnectionsUse {
					ok, code, msg := c.callerMayUse(r.Context(), m[1], number, numbers, cn)
					if code != 0 {
						fail(code, msg)
						return true
					}
					if !ok {
						continue
					}
				}
				held = append(held, p)
			}
			res := map[string]any{}
			if held != nil {
				res["permissions"] = held
			}
			writeJSON(w, 200, res)
		}
	case r.Method == http.MethodGet:
		cn, ok := c.conns[connKey(project, location, id)]
		if !ok {
			notFound()
			return true
		}
		writeJSON(w, 200, cn.resource(number))
	case r.Method == http.MethodDelete:
		if _, ok := c.conns[connKey(project, location, id)]; !ok {
			notFound()
			return true
		}
		delete(c.conns, connKey(project, location, id))
		c.saveLocked()
		writeJSON(w, 200, map[string]any{})
	case r.Method == http.MethodPatch:
		cn, ok := c.conns[connKey(project, location, id)]
		if !ok {
			notFound()
			return true
		}
		mask := r.URL.Query().Get("updateMask")
		if mask == "" {
			fail(400, "Missing required field: update_mask")
			return true
		}
		fields, ok := body()
		if !ok {
			return true
		}
		next := map[string]json.RawMessage{}
		for k, v := range cn.Fields {
			next[k] = v
		}
		for _, path := range strings.Split(mask, ",") {
			path = strings.TrimSpace(path)
			top, sub, nested := strings.Cut(path, ".")
			top = lowerCamel(top)
			known := top == "friendlyName" || top == "description" || top == "kmsKeyName" || top == "configuration"
			for _, t := range connectionTypes {
				known = known || top == t
			}
			if !known {
				fail(400, "Invalid update_mask path "+strconv.Quote(path)+": a connection's updatable fields are friendlyName, description, kmsKeyName, configuration and its type's properties")
				return true
			}
			v, set := fields[top]
			if !nested {
				if set {
					next[top] = v
				} else {
					delete(next, top)
				}
				continue
			}
			var into, from map[string]json.RawMessage
			_ = json.Unmarshal(next[top], &into)
			_ = json.Unmarshal(v, &from)
			if into == nil {
				into = map[string]json.RawMessage{}
			}
			sub = lowerCamel(sub)
			if fv, ok := from[sub]; ok {
				into[sub] = fv
			} else {
				delete(into, sub)
			}
			next[top], _ = json.Marshal(into)
		}
		if code, msg := prepareConnection(next, number); code != 0 {
			fail(code, msg)
			return true
		}
		if _, ok := cn.Fields["hasCredential"]; ok {
			if _, still := next["cloudSql"]; still {
				next["hasCredential"] = cn.Fields["hasCredential"]
			}
		}
		cn.Fields = next
		cn.Modified = c.clock().UnixMilli()
		c.saveLocked()
		writeJSON(w, 200, cn.resource(number))
	default:
		fail(405, "Method "+r.Method+" is not allowed on "+r.URL.Path)
	}
	return true
}

func (cn *bqConnection) etagString() string {
	// An IAM etag is bytes, base64 in JSON.
	return base64.StdEncoding.EncodeToString([]byte("BwY" + strconv.FormatInt(cn.Etag, 36)))
}

func (cn *bqConnection) policy() map[string]any {
	p := map[string]any{}
	if len(cn.Policy) > 0 {
		_ = json.Unmarshal(cn.Policy, &p)
	}
	p["etag"] = cn.etagString()
	if _, ok := p["version"]; !ok {
		p["version"] = 1
	}
	return p
}

// lowerCamel is a field mask path's name as JSON names it: friendly_name
// is friendlyName.
func lowerCamel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func connReason(code int) string {
	switch code {
	case 400:
		return "invalid"
	case 404:
		return "notFound"
	case 409:
		return "duplicate"
	}
	return "invalid"
}

// createCloudResource makes a CLOUD_RESOURCE connection, as the
// DEFAULT keyword does (connectionauth.go, #1765), or returns the one
// already there.
func (c *bqConnections) createCloudResource(number int64, location, id string) (*bqConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conns == nil {
		c.conns = map[string]*bqConnection{}
	}
	project := strconv.FormatInt(number, 10)
	location = strings.ToLower(location)
	if cn, ok := c.conns[connKey(project, location, id)]; ok {
		cp := *cn
		return &cp, nil
	}
	fields := map[string]json.RawMessage{"cloudResource": json.RawMessage("{}")}
	if code, msg := prepareConnection(fields, number); code != 0 {
		return nil, errors.New(msg)
	}
	now := c.clock().UnixMilli()
	cn := &bqConnection{Project: project, Location: location, ID: id, Fields: fields, Created: now, Modified: now}
	c.conns[connKey(project, location, id)] = cn
	c.saveLocked()
	cp := *cn
	return &cp, nil
}

// callerMayUse is whether the caller holds bigquery.connections.use on cn
// (#1770), as checkConnectionUse decides it: through an unconditioned
// binding of the connection's policy or of its project's, whose ID is
// raw, the project the request named, or the one number names.
func (c *bqConnections) callerMayUse(ctx context.Context, raw string, number int64, numbers *projectNumbers, cn *bqConnection) (bool, int, string) {
	id := raw
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		if known, ok := numbers.idOf(n); ok {
			id = known
		} else if named, ok := callerProjectIn(cn.bindings(), n, numbers); ok {
			// A number the host did not send, derived from the ID: the
			// policy's own binding to the caller of that project names it.
			id = named
		}
	}
	caller := front{resourceManager: c.resourceManager}.members(ctx, "serviceAccount:"+callerIdentity(id))
	if bindingsGrant(cn.bindings(), caller, permConnectionsUse) {
		return true, 0, ""
	}
	proj, code, msg := front{resourceManager: c.resourceManager}.projectBindings(ctx, id)
	if code != 0 {
		return false, code, msg
	}
	return bindingsGrant(proj, caller, permConnectionsUse), 0, ""
}

// callerProjectIn is the project ID whose number is n, named by a binding
// to the caller of that project (callerIdentity) in bindings.
func callerProjectIn(bindings []policyBinding, n int64, numbers *projectNumbers) (string, bool) {
	for _, b := range bindings {
		for _, m := range b.Members {
			rest, ok := strings.CutPrefix(strings.TrimSpace(m), "serviceAccount:cloudburrow-local@")
			if !ok {
				continue
			}
			id, ok := strings.CutSuffix(rest, ".iam.gserviceaccount.com")
			if ok && id != "" && numbers.of(id) == n {
				return id, true
			}
		}
	}
	return "", false
}
