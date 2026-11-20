package workflows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/localhost"
)

// TokenMinter mints the tokens an http.* step's auth and a connector ask
// for, signed by the instance's own local key; nothing is obtained from
// Google.
type TokenMinter interface {
	ServiceAccountIDToken(account, audience string) (string, error)
	ServiceAccountAccessToken(account, scope string) (string, error)
}

// HTTPCaller sends an execution's http.* steps and connector calls. It
// never sends a request outside the instance: a URL whose host is not
// loopback or a .localhost name is refused before anything is sent, and a
// googleapis.com URL is sent to the local endpoint of the API it names when
// this instance serves it, and refused when it does not.
type HTTPCaller struct {
	Client *http.Client
	// Local is the "http://host:port" serving an API, by the first label of
	// its googleapis.com host ("storage", "pubsub", "run" ...), or "" when
	// this instance does not serve it.
	Local func(api string) string
	// Tokens and ServiceAccount authenticate auth: OIDC and OAuth2 steps
	// and connectors; nil sends no Authorization header.
	Tokens         TokenMinter
	ServiceAccount string
}

// NewHTTPClient is the client executions use: .localhost names dial
// loopback, no proxy is consulted, and a dial to any address that is not
// loopback is refused, so a redirect cannot leave the instance either.
func NewHTTPClient() *http.Client {
	t := localhost.Transport()
	t.Proxy = nil
	d := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				return &egressError{host: host}
			}
			return nil
		}}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, localhost.Rewrite(addr))
	}
	return &http.Client{Transport: t}
}

type egressError struct{ host string }

func (e *egressError) Error() string {
	return fmt.Sprintf("refused to connect to %s: CloudBurrow runs locally and sends no request outside the instance", e.host)
}

// isLocalHost is whether a URL's host is this machine's.
func isLocalHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || localhost.IsSubdomain(h) {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// resolve maps a URL to where it is sent: itself when local, the API's
// local endpoint for a googleapis.com URL this instance serves. ok is
// false for anything else, with why.
func (h *HTTPCaller) resolve(raw string) (*url.URL, string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", raiseErr("ValueError", "url %q must be an absolute http or https URL", raw)
	}
	host := u.Hostname()
	if isLocalHost(host) {
		return u, "", nil
	}
	if api, ok := strings.CutSuffix(strings.ToLower(host), ".googleapis.com"); ok {
		// A regional host such as us-central1-run.googleapis.com names run.
		if i := strings.LastIndex(api, "-"); i >= 0 && h.Local != nil && h.Local(api) == "" && h.Local(api[i+1:]) != "" {
			api = api[i+1:]
		}
		if h.Local != nil {
			if base := h.Local(api); base != "" {
				b, err := url.Parse(base)
				if err != nil {
					return nil, "", raiseErr("SystemError", "bad local endpoint for %s: %v", api, err)
				}
				out := *u
				out.Scheme, out.Host = b.Scheme, b.Host
				return &out, api, nil
			}
		}
		return nil, api, raiseErr("ConnectionFailedError",
			"refused: %s.googleapis.com is not served by this CloudBurrow instance, and CloudBurrow sends no request to Google (start the instance with the service in --services)", api)
	}
	return nil, "", raiseErr("ConnectionFailedError",
		"refused: %s is outside this CloudBurrow instance, which runs locally and sends no request off the machine; call a local Cloud Run service or a served API instead", host)
}

// httpStep is http.get, post, put, patch, delete and request.
func (h *HTTPCaller) httpStep(ctx context.Context, verb string, args map[string]any) (any, error) {
	known := map[string]bool{"url": true, "method": verb == "request", "headers": true, "body": true, "query": true,
		"auth": true, "timeout": true, "private_service_name": true}
	for k := range args {
		if !known[k] {
			return nil, raiseErr("TypeError", "http.%s got an unexpected argument %q", verb, k)
		}
	}
	method := strings.ToUpper(verb)
	if verb == "request" {
		m, err := str(args["method"], "method")
		if err != nil {
			return nil, err
		}
		method = strings.ToUpper(m)
	}
	raw, err := str(args["url"], "url")
	if err != nil {
		return nil, err
	}
	if args["private_service_name"] != nil {
		return nil, raiseErr("ConnectionFailedError", "private_service_name (Service Directory) is not served by CloudBurrow")
	}
	timeout := float64(DefaultHTTPTimeout)
	if t, ok := toFloat(args["timeout"]); ok {
		if t <= 0 || t > MaxHTTPTimeout {
			return nil, raiseErr("ValueError", "timeout must be between 0 and %d seconds", MaxHTTPTimeout)
		}
		timeout = t
	}
	return h.send(ctx, method, raw, args["headers"], args["query"], args["body"], args["auth"], timeout, args["body"] != nil)
}

func (h *HTTPCaller) send(ctx context.Context, method, raw string, headers, query, body, auth any, timeout float64, hasBody bool) (any, error) {
	u, _, err := h.resolve(raw)
	if err != nil {
		return nil, err
	}
	if query != nil {
		qm, ok := query.(map[string]any)
		if !ok {
			return nil, raiseErr("TypeError", "query must be a map")
		}
		q := u.Query()
		for _, k := range sortedKeys(qm) {
			switch v := qm[k].(type) {
			case []any:
				for _, x := range v {
					s, _ := scalarString(x)
					q.Add(k, s)
				}
			default:
				s, ok := scalarString(v)
				if !ok {
					return nil, raiseErr("TypeError", "query value %q must be a scalar or a list", k)
				}
				q.Set(k, s)
			}
		}
		u.RawQuery = q.Encode()
	}
	hdr := http.Header{}
	if headers != nil {
		hm, ok := headers.(map[string]any)
		if !ok {
			return nil, raiseErr("TypeError", "headers must be a map")
		}
		for k, v := range hm {
			s, ok := scalarString(v)
			if !ok {
				return nil, raiseErr("TypeError", "header %q must be a string", k)
			}
			hdr.Set(k, s)
		}
	}
	var payload []byte
	if hasBody {
		ct := hdr.Get("Content-Type")
		switch b := body.(type) {
		case []byte:
			payload = b
		case string:
			if ct == "" || strings.HasPrefix(ct, "application/json") {
				payload, _ = encodeJSON(b, false)
				if ct == "" {
					hdr.Set("Content-Type", "application/json")
				}
			} else {
				payload = []byte(b)
			}
		default:
			if strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
				m, ok := b.(map[string]any)
				if !ok {
					return nil, raiseErr("TypeError", "a form body must be a map")
				}
				form := url.Values{}
				for k, v := range m {
					s, _ := scalarString(v)
					form.Set(k, s)
				}
				payload = []byte(form.Encode())
			} else {
				payload, err = encodeJSON(b, false)
				if err != nil {
					return nil, err
				}
				if ct == "" {
					hdr.Set("Content-Type", "application/json")
				}
			}
		}
	}
	if err := h.authorize(hdr, auth, raw); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()
	req, rerr := http.NewRequestWithContext(cctx, method, u.String(), bytes.NewReader(payload))
	if rerr != nil {
		return nil, raiseErr("ValueError", "bad request: %v", rerr)
	}
	req.Header = hdr
	client := h.Client
	if client == nil {
		client = NewHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		var eg *egressError
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.As(err, &eg):
			return nil, raiseErr("ConnectionFailedError", "%v", eg)
		case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
			return nil, raiseErr("TimeoutError", "the HTTP request to %s timed out after %v seconds", raw, timeout)
		}
		return nil, raiseErr("ConnectionFailedError", "connection to %s failed: %v", raw, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxHTTPResponseBytes+1))
	if err != nil {
		if cctx.Err() != nil && ctx.Err() == nil {
			return nil, raiseErr("TimeoutError", "the HTTP request to %s timed out after %v seconds", raw, timeout)
		}
		return nil, raiseErr("ConnectionError", "the connection to %s broke while reading the response: %v", raw, err)
	}
	if len(data) > MaxHTTPResponseBytes {
		return nil, raiseErr("ResourceLimitError", "the HTTP response is larger than %d bytes", MaxHTTPResponseBytes)
	}
	rh := map[string]any{}
	for k, v := range resp.Header {
		rh[k] = strings.Join(v, ", ")
	}
	out := map[string]any{"code": int64(resp.StatusCode), "headers": rh, "body": decodeBody(resp.Header.Get("Content-Type"), data)}
	if resp.StatusCode >= 400 {
		return nil, &wfError{val: map[string]any{
			"message": fmt.Sprintf("HTTP server responded with error code %d", resp.StatusCode),
			"code":    int64(resp.StatusCode), "headers": rh, "body": out["body"], "tags": []any{"HttpError"},
		}}
	}
	return out, nil
}

// decodeBody is a response body as Google decodes it: JSON parsed, text
// as a string, anything else as bytes.
func decodeBody(ct string, data []byte) any {
	if len(data) == 0 {
		return nil
	}
	mt, _, _ := mime.ParseMediaType(ct)
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		if v, err := decodeJSON(data); err == nil {
			return v
		}
		return string(data)
	case strings.HasPrefix(mt, "text/"):
		return string(data)
	}
	return data
}

// authorize adds the auth an http.* step or a connector asks for.
func (h *HTTPCaller) authorize(hdr http.Header, auth any, target string) error {
	if auth == nil {
		return nil
	}
	m, ok := auth.(map[string]any)
	if !ok {
		return raiseErr("TypeError", "auth must be a map with type OIDC or OAuth2")
	}
	typ, _ := m["type"].(string)
	if h.Tokens == nil {
		return raiseErr("AuthError", "this instance has no local credentials to mint a %s token", typ)
	}
	var tok string
	var err error
	switch typ {
	case "OIDC":
		aud, _ := m["audience"].(string)
		if aud == "" {
			u, _ := url.Parse(target)
			aud = u.Scheme + "://" + u.Host + u.Path
		}
		tok, err = h.Tokens.ServiceAccountIDToken(h.ServiceAccount, aud)
	case "OAuth2":
		scope := "https://www.googleapis.com/auth/cloud-platform"
		switch s := m["scopes"].(type) {
		case string:
			scope = s
		case []any:
			var parts []string
			for _, x := range s {
				if xs, ok := x.(string); ok {
					parts = append(parts, xs)
				}
			}
			scope = strings.Join(parts, " ")
		}
		if sc, ok := m["scope"].(string); ok && sc != "" {
			scope = sc
		}
		tok, err = h.Tokens.ServiceAccountAccessToken(h.ServiceAccount, scope)
	default:
		return raiseErr("ValueError", "auth type %q: use OIDC or OAuth2", typ)
	}
	if err != nil {
		return raiseErr("AuthError", "mint the %s token: %v", typ, err)
	}
	hdr.Set("Authorization", "Bearer "+tok)
	return nil
}

// ---- connectors ----

// connectorMethod is one connector call: the REST method it sends, by its
// discovery document's path. Path parameters are {name} (one segment) or
// {+name} (several); every other argument but body and connector_params is
// a query parameter.
type connectorMethod struct {
	httpMethod string
	path       string
	upload     string // the media upload path, used when uploadType=media
	// lro marks a method answering a long-running operation, which the
	// connector polls until it is done and answers its response.
	lro bool
}

// connectors are the connector methods of the APIs CloudBurrow serves
// (docs/reference/googleapis), each the API's discovery path.
var connectors = map[string]connectorMethod{
	// Cloud Storage JSON API v1.
	"storage.v1.buckets.get":     {"GET", "/storage/v1/b/{bucket}", "", false},
	"storage.v1.buckets.list":    {"GET", "/storage/v1/b", "", false},
	"storage.v1.buckets.insert":  {"POST", "/storage/v1/b", "", false},
	"storage.v1.buckets.delete":  {"DELETE", "/storage/v1/b/{bucket}", "", false},
	"storage.v1.buckets.patch":   {"PATCH", "/storage/v1/b/{bucket}", "", false},
	"storage.v1.objects.get":     {"GET", "/storage/v1/b/{bucket}/o/{object}", "", false},
	"storage.v1.objects.list":    {"GET", "/storage/v1/b/{bucket}/o", "", false},
	"storage.v1.objects.insert":  {"POST", "/storage/v1/b/{bucket}/o", "/upload/storage/v1/b/{bucket}/o", false},
	"storage.v1.objects.delete":  {"DELETE", "/storage/v1/b/{bucket}/o/{object}", "", false},
	"storage.v1.objects.patch":   {"PATCH", "/storage/v1/b/{bucket}/o/{object}", "", false},
	"storage.v1.objects.copy":    {"POST", "/storage/v1/b/{sourceBucket}/o/{sourceObject}/copyTo/b/{destinationBucket}/o/{destinationObject}", "", false},
	"storage.v1.objects.compose": {"POST", "/storage/v1/b/{destinationBucket}/o/{destinationObject}/compose", "", false},
	// Pub/Sub v1.
	"pubsub.v1.projects.topics.create":                   {"PUT", "/v1/{+name}", "", false},
	"pubsub.v1.projects.topics.get":                      {"GET", "/v1/{+topic}", "", false},
	"pubsub.v1.projects.topics.list":                     {"GET", "/v1/{+project}/topics", "", false},
	"pubsub.v1.projects.topics.delete":                   {"DELETE", "/v1/{+topic}", "", false},
	"pubsub.v1.projects.topics.publish":                  {"POST", "/v1/{+topic}:publish", "", false},
	"pubsub.v1.projects.subscriptions.create":            {"PUT", "/v1/{+name}", "", false},
	"pubsub.v1.projects.subscriptions.get":               {"GET", "/v1/{+subscription}", "", false},
	"pubsub.v1.projects.subscriptions.delete":            {"DELETE", "/v1/{+subscription}", "", false},
	"pubsub.v1.projects.subscriptions.pull":              {"POST", "/v1/{+subscription}:pull", "", false},
	"pubsub.v1.projects.subscriptions.acknowledge":       {"POST", "/v1/{+subscription}:acknowledge", "", false},
	"pubsub.v1.projects.subscriptions.modifyAckDeadline": {"POST", "/v1/{+subscription}:modifyAckDeadline", "", false},
	// Secret Manager v1.
	"secretmanager.v1.projects.secrets.create":          {"POST", "/v1/{+parent}/secrets", "", false},
	"secretmanager.v1.projects.secrets.get":             {"GET", "/v1/{+name}", "", false},
	"secretmanager.v1.projects.secrets.list":            {"GET", "/v1/{+parent}/secrets", "", false},
	"secretmanager.v1.projects.secrets.delete":          {"DELETE", "/v1/{+name}", "", false},
	"secretmanager.v1.projects.secrets.addVersion":      {"POST", "/v1/{+parent}:addVersion", "", false},
	"secretmanager.v1.projects.secrets.versions.access": {"GET", "/v1/{+name}:access", "", false},
	"secretmanager.v1.projects.secrets.versions.get":    {"GET", "/v1/{+name}", "", false},
	// Cloud Tasks v2.
	"cloudtasks.v2.projects.locations.queues.tasks.create": {"POST", "/v2/{+parent}/tasks", "", false},
	"cloudtasks.v2.projects.locations.queues.get":          {"GET", "/v2/{+name}", "", false},
	"cloudtasks.v2.projects.locations.queues.create":       {"POST", "/v2/{+parent}/queues", "", false},
	// Cloud Run Admin API v2.
	"run.v2.projects.locations.services.get":    {"GET", "/v2/{+name}", "", false},
	"run.v2.projects.locations.services.list":   {"GET", "/v2/{+parent}/services", "", false},
	"run.v2.projects.locations.services.create": {"POST", "/v2/{+parent}/services", "", true},
	"run.v2.projects.locations.services.patch":  {"PATCH", "/v2/{+name}", "", true},
	"run.v2.projects.locations.services.delete": {"DELETE", "/v2/{+name}", "", true},
	"run.v2.projects.locations.jobs.run":        {"POST", "/v2/{+name}:run", "", true},
	"run.v2.projects.locations.jobs.get":        {"GET", "/v2/{+name}", "", false},
	// Cloud Logging v2.
	"logging.v2.entries.write": {"POST", "/v2/entries:write", "", false},
	"logging.v2.entries.list":  {"POST", "/v2/entries:list", "", false},
	// Firestore v1.
	"firestore.v1.projects.databases.documents.get":            {"GET", "/v1/{+name}", "", false},
	"firestore.v1.projects.databases.documents.patch":          {"PATCH", "/v1/{+name}", "", false},
	"firestore.v1.projects.databases.documents.delete":         {"DELETE", "/v1/{+name}", "", false},
	"firestore.v1.projects.databases.documents.createDocument": {"POST", "/v1/{+parent}/{collectionId}", "", false},
	"firestore.v1.projects.databases.documents.list":           {"GET", "/v1/{+parent}/{collectionId}", "", false},
	// BigQuery v2.
	"bigquery.v2.jobs.query":          {"POST", "/bigquery/v2/projects/{projectId}/queries", "", false},
	"bigquery.v2.jobs.insert":         {"POST", "/bigquery/v2/projects/{projectId}/jobs", "", false},
	"bigquery.v2.jobs.get":            {"GET", "/bigquery/v2/projects/{projectId}/jobs/{jobId}", "", false},
	"bigquery.v2.datasets.get":        {"GET", "/bigquery/v2/projects/{projectId}/datasets/{datasetId}", "", false},
	"bigquery.v2.datasets.insert":     {"POST", "/bigquery/v2/projects/{projectId}/datasets", "", false},
	"bigquery.v2.tabledata.list":      {"GET", "/bigquery/v2/projects/{projectId}/datasets/{datasetId}/tables/{tableId}/data", "", false},
	"bigquery.v2.tabledata.insertAll": {"POST", "/bigquery/v2/projects/{projectId}/datasets/{datasetId}/tables/{tableId}/insertAll", "", false},
	// Cloud KMS v1.
	"cloudkms.v1.projects.locations.keyRings.cryptoKeys.encrypt": {"POST", "/v1/{+name}:encrypt", "", false},
	"cloudkms.v1.projects.locations.keyRings.cryptoKeys.decrypt": {"POST", "/v1/{+name}:decrypt", "", false},
	// Cloud Scheduler v1.
	"cloudscheduler.v1.projects.locations.jobs.get":    {"GET", "/v1/{+name}", "", false},
	"cloudscheduler.v1.projects.locations.jobs.create": {"POST", "/v1/{+parent}/jobs", "", false},
	"cloudscheduler.v1.projects.locations.jobs.run":    {"POST", "/v1/{+name}:run", "", false},
	// Workflow Executions v1.
	"workflowexecutions.v1.projects.locations.workflows.executions.create": {"POST", "/v1/{+parent}/executions", "", false},
	"workflowexecutions.v1.projects.locations.workflows.executions.get":    {"GET", "/v1/{+name}", "", false},
	"workflowexecutions.v1.projects.locations.workflows.executions.cancel": {"POST", "/v1/{+name}:cancel", "", false},
	// Workflows v1.
	"workflows.v1.projects.locations.workflows.get":  {"GET", "/v1/{+name}", "", false},
	"workflows.v1.projects.locations.workflows.list": {"GET", "/v1/{+parent}/workflows", "", false},
}

// ConnectorNames lists the connector methods served.
func ConnectorNames() []string {
	out := make([]string, 0, len(connectors))
	for k := range connectors {
		out = append(out, "googleapis."+k)
	}
	sort.Strings(out)
	return out
}

// connector calls googleapis.API.VERSION.RESOURCE.METHOD at the API's
// local endpoint, with the workflow's service account's token.
func (h *HTTPCaller) connector(ctx context.Context, name string, args map[string]any) (any, error) {
	full := strings.TrimPrefix(name, "googleapis.")
	api := strings.SplitN(full, ".", 2)[0]
	// secretmanager's accessString helper: the payload as a string.
	if full == "secretmanager.v1.projects.secrets.versions.accessString" {
		sid, _ := args["secret_id"].(string)
		proj, _ := args["project_id"].(string)
		ver, _ := args["version"].(string)
		if ver == "" {
			ver = "latest"
		}
		n := fmt.Sprintf("projects/%s/secrets/%s/versions/%s", proj, sid, ver)
		v, err := h.connector(ctx, "googleapis.secretmanager.v1.projects.secrets.versions.access", map[string]any{"name": n})
		if err != nil {
			return nil, err
		}
		data, _ := mapPath(v, "payload", "data").(string)
		b, err := stdlib["base64.decode"].call(nil, []any{data, true})
		if err != nil {
			return nil, err
		}
		return string(b.([]byte)), nil
	}
	base := ""
	if h.Local != nil {
		base = h.Local(api)
	}
	if base == "" {
		// Google answers a call to an API the project has not enabled with
		// 403 SERVICE_DISABLED; an API this instance does not serve is
		// refused the same way, and nothing is sent to Google.
		msg := fmt.Sprintf("%s.googleapis.com is not served by this CloudBurrow instance: start it with the service in --services. CloudBurrow sends no request to Google.", api)
		return nil, &wfError{val: map[string]any{
			"message": "HTTP server responded with error code 403", "code": int64(403), "headers": map[string]any{},
			"body": map[string]any{"error": map[string]any{"code": int64(403), "message": msg, "status": "PERMISSION_DENIED",
				"details": []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "SERVICE_DISABLED",
					"domain": "googleapis.com", "metadata": map[string]any{"service": api + ".googleapis.com"}}}}},
			"tags": []any{"HttpError"},
		}}
	}
	m, ok := connectors[full]
	if !ok {
		return nil, &wfError{val: map[string]any{
			"message": "HTTP server responded with error code 501", "code": int64(501), "headers": map[string]any{},
			"body": map[string]any{"error": map[string]any{"code": int64(501), "status": "UNIMPLEMENTED",
				"message": fmt.Sprintf("the connector %s is not implemented by CloudBurrow", name)}},
			"tags": []any{"HttpError"},
		}}
	}
	params := map[string]any{}
	var body any
	hasBody := false
	timeout := float64(MaxHTTPTimeout)
	skipPolling := false
	for k, v := range args {
		switch k {
		case "body":
			body, hasBody = v, true
		case "connector_params":
			cp, _ := v.(map[string]any)
			if t, ok := toFloat(cp["timeout"]); ok && t > 0 {
				timeout = t
			}
			if b, ok := cp["skip_polling"].(bool); ok {
				skipPolling = b
			}
		default:
			params[k] = v
		}
	}
	path := m.path
	hdr := map[string]any{}
	if m.upload != "" && params["uploadType"] == "media" {
		path = m.upload
		switch b := body.(type) {
		case string:
			body = []byte(b)
			hdr["Content-Type"] = "text/plain"
		case []byte:
			hdr["Content-Type"] = "application/octet-stream"
		default:
			enc, err := encodeJSON(b, false)
			if err != nil {
				return nil, err
			}
			body = enc
			hdr["Content-Type"] = "application/json"
		}
	}
	p, err := expandPath(path, params)
	if err != nil {
		return nil, err
	}
	var auth any
	if h.Tokens != nil {
		auth = map[string]any{"type": "OAuth2"}
	}
	resp, err := h.send(ctx, m.httpMethod, base+p, hdr, params, body, auth, timeout, hasBody)
	if err != nil {
		return nil, err
	}
	out := resp.(map[string]any)["body"]
	// Objects read with alt=media are their content.
	if m.lro && !skipPolling {
		return h.poll(ctx, base, out, timeout)
	}
	return out, nil
}

// poll waits for a long-running operation and answers its response, or
// raises its error as an OperationError.
func (h *HTTPCaller) poll(ctx context.Context, base string, op any, timeout float64) (any, error) {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		m, ok := op.(map[string]any)
		if !ok {
			return op, nil
		}
		if done, _ := m["done"].(bool); done {
			if e, ok := m["error"]; ok {
				return nil, &wfError{val: map[string]any{"message": "Long-running operation failed", "operation": m, "error": e, "tags": []any{"OperationError"}}}
			}
			return m["response"], nil
		}
		name, _ := m["name"].(string)
		if name == "" {
			return op, nil
		}
		if time.Now().After(deadline) {
			return nil, raiseErr("TimeoutError", "the operation %s did not finish in %v seconds", name, timeout)
		}
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		auth := map[string]any{"type": "OAuth2"}
		if h.Tokens == nil {
			auth = nil
		}
		resp, err := h.send(ctx, "GET", base+"/v2/"+name, nil, nil, nil, auth, MaxHTTPTimeout, false)
		if err != nil {
			return nil, err
		}
		op = resp.(map[string]any)["body"]
	}
}

func mapPath(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// expandPath fills a discovery path's parameters from args, removing them.
func expandPath(path string, args map[string]any) (string, error) {
	var b strings.Builder
	for {
		i := strings.Index(path, "{")
		if i < 0 {
			b.WriteString(path)
			return b.String(), nil
		}
		j := strings.Index(path[i:], "}")
		b.WriteString(path[:i])
		name := path[i+1 : i+j]
		reserved := strings.HasPrefix(name, "+")
		name = strings.TrimPrefix(name, "+")
		v, ok := args[name]
		if !ok {
			return "", raiseErr("TypeError", "the connector is missing the argument %q", name)
		}
		s, ok := scalarString(v)
		if !ok {
			return "", raiseErr("TypeError", "argument %q must be a string", name)
		}
		delete(args, name)
		if reserved {
			b.WriteString(s)
		} else {
			b.WriteString(url.PathEscape(s))
		}
		path = path[i+j+1:]
	}
}
