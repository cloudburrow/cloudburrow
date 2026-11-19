// Package apikeys serves the API Keys API v2 (apikeys.googleapis.com,
// #1915): each project's API keys, with their application restrictions
// (websites, IP addresses, Android apps, iOS apps) and API restrictions.
// gcloud `services api-keys` and Terraform's google_apikeys_key call it,
// and the console's APIs & Services > Credentials page lists and edits the
// keys.
//
// Keys are stored in the instance's store, so they persist exactly as the
// instance's other metadata does. Every change happens before the call
// returns, so each operation is returned done and operations.get answers it
// afterwards.
//
// A deleted key is kept for 30 days, as Google documents, and can be
// undeleted until then; afterwards it is purged, and keys.lookupKey answers
// its project with an empty name.
//
// Authorize is the decision the served APIs take on a request that carries
// a key (key= or x-goog-api-key): Google's API_KEY_INVALID for a key that
// does not exist, and API_KEY_SERVICE_BLOCKED or one of the application
// restriction reasons for a key whose restrictions refuse the request
// (check.go).
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Projects resolves the project a request names, by ID or by number.
type Projects interface {
	Resolve(idOrNumber string) (string, error)
}

// Restrictions are a key's restrictions, as the REST API's JSON names them.
type Restrictions struct {
	BrowserKeyRestrictions *BrowserKeyRestrictions `json:"browserKeyRestrictions,omitempty"`
	ServerKeyRestrictions  *ServerKeyRestrictions  `json:"serverKeyRestrictions,omitempty"`
	AndroidKeyRestrictions *AndroidKeyRestrictions `json:"androidKeyRestrictions,omitempty"`
	IosKeyRestrictions     *IosKeyRestrictions     `json:"iosKeyRestrictions,omitempty"`
	APITargets             []APITarget             `json:"apiTargets,omitempty"`
}

// BrowserKeyRestrictions are the websites (HTTP referrers) a key may be
// used from.
type BrowserKeyRestrictions struct {
	AllowedReferrers []string `json:"allowedReferrers,omitempty"`
}

// ServerKeyRestrictions are the caller IP addresses a key may be used
// from.
type ServerKeyRestrictions struct {
	AllowedIps []string `json:"allowedIps,omitempty"`
}

// AndroidKeyRestrictions are the Android apps a key may be used from.
type AndroidKeyRestrictions struct {
	AllowedApplications []AndroidApplication `json:"allowedApplications,omitempty"`
}

// AndroidApplication is one Android app: its package and its signing
// certificate's SHA-1 fingerprint.
type AndroidApplication struct {
	Sha1Fingerprint string `json:"sha1Fingerprint,omitempty"`
	PackageName     string `json:"packageName,omitempty"`
}

// IosKeyRestrictions are the iOS apps a key may be used from.
type IosKeyRestrictions struct {
	AllowedBundleIds []string `json:"allowedBundleIds,omitempty"`
}

// APITarget is one API (and optionally its methods) a key may call.
type APITarget struct {
	Service string   `json:"service,omitempty"`
	Methods []string `json:"methods,omitempty"`
}

// Key is one stored API key.
type Key struct {
	// Project is the owning project's ID; the resource name carries its
	// number.
	Project             string            `json:"project"`
	ID                  string            `json:"id"`
	UID                 string            `json:"uid"`
	DisplayName         string            `json:"displayName,omitempty"`
	KeyString           string            `json:"keyString"`
	CreateTime          time.Time         `json:"createTime"`
	UpdateTime          time.Time         `json:"updateTime"`
	DeleteTime          time.Time         `json:"deleteTime,omitzero"`
	Annotations         map[string]string `json:"annotations,omitempty"`
	Restrictions        *Restrictions     `json:"restrictions,omitempty"`
	ServiceAccountEmail string            `json:"serviceAccountEmail,omitempty"`
	// PreviousKey is the key this one was rotated from in the console
	// (Rotate key), which the key's page offers to delete. Not part of the
	// API's resource.
	PreviousKey string `json:"previousKey,omitempty"`
	// Purged is a key deleted more than 30 days ago: only its key string
	// and project are kept, for keys.lookupKey.
	Purged bool `json:"purged,omitempty"`
}

// Name is the key's resource name, under its project's number.
func (k *Key) Name() string {
	return fmt.Sprintf("projects/%d/locations/global/keys/%s", resource.ProjectNumber(k.Project), k.ID)
}

// Deleted reports whether the key is deleted (and not yet purged).
func (k *Key) Deleted() bool { return !k.DeleteTime.IsZero() }

// Etag is a checksum of the key's mutable state.
func (k *Key) Etag() string {
	b, _ := json.Marshal([]any{k.DisplayName, k.Annotations, k.Restrictions, k.UpdateTime.UnixNano(), k.DeleteTime.UnixNano()})
	sum := sha256.Sum256(b)
	return `W/"` + base64.StdEncoding.EncodeToString(sum[:18]) + `"`
}

// resourceJSON is the key as the REST API answers it. The key string is
// answered only by getKeyString, and to the create that made it.
func (k *Key) resourceJSON(withKeyString bool) map[string]any {
	out := map[string]any{
		"name":       k.Name(),
		"uid":        k.UID,
		"createTime": k.CreateTime.UTC().Format(time.RFC3339Nano),
		"updateTime": k.UpdateTime.UTC().Format(time.RFC3339Nano),
		"etag":       k.Etag(),
	}
	if k.DisplayName != "" {
		out["displayName"] = k.DisplayName
	}
	if withKeyString {
		out["keyString"] = k.KeyString
	}
	if k.Deleted() {
		out["deleteTime"] = k.DeleteTime.UTC().Format(time.RFC3339Nano)
	}
	if len(k.Annotations) > 0 {
		out["annotations"] = k.Annotations
	}
	if k.Restrictions != nil && !k.Restrictions.empty() {
		out["restrictions"] = k.Restrictions
	}
	if k.ServiceAccountEmail != "" {
		out["serviceAccountEmail"] = k.ServiceAccountEmail
	}
	return out
}

func (r *Restrictions) empty() bool {
	return r.BrowserKeyRestrictions == nil && r.ServerKeyRestrictions == nil && r.AndroidKeyRestrictions == nil &&
		r.IosKeyRestrictions == nil && len(r.APITargets) == 0
}

const (
	// Retention is how long a deleted key can be undeleted (30 days, as
	// Google documents), after which it is purged.
	Retention = 30 * 24 * time.Hour
	// MaxKeys is Google's system limit of API keys in a project.
	MaxKeys = 300
	// MaxApplicationRestrictions is Google's limit of application
	// restrictions on one key.
	MaxApplicationRestrictions = 1200
	maxDisplayName             = 63
	defaultPageSize            = 50
	maxPageSize                = 300
	keyPrefix                  = "apikeys/projects/"
	opType                     = "type.googleapis.com/google.api.apikeys.v2.Key"
)

// Server serves the API Keys API v2 over HTTP JSON.
type Server struct {
	addr     string
	st       store.Store
	projects Projects
	// Now is the clock, for tests of the 30-day retention.
	Now func() time.Time

	mu sync.Mutex
	// byString indexes every stored key (purged ones too) by key string,
	// to the store key it is under; nil until first built.
	byString map[string]string
	ops      map[string]map[string]any

	srv *http.Server
	ln  net.Listener
	// Observe, if set, is called once per request with the method name and
	// the answer's HTTP status.
	Observe func(method string, code int)
}

// NewServer serves at addr once started, keeping keys in st.
func NewServer(addr string, st store.Store, projects Projects) *Server {
	return &Server{addr: addr, st: st, projects: projects, Now: time.Now, ops: map[string]map[string]any{}}
}

func (s *Server) Name() string { return "apikeys" }

// Start binds the listener and serves in the background.
func (s *Server) Start(context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("apikeys: listen on %s: %w", s.addr, err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: hostguard.Wrap(s), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Addr is the bound address, empty before Start.
func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// apiError is an error with the HTTP status and Google status it answers.
type apiError struct {
	code   int
	status string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errorf(code int, status, format string, args ...any) error {
	return &apiError{code: code, status: status, msg: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return errorf(http.StatusBadRequest, "INVALID_ARGUMENT", format, args...)
}

func notFound(name string) error {
	return errorf(http.StatusNotFound, "NOT_FOUND", "Key %s not found.", name)
}

// ErrorStatus is err's HTTP status and Google status, for the console.
func ErrorStatus(err error) (int, string) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.code, ae.status
	}
	return http.StatusInternalServerError, "INTERNAL"
}

// ServeHTTP routes /v2/....
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, out, err := s.route(r)
	if err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			ae = &apiError{code: http.StatusInternalServerError, status: "INTERNAL", msg: err.Error()}
		}
		if s.Observe != nil {
			s.Observe(method, ae.code)
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(ae.code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": ae.code, "message": ae.msg, "status": ae.status,
		}})
		return
	}
	if s.Observe != nil {
		s.Observe(method, http.StatusOK)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) route(r *http.Request) (string, any, error) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s is not an API Keys API v2 path", r.URL.Path)
	}
	verb := ""
	if i := strings.LastIndex(path, ":"); i > strings.LastIndex(path, "/") {
		path, verb = path[:i], path[i+1:]
	}
	p := strings.Split(strings.Trim(path, "/"), "/")
	q := r.URL.Query()
	switch {
	case len(p) == 1 && p[0] == "keys" && verb == "lookupKey" && r.Method == http.MethodGet:
		out, err := s.LookupKey(q.Get("keyString"))
		return "apikeys.keys.lookupKey", out, err
	case len(p) == 2 && p[0] == "operations" && verb == "" && r.Method == http.MethodGet:
		out, err := s.operation("operations/" + p[1])
		return "apikeys.operations.get", out, err
	}
	// projects/{p}/locations/{l}/keys[/{k}[/keyString]]
	if len(p) < 5 || p[0] != "projects" || p[2] != "locations" || p[4] != "keys" || len(p) > 7 {
		return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s is not an API Keys API v2 path", r.URL.Path)
	}
	project, err := s.project(p[1])
	if err != nil {
		return "", nil, err
	}
	if p[3] != "global" {
		return "", nil, invalid("Location %s is not supported: keys are global, and the only supported location is global.", p[3])
	}
	if len(p) == 5 {
		switch {
		case verb == "" && r.Method == http.MethodPost:
			var k keyBody
			if err := decode(r, &k); err != nil {
				return "apikeys.projects.locations.keys.create", nil, err
			}
			out, err := s.create(project, q.Get("keyId"), k)
			return "apikeys.projects.locations.keys.create", out, err
		case verb == "" && r.Method == http.MethodGet:
			out, err := s.list(project, q)
			return "apikeys.projects.locations.keys.list", out, err
		}
		return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s %s is not an API Keys API v2 method", r.Method, r.URL.Path)
	}
	id := p[5]
	if len(p) == 7 {
		if p[6] == "keyString" && verb == "" && r.Method == http.MethodGet {
			k, err := s.Get(project, id)
			if err != nil {
				return "apikeys.projects.locations.keys.getKeyString", nil, err
			}
			return "apikeys.projects.locations.keys.getKeyString", map[string]any{"keyString": k.KeyString}, nil
		}
		return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s %s is not an API Keys API v2 method", r.Method, r.URL.Path)
	}
	switch {
	case verb == "" && r.Method == http.MethodGet:
		k, err := s.Get(project, id)
		if err != nil {
			return "apikeys.projects.locations.keys.get", nil, err
		}
		return "apikeys.projects.locations.keys.get", k.resourceJSON(false), nil
	case verb == "" && r.Method == http.MethodPatch:
		var k keyBody
		if err := decode(r, &k); err != nil {
			return "apikeys.projects.locations.keys.patch", nil, err
		}
		var mask []string
		if m := q.Get("updateMask"); m != "" {
			mask = strings.Split(m, ",")
		}
		out, err := s.patch(project, id, k, mask)
		return "apikeys.projects.locations.keys.patch", out, err
	case verb == "" && r.Method == http.MethodDelete:
		out, err := s.delete(project, id, q.Get("etag"))
		return "apikeys.projects.locations.keys.delete", out, err
	case verb == "undelete" && r.Method == http.MethodPost:
		if err := decode(r, &struct{}{}); err != nil {
			return "apikeys.projects.locations.keys.undelete", nil, err
		}
		out, err := s.undelete(project, id)
		return "apikeys.projects.locations.keys.undelete", out, err
	}
	return "", nil, errorf(http.StatusNotFound, "NOT_FOUND", "%s %s is not an API Keys API v2 method", r.Method, r.URL.Path)
}

// keyBody is a Key as a request carries it, with which fields were set.
type keyBody struct {
	Name                string            `json:"name"`
	DisplayName         *string           `json:"displayName"`
	Annotations         map[string]string `json:"annotations"`
	Restrictions        *Restrictions     `json:"restrictions"`
	Etag                string            `json:"etag"`
	ServiceAccountEmail string            `json:"serviceAccountEmail"`
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return invalid("Invalid JSON payload received. %v", err)
	}
	return nil
}

func (s *Server) project(idOrNumber string) (string, error) {
	if s.projects == nil {
		return idOrNumber, nil
	}
	return s.projects.Resolve(idOrNumber)
}

func storeKey(project, id string) string { return keyPrefix + project + "/keys/" + id }

func (s *Server) load(sk string) (*Key, error) {
	b, err := s.st.Get(sk)
	if err != nil {
		return nil, err
	}
	var k Key
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("apikeys: read %s: %w", sk, err)
	}
	return &k, nil
}

func (s *Server) save(k *Key) error {
	sk := storeKey(k.Project, k.ID)
	if !store.SafeKey(sk) {
		return invalid("project %q cannot be stored", k.Project)
	}
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	if err := s.st.Put(sk, b); err != nil {
		return err
	}
	if s.byString != nil {
		s.byString[k.KeyString] = sk
	}
	return nil
}

// index builds byString once; s.mu is held.
func (s *Server) index() error {
	if s.byString != nil {
		return nil
	}
	keys, err := s.st.List(keyPrefix)
	if err != nil {
		return err
	}
	idx := map[string]string{}
	for _, sk := range keys {
		k, err := s.load(sk)
		if err != nil {
			continue
		}
		idx[k.KeyString] = sk
	}
	s.byString = idx
	return nil
}

// purgeIfDue purges k when it was deleted more than Retention ago; s.mu is
// held.
func (s *Server) purgeIfDue(k *Key) error {
	if k.Purged || !k.Deleted() || s.Now().Sub(k.DeleteTime) < Retention {
		return nil
	}
	*k = Key{Project: k.Project, ID: k.ID, KeyString: k.KeyString, DeleteTime: k.DeleteTime, Purged: true}
	return s.save(k)
}

// projectKeys is every key of project not purged, oldest first; s.mu is
// held.
func (s *Server) projectKeys(project string) ([]*Key, error) {
	sks, err := s.st.List(keyPrefix + project + "/keys/")
	if err != nil {
		return nil, err
	}
	var out []*Key
	for _, sk := range sks {
		k, err := s.load(sk)
		if err != nil {
			return nil, err
		}
		if err := s.purgeIfDue(k); err != nil {
			return nil, err
		}
		if !k.Purged {
			out = append(out, k)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreateTime.Equal(out[j].CreateTime) {
			return out[i].CreateTime.Before(out[j].CreateTime)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// getLocked is a key not purged; s.mu is held.
func (s *Server) getLocked(project, id string) (*Key, error) {
	k, err := s.load(storeKey(project, id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(fmt.Sprintf("projects/%d/locations/global/keys/%s", resource.ProjectNumber(project), id))
	}
	if err != nil {
		return nil, err
	}
	if err := s.purgeIfDue(k); err != nil {
		return nil, err
	}
	if k.Purged {
		return nil, notFound(k.Name())
	}
	return k, nil
}

// Get is a project's key by ID, deleted or not.
func (s *Server) Get(project, id string) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(project, id)
}

// List is a project's keys, oldest first, with the deleted ones when
// showDeleted, for the console.
func (s *Server) List(project string, showDeleted bool) ([]*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.projectKeys(project)
	if err != nil {
		return nil, err
	}
	var out []*Key
	for _, k := range all {
		if showDeleted || !k.Deleted() {
			out = append(out, k)
		}
	}
	return out, nil
}

var (
	keyIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	uuidLike     = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)
	emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// newKeyString is a key string of Google's form: "AIza" and 35 more
// characters of the URL-safe base64 alphabet, 39 in all.
func newKeyString() string {
	b := make([]byte, 35)
	_, _ = rand.Read(b)
	out := []byte("AIza")
	for _, c := range b {
		out = append(out, keyAlphabet[int(c)%len(keyAlphabet)])
	}
	return string(out)
}

// Create makes a key in project, for the API and the console.
func (s *Server) Create(project, keyID, displayName string, r *Restrictions, annotations map[string]string, serviceAccount string) (*Key, error) {
	var dn *string
	if displayName != "" {
		dn = &displayName
	}
	return s.createKey(project, keyID, keyBody{DisplayName: dn, Restrictions: r, Annotations: annotations, ServiceAccountEmail: serviceAccount})
}

func (s *Server) createKey(project, keyID string, b keyBody) (*Key, error) {
	if keyID != "" {
		if !keyIDPattern.MatchString(keyID) || uuidLike.MatchString(keyID) {
			return nil, invalid("Invalid key_id %q: it must match [a-z]([a-z0-9-]{0,61}[a-z0-9])? and must not be a UUID.", keyID)
		}
	} else {
		keyID = newUUID()
	}
	k := &Key{Project: project, ID: keyID, UID: newUUID(), Annotations: b.Annotations,
		Restrictions: b.Restrictions, ServiceAccountEmail: b.ServiceAccountEmail}
	if b.DisplayName != nil {
		k.DisplayName = *b.DisplayName
	}
	if err := validate(k); err != nil {
		return nil, err
	}
	if k.ServiceAccountEmail != "" && !emailPattern.MatchString(k.ServiceAccountEmail) {
		return nil, invalid("Invalid service_account_email %q.", k.ServiceAccountEmail)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.index(); err != nil {
		return nil, err
	}
	if _, err := s.st.Get(storeKey(project, keyID)); err == nil {
		return nil, errorf(http.StatusConflict, "ALREADY_EXISTS", "Key projects/%d/locations/global/keys/%s already exists.", resource.ProjectNumber(project), keyID)
	}
	all, err := s.projectKeys(project)
	if err != nil {
		return nil, err
	}
	live := 0
	for _, o := range all {
		if !o.Deleted() {
			live++
		}
	}
	if live >= MaxKeys {
		return nil, errorf(http.StatusTooManyRequests, "RESOURCE_EXHAUSTED",
			"Project %s already has %d API keys, the most a project can have.", project, MaxKeys)
	}
	for {
		k.KeyString = newKeyString()
		if _, dup := s.byString[k.KeyString]; !dup {
			break
		}
	}
	now := s.Now().UTC()
	k.CreateTime, k.UpdateTime = now, now
	if err := s.save(k); err != nil {
		return nil, err
	}
	return k, nil
}

func (s *Server) create(project, keyID string, b keyBody) (any, error) {
	k, err := s.createKey(project, keyID, b)
	if err != nil {
		return nil, err
	}
	// The create's answer carries the new key string, as Google's does.
	return s.done(k, true), nil
}

// validate checks a key's display name and restrictions as Google
// documents them.
func validate(k *Key) error {
	if len([]rune(k.DisplayName)) > maxDisplayName {
		return invalid("display_name must be at most %d characters.", maxDisplayName)
	}
	r := k.Restrictions
	if r == nil {
		return nil
	}
	kinds := 0
	apps := 0
	if b := r.BrowserKeyRestrictions; b != nil {
		kinds++
		apps += len(b.AllowedReferrers)
		for _, ref := range b.AllowedReferrers {
			if err := validReferrer(ref); err != nil {
				return err
			}
		}
	}
	if sv := r.ServerKeyRestrictions; sv != nil {
		kinds++
		apps += len(sv.AllowedIps)
		for _, ip := range sv.AllowedIps {
			if _, err := parseIPOrCIDR(ip); err != nil {
				return invalid("Invalid IP address or subnet %q in allowed_ips.", ip)
			}
		}
	}
	if a := r.AndroidKeyRestrictions; a != nil {
		kinds++
		apps += len(a.AllowedApplications)
		for i := range a.AllowedApplications {
			app := &a.AllowedApplications[i]
			if !androidPackage.MatchString(app.PackageName) {
				return invalid("Invalid Android package name %q.", app.PackageName)
			}
			fp, ok := normalizeSHA1(app.Sha1Fingerprint)
			if !ok {
				return invalid("Invalid SHA-1 certificate fingerprint %q: it must be 20 bytes, in hex, with or without colons.", app.Sha1Fingerprint)
			}
			app.Sha1Fingerprint = fp
		}
	}
	if i := r.IosKeyRestrictions; i != nil {
		kinds++
		apps += len(i.AllowedBundleIds)
		for _, id := range i.AllowedBundleIds {
			if !iosBundle.MatchString(id) {
				return invalid("Invalid iOS bundle ID %q.", id)
			}
		}
	}
	if kinds > 1 {
		return invalid("Only one type of application restriction (browser_key_restrictions, server_key_restrictions, android_key_restrictions or ios_key_restrictions) can be set on a key.")
	}
	if apps > MaxApplicationRestrictions {
		return invalid("A key can have at most %d application restrictions, got %d.", MaxApplicationRestrictions, apps)
	}
	for _, t := range r.APITargets {
		if strings.TrimSpace(t.Service) == "" {
			return invalid("api_targets.service is required.")
		}
		for _, m := range t.Methods {
			if m == "" || strings.Contains(strings.TrimSuffix(m, "*"), "*") {
				return invalid("Invalid method %q in api_targets: a wildcard (*) is allowed only as the last character.", m)
			}
		}
	}
	return nil
}

var (
	androidPackage = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z0-9_]+)*$`)
	iosBundle      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	hex40          = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// normalizeSHA1 is a fingerprint in the plain hex form Google answers,
// from either form it accepts.
func normalizeSHA1(fp string) (string, bool) {
	plain := strings.ReplaceAll(fp, ":", "")
	if strings.Contains(fp, ":") && len(fp) != 59 {
		return "", false
	}
	if !hex40.MatchString(plain) {
		return "", false
	}
	return plain, true
}

func parseIPOrCIDR(s string) (*net.IPNet, error) {
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		return n, err
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("not an IP address")
	}
	bits := 128
	if ip.To4() != nil {
		ip, bits = ip.To4(), 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

// validReferrer accepts the website restrictions Google documents: a URL
// with an optional scheme and port, where * can stand for a subdomain (a
// leading "*.") or a path, but not for part of the middle of a URL.
func validReferrer(ref string) error {
	if _, err := parseReferrerPattern(ref); err != nil {
		return invalid("Invalid website restriction %q: %v.", ref, err)
	}
	return nil
}

// list answers keys.list, paged by offset.
func (s *Server) list(project string, q url.Values) (any, error) {
	size := defaultPageSize
	if v := q.Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, invalid("Invalid page_size %q.", v)
		}
		if n > 0 {
			size = min(n, maxPageSize)
		}
	}
	start := 0
	if tok := q.Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 {
			return nil, invalid("Invalid page_token %q.", tok)
		}
		start = n
	}
	show := false
	if v := q.Get("showDeleted"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, invalid("Invalid show_deleted %q.", v)
		}
		show = b
	}
	keys, err := s.List(project, show)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if start < len(keys) {
		end := min(start+size, len(keys))
		page := make([]any, 0, end-start)
		for _, k := range keys[start:end] {
			page = append(page, k.resourceJSON(false))
		}
		out["keys"] = page
		if end < len(keys) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
	}
	return out, nil
}

var mutable = map[string]bool{"displayName": true, "display_name": true, "restrictions": true, "annotations": true}

// Update changes a key's display name and restrictions, for the console.
func (s *Server) Update(project, id, displayName string, r *Restrictions) (*Key, error) {
	return s.update(project, id, keyBody{DisplayName: &displayName, Restrictions: r}, []string{"displayName", "restrictions"})
}

func (s *Server) update(project, id string, b keyBody, mask []string) (*Key, error) {
	fields := map[string]bool{}
	if len(mask) == 0 {
		// The implied mask: every mutable field set on the wire.
		if b.DisplayName != nil {
			fields["displayName"] = true
		}
		if b.Restrictions != nil {
			fields["restrictions"] = true
		}
		if b.Annotations != nil {
			fields["annotations"] = true
		}
	}
	for _, m := range mask {
		m = strings.TrimSpace(m)
		switch {
		case m == "*":
			fields["displayName"], fields["restrictions"], fields["annotations"] = true, true, true
		case m == "display_name":
			fields["displayName"] = true
		case strings.HasPrefix(m, "restrictions.") || m == "restrictions":
			fields["restrictions"] = true
		case mutable[m]:
			fields[m] = true
		default:
			return nil, invalid("Invalid update_mask path %q: only display_name, restrictions and annotations can be updated.", m)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.getLocked(project, id)
	if err != nil {
		return nil, err
	}
	if k.Deleted() {
		return nil, errorf(http.StatusBadRequest, "FAILED_PRECONDITION", "Key %s is deleted; undelete it before updating it.", k.Name())
	}
	if b.Etag != "" && b.Etag != k.Etag() {
		return nil, errorf(http.StatusConflict, "ABORTED", "The etag %s does not match the key's current etag %s.", b.Etag, k.Etag())
	}
	next := *k
	if fields["displayName"] {
		next.DisplayName = ""
		if b.DisplayName != nil {
			next.DisplayName = *b.DisplayName
		}
	}
	if fields["restrictions"] {
		next.Restrictions = b.Restrictions
	}
	if fields["annotations"] {
		next.Annotations = b.Annotations
	}
	if err := validate(&next); err != nil {
		return nil, err
	}
	next.UpdateTime = s.Now().UTC()
	if err := s.save(&next); err != nil {
		return nil, err
	}
	return &next, nil
}

func (s *Server) patch(project, id string, b keyBody, mask []string) (any, error) {
	k, err := s.update(project, id, b, mask)
	if err != nil {
		return nil, err
	}
	return s.done(k, false), nil
}

// Delete deletes a key, for the console: it can be undeleted for 30 days.
func (s *Server) Delete(project, id string) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(project, id, "")
}

func (s *Server) deleteLocked(project, id, etag string) (*Key, error) {
	k, err := s.getLocked(project, id)
	if err != nil {
		return nil, err
	}
	if etag != "" && etag != k.Etag() {
		return nil, errorf(http.StatusConflict, "ABORTED", "The etag %s does not match the key's current etag %s.", etag, k.Etag())
	}
	if !k.Deleted() {
		k.DeleteTime = s.Now().UTC()
		if err := s.save(k); err != nil {
			return nil, err
		}
	}
	return k, nil
}

func (s *Server) delete(project, id, etag string) (any, error) {
	s.mu.Lock()
	k, err := s.deleteLocked(project, id, etag)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.done(k, false), nil
}

// Undelete restores a key deleted within the last 30 days.
func (s *Server) Undelete(project, id string) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.getLocked(project, id)
	if err != nil {
		return nil, err
	}
	if !k.Deleted() {
		return nil, errorf(http.StatusBadRequest, "FAILED_PRECONDITION", "Key %s is not deleted.", k.Name())
	}
	k.DeleteTime = time.Time{}
	k.UpdateTime = s.Now().UTC()
	if err := s.save(k); err != nil {
		return nil, err
	}
	return k, nil
}

func (s *Server) undelete(project, id string) (any, error) {
	k, err := s.Undelete(project, id)
	if err != nil {
		return nil, err
	}
	return s.done(k, false), nil
}

// Rotate is the console's Rotate key: a new key with the old one's
// restrictions, named displayName, which remembers the old key so its
// page can offer to delete it. The old key keeps working until deleted.
func (s *Server) Rotate(project, id, displayName string) (*Key, error) {
	old, err := s.Get(project, id)
	if err != nil {
		return nil, err
	}
	if old.Deleted() {
		return nil, errorf(http.StatusBadRequest, "FAILED_PRECONDITION", "Key %s is deleted.", old.Name())
	}
	var r *Restrictions
	if old.Restrictions != nil {
		b, _ := json.Marshal(old.Restrictions)
		r = &Restrictions{}
		_ = json.Unmarshal(b, r)
	}
	k, err := s.Create(project, "", displayName, r, old.Annotations, old.ServiceAccountEmail)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k.PreviousKey = old.ID
	return k, s.save(k)
}

// ClearPrevious forgets a rotated key's previous key, once it is deleted.
func (s *Server) ClearPrevious(project, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.getLocked(project, id)
	if err != nil {
		return err
	}
	k.PreviousKey = ""
	return s.save(k)
}

// Lookup finds a key by its key string: its project and the key, which is
// nil (with ok false) for a key string no live key has. A deleted key is
// found with ok false: it no longer authorizes anything.
func (s *Server) Lookup(keyString string) (project string, key *Key, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index() != nil {
		return "", nil, false
	}
	sk, found := s.byString[keyString]
	if !found {
		return "", nil, false
	}
	k, err := s.load(sk)
	if err != nil {
		return "", nil, false
	}
	if s.purgeIfDue(k) != nil {
		return "", nil, false
	}
	if k.Purged || k.Deleted() {
		return k.Project, nil, false
	}
	return k.Project, k, true
}

// LookupKey answers keys.lookupKey: the project that owns the key string,
// and the key's name, empty once the key is purged.
func (s *Server) LookupKey(keyString string) (map[string]any, error) {
	if keyString == "" {
		return nil, invalid("key_string is required.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.index(); err != nil {
		return nil, err
	}
	sk, found := s.byString[keyString]
	if !found {
		return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Key not found.")
	}
	k, err := s.load(sk)
	if err != nil {
		return nil, err
	}
	if err := s.purgeIfDue(k); err != nil {
		return nil, err
	}
	out := map[string]any{"parent": fmt.Sprintf("projects/%d/locations/global", resource.ProjectNumber(k.Project))}
	if !k.Purged {
		out["name"] = k.Name()
	}
	return out, nil
}

// done records a finished operation whose response is k.
func (s *Server) done(k *Key, withKeyString bool) map[string]any {
	name := fmt.Sprintf("operations/akmf.p7-%d-%s", resource.ProjectNumber(k.Project), newUUID())
	resp := k.resourceJSON(withKeyString)
	resp["@type"] = opType
	op := map[string]any{"name": name, "done": true, "response": resp}
	s.mu.Lock()
	s.ops[name] = op
	s.mu.Unlock()
	return op
}

func (s *Server) operation(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[name]; ok {
		return op, nil
	}
	return nil, errorf(http.StatusNotFound, "NOT_FOUND", "Operation %s not found.", name)
}

// ResetProject forgets a project's keys and the operations that made them,
// or every project's for "": /admin/reset's, and a purged project's.
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := keyPrefix
	if project != "" {
		prefix = keyPrefix + project + "/keys/"
	}
	sks, err := s.st.List(prefix)
	if err != nil {
		return err
	}
	for _, sk := range sks {
		if err := s.st.Delete(sk); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	s.byString = nil
	opPrefix := "operations/akmf.p7-"
	if project != "" {
		opPrefix += strconv.FormatInt(resource.ProjectNumber(project), 10) + "-"
	}
	for n := range s.ops {
		if strings.HasPrefix(n, opPrefix) {
			delete(s.ops, n)
		}
	}
	return nil
}

// Method is one method of the v2 discovery document.
type Method struct {
	ID          string
	HTTPMethod  string
	Path        string
	Implemented bool
}

// Methods is every method of apikeys v2 (google.golang.org/api/apikeys/v2's
// discovery document). The coverage registry reads it.
var Methods = []Method{
	{"apikeys.keys.lookupKey", "GET", "v2/keys:lookupKey", true},
	{"apikeys.operations.get", "GET", "v2/operations/x", true},
	{"apikeys.projects.locations.keys.create", "POST", "v2/projects/p/locations/global/keys", true},
	{"apikeys.projects.locations.keys.delete", "DELETE", "v2/projects/p/locations/global/keys/k", true},
	{"apikeys.projects.locations.keys.get", "GET", "v2/projects/p/locations/global/keys/k", true},
	{"apikeys.projects.locations.keys.getKeyString", "GET", "v2/projects/p/locations/global/keys/k/keyString", true},
	{"apikeys.projects.locations.keys.list", "GET", "v2/projects/p/locations/global/keys", true},
	{"apikeys.projects.locations.keys.patch", "PATCH", "v2/projects/p/locations/global/keys/k", true},
	{"apikeys.projects.locations.keys.undelete", "POST", "v2/projects/p/locations/global/keys/k:undelete", true},
}

// MethodStatuses maps each method ID to whether it is implemented.
func MethodStatuses() map[string]bool {
	out := make(map[string]bool, len(Methods))
	for _, m := range Methods {
		out[m.ID] = m.Implemented
	}
	return out
}
