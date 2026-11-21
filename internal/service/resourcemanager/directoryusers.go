package resourcemanager

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The customer's users (#2103): the Google Accounts a dynamic group's
// query is evaluated over. Google keeps them in the Admin SDK's Directory
// API (admin.googleapis.com/admin/directory/v1/users), whose User resource
// the dynamic group attributes are read from: "The attributes you can use
// in dynamic group queries are based on the Admin SDK User resource"
// (https://docs.cloud.google.com/identity/docs/how-to/dynamic-groups-attributes,
// read 2026-11-21). Served here at the Directory API's paths on Resource
// Manager's port: users insert, get, list, update, patch and delete, in the
// shapes of google.golang.org/api/admin/directory/v1. A user is stored as
// its JSON object, so every attribute the resource documents is kept and
// returned as written; the password is kept as a hash and never returned
// ("the API never returns the password", Users reference,
// https://developers.google.com/workspace/admin/directory/reference/rest/v1/users).

const (
	userPrefix = "resourcemanager/cloudidentity/users/"
	// RootOrgUnitID is the ID of the customer's top organizational unit,
	// orgUnitPath "/".
	RootOrgUnitID = "03cloudburrowroot"
	// minPassword and maxPassword are the Directory API's password limits:
	// "A password can contain any combination of ASCII characters, and must
	// be between 8-100 characters."
	minPassword = 8
	maxPassword = 100
	// maxNameLen is a given or family name's limit: "Maximum allowed data
	// size for this field is 60 characters."
	maxNameLen = 60
)

// DirectoryUser is one stored user: its JSON object (without the
// password) and the password's hash.
type DirectoryUser struct {
	Object       map[string]any `json:"object"`
	PasswordHash string         `json:"passwordHash,omitempty"`
}

// ID is the user's unique ID.
func (u *DirectoryUser) ID() string { s, _ := u.Object["id"].(string); return s }

// Email is the user's primary email, lower case.
func (u *DirectoryUser) Email() string {
	s, _ := u.Object["primaryEmail"].(string)
	return normEmail(s)
}

// OrgUnitPath is the user's organizational unit, "/" by default.
func (u *DirectoryUser) OrgUnitPath() string {
	if s, _ := u.Object["orgUnitPath"].(string); s != "" {
		return s
	}
	return "/"
}

// OrgUnitID is the ID of the organizational unit at path: RootOrgUnitID
// for "/", and for any other path an ID derived from it, as the instance
// keeps no organizational units of its own.
func OrgUnitID(path string) string {
	path = "/" + strings.Trim(path, "/")
	if path == "/" {
		return RootOrgUnitID
	}
	sum := sha256.Sum256([]byte(strings.ToLower(path)))
	return "03" + hex.EncodeToString(sum[:])[:14]
}

func newUserID() string {
	// A Directory user ID is a 21-digit number.
	n, _ := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
	return "1" + fmt.Sprintf("%020d", n)
}

func dirNotFound() error {
	return apierror.NotFound("Resource Not Found: userKey")
}

func (r *Registry) usersLocked() ([]*DirectoryUser, error) {
	keys, err := r.st.List(userPrefix)
	if err != nil {
		return nil, apierror.Internal(err, "list users")
	}
	var out []*DirectoryUser
	for _, k := range keys {
		b, err := r.st.Get(k)
		if err != nil {
			continue
		}
		var u DirectoryUser
		if err := json.Unmarshal(b, &u); err != nil {
			return nil, apierror.Internal(err, "decode user")
		}
		out = append(out, &u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email() < out[j].Email() })
	return out, nil
}

// userLocked is the user whose ID, primary email or alias is key.
func (r *Registry) userLocked(key string) (*DirectoryUser, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, dirNotFound()
	}
	if b, err := r.st.Get(userPrefix + key); err == nil {
		var u DirectoryUser
		if err := json.Unmarshal(b, &u); err != nil {
			return nil, apierror.Internal(err, "decode user")
		}
		return &u, nil
	}
	us, err := r.usersLocked()
	if err != nil {
		return nil, err
	}
	for _, u := range us {
		if u.Email() == normEmail(key) {
			return u, nil
		}
		if al, ok := u.Object["aliases"].([]any); ok {
			for _, a := range al {
				if s, _ := a.(string); normEmail(s) == normEmail(key) {
					return u, nil
				}
			}
		}
	}
	return nil, dirNotFound()
}

func (r *Registry) putUserLocked(u *DirectoryUser) error {
	b, err := json.Marshal(u)
	if err != nil {
		return apierror.Internal(err, "encode user")
	}
	if err := r.st.Put(userPrefix+u.ID(), b); err != nil {
		return apierror.Internal(err, "store user")
	}
	return nil
}

// Users are the customer's users, by primary email.
func (r *Registry) Users() ([]*DirectoryUser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usersLocked()
}

// User is the user whose ID or primary email is key.
func (r *Registry) User(key string) (*DirectoryUser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.userLocked(key)
}

// readOnlyUserFields are the User fields the server sets.
var readOnlyUserFields = []string{"kind", "etag", "id", "customerId", "creationTime", "lastLoginTime", "isAdmin",
	"isDelegatedAdmin", "agreedToTerms", "isMailboxSetup", "isEnrolledIn2Sv", "isEnforcedIn2Sv", "nonEditableAliases",
	"deletionTime", "suspensionReason", "thumbnailPhotoUrl", "thumbnailPhotoEtag", "aliases"}

func checkUserName(obj map[string]any, insert bool) error {
	name, _ := obj["name"].(map[string]any)
	if name == nil {
		if insert {
			return apierror.InvalidArgument("Invalid Given/Family Name: name is required.")
		}
		return nil
	}
	for _, k := range []string{"givenName", "familyName"} {
		v, _ := name[k].(string)
		if strings.TrimSpace(v) == "" && insert {
			return apierror.InvalidArgument("Invalid Given/Family Name: %s", k)
		}
		if utf8.RuneCountInString(v) > maxNameLen {
			return apierror.InvalidArgument("Invalid Given/Family Name: %s must not be longer than %d characters.", k, maxNameLen)
		}
	}
	return nil
}

func passwordHash(p string) (string, error) {
	if n := utf8.RuneCountInString(p); n < minPassword || n > maxPassword {
		return "", apierror.InvalidArgument("Invalid Password: a password must be between %d and %d characters.", minPassword, maxPassword)
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:]), nil
}

// fillUser sets the server's fields of a user object.
func (r *Registry) fillUser(u *DirectoryUser) {
	o := u.Object
	o["kind"] = "admin#directory#user"
	o["customerId"] = OrganizationCustomerID
	email := u.Email()
	o["primaryEmail"] = email
	if s, _ := o["orgUnitPath"].(string); s == "" {
		o["orgUnitPath"] = "/"
	}
	if name, ok := o["name"].(map[string]any); ok {
		g, _ := name["givenName"].(string)
		f, _ := name["familyName"].(string)
		name["fullName"] = strings.TrimSpace(g + " " + f)
	}
	for _, k := range []string{"suspended", "archived", "changePasswordAtNextLogin", "ipWhitelisted", "isAdmin",
		"isDelegatedAdmin", "isEnrolledIn2Sv", "isEnforcedIn2Sv"} {
		if _, ok := o[k].(bool); !ok {
			o[k] = false
		}
	}
	if _, ok := o["includeInGlobalAddressList"].(bool); !ok {
		o["includeInGlobalAddressList"] = true
	}
	o["agreedToTerms"] = true
	o["isMailboxSetup"] = true
	// emails holds the primary address.
	emails, _ := o["emails"].([]any)
	has := false
	for _, e := range emails {
		if m, ok := e.(map[string]any); ok && normEmail(fmt.Sprint(m["address"])) == email {
			m["primary"] = true
			has = true
		}
	}
	if !has {
		emails = append([]any{map[string]any{"address": email, "primary": true}}, emails...)
	}
	o["emails"] = emails
	sum := sha256.Sum256([]byte(fmt.Sprint(o["id"], r.now().UnixNano())))
	o["etag"] = `"` + hex.EncodeToString(sum[:12]) + `"`
}

// InsertUser creates a user from its JSON object.
func (r *Registry) InsertUser(obj map[string]any) (*DirectoryUser, error) {
	if obj == nil {
		obj = map[string]any{}
	}
	email := normEmail(fmt.Sprint(obj["primaryEmail"]))
	if obj["primaryEmail"] == nil || email == "" {
		return nil, apierror.InvalidArgument("Invalid Input: primary_user_email")
	}
	if !validEmail(email) {
		return nil, apierror.InvalidArgument("Invalid Input: primary_user_email")
	}
	if _, d, _ := strings.Cut(email, "@"); d != OrganizationDomain {
		return nil, apierror.InvalidArgument("Domain not found: a user's primary email must be in the customer's domain, %s.", OrganizationDomain)
	}
	if err := checkUserName(obj, true); err != nil {
		return nil, err
	}
	pw, _ := obj["password"].(string)
	hash, err := passwordHash(pw)
	if err != nil {
		return nil, err
	}
	delete(obj, "password")
	delete(obj, "hashFunction")
	for _, k := range readOnlyUserFields {
		delete(obj, k)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.userLocked(email); err == nil {
		return nil, apierror.AlreadyExists("Entity already exists.")
	}
	if g, err := r.groupByEmailLocked(email); err != nil {
		return nil, err
	} else if g != nil {
		return nil, apierror.AlreadyExists("Entity already exists.")
	}
	obj["id"] = newUserID()
	obj["creationTime"] = r.now().UTC().Format("2006-01-02T15:04:05.000Z")
	u := &DirectoryUser{Object: obj, PasswordHash: hash}
	u.Object["primaryEmail"] = email
	r.fillUser(u)
	return u, r.putUserLocked(u)
}

// UpdateUser changes a user: patch merges obj's top-level fields into it;
// update replaces every field obj names and keeps the others, as the
// Directory API's update does ("only the fields in the request are
// updated").
func (r *Registry) UpdateUser(key string, obj map[string]any) (*DirectoryUser, error) {
	if err := checkUserName(obj, false); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	u, err := r.userLocked(key)
	if err != nil {
		return nil, err
	}
	if pw, ok := obj["password"].(string); ok {
		h, err := passwordHash(pw)
		if err != nil {
			return nil, err
		}
		u.PasswordHash = h
	}
	delete(obj, "password")
	delete(obj, "hashFunction")
	for _, k := range readOnlyUserFields {
		delete(obj, k)
	}
	if v, ok := obj["primaryEmail"]; ok {
		email := normEmail(fmt.Sprint(v))
		if !validEmail(email) {
			return nil, apierror.InvalidArgument("Invalid Input: primary_user_email")
		}
		if _, d, _ := strings.Cut(email, "@"); d != OrganizationDomain {
			return nil, apierror.InvalidArgument("Domain not found: a user's primary email must be in the customer's domain, %s.", OrganizationDomain)
		}
		if email != u.Email() {
			if o, err := r.userLocked(email); err == nil && o.ID() != u.ID() {
				return nil, apierror.AlreadyExists("Entity already exists.")
			}
			// The old address stays the user's alias.
			al, _ := u.Object["aliases"].([]any)
			u.Object["aliases"] = append(al, u.Email())
		}
		obj["primaryEmail"] = email
	}
	for k, v := range obj {
		if k == "name" {
			if nm, ok := v.(map[string]any); ok {
				cur, _ := u.Object["name"].(map[string]any)
				if cur == nil {
					cur = map[string]any{}
				}
				for nk, nv := range nm {
					cur[nk] = nv
				}
				u.Object["name"] = cur
				continue
			}
		}
		if v == nil {
			delete(u.Object, k)
			continue
		}
		u.Object[k] = v
	}
	r.fillUser(u)
	return u, r.putUserLocked(u)
}

// DeleteUser deletes a user, and its direct memberships of groups.
func (r *Registry) DeleteUser(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, err := r.userLocked(key)
	if err != nil {
		return err
	}
	gs, err := r.groupsLocked()
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.DynamicQuery != "" {
			continue
		}
		n := len(g.Members)
		g.Members = deleteMembersFunc(g.Members, func(m GroupMember) bool { return m.Email == u.Email() && m.Namespace == "" })
		if len(g.Members) != n {
			if err := r.putGroupLocked(g); err != nil {
				return err
			}
		}
	}
	if err := r.st.Delete(userPrefix + u.ID()); err != nil {
		return apierror.Internal(err, "delete user")
	}
	return nil
}

func deleteMembersFunc(ms []GroupMember, del func(GroupMember) bool) []GroupMember {
	out := ms[:0]
	for _, m := range ms {
		if !del(m) {
			out = append(out, m)
		}
	}
	return out
}

// IsDirectoryPath is whether path is the Directory API's users'.
func IsDirectoryPath(path string) bool {
	return path == "/admin/directory/v1/users" || strings.HasPrefix(path, "/admin/directory/v1/users/")
}

func decodeObject(req *http.Request) (map[string]any, error) {
	var obj map[string]any
	if err := rest.DecodeJSON(req, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

// directoryRoutes registers the Directory API's users.
func (r *Registry) directoryRoutes(rt *rest.Router) {
	rt.Handle("POST /admin/directory/v1/users", func(w http.ResponseWriter, req *http.Request) error {
		obj, err := decodeObject(req)
		if err != nil {
			return err
		}
		u, err := r.InsertUser(obj)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, u.Object)
	})
	rt.Handle("GET /admin/directory/v1/users", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		customer, domain := q.Get("customer"), q.Get("domain")
		if customer == "" && domain == "" {
			return apierror.InvalidArgument("Bad Request: customer or domain is required.")
		}
		if customer != "" && customer != "my_customer" && customer != OrganizationCustomerID {
			return apierror.InvalidArgument("Bad Request: the customer is %s (my_customer).", OrganizationCustomerID)
		}
		if domain != "" && normEmail(domain) != OrganizationDomain {
			return apierror.NotFound("Domain not found.")
		}
		if q.Get("query") != "" {
			return apierror.Unimplemented("Not implemented here: users.list's query; list every user and filter, or use a dynamic group's query.")
		}
		size := 100
		if s := q.Get("maxResults"); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || n > 500 {
				return apierror.InvalidArgument("Invalid value for maxResults: %s; it must be between 1 and 500.", s)
			}
			size = n
		}
		us, err := r.Users()
		if err != nil {
			return err
		}
		key := func(u *DirectoryUser) string { return u.Email() }
		switch ob := q.Get("orderBy"); ob {
		case "", "email":
		case "givenName", "familyName":
			key = func(u *DirectoryUser) string {
				nm, _ := u.Object["name"].(map[string]any)
				s, _ := nm[ob].(string)
				return strings.ToLower(s) + "\x00" + u.Email()
			}
		default:
			return apierror.InvalidArgument("Invalid value for orderBy: %s", ob)
		}
		sort.SliceStable(us, func(i, j int) bool { return key(us[i]) < key(us[j]) })
		if q.Get("sortOrder") == "DESCENDING" {
			for i, j := 0, len(us)-1; i < j; i, j = i+1, j-1 {
				us[i], us[j] = us[j], us[i]
			}
		}
		keys := make([]string, len(us))
		byKey := map[string]*DirectoryUser{}
		for i, u := range us {
			keys[i] = u.ID()
			byKey[u.ID()] = u
		}
		page, next, err := pageOfSize(req, "users:"+q.Get("orderBy")+q.Get("sortOrder"), keys, size)
		if err != nil {
			return err
		}
		out := map[string]any{"kind": "admin#directory#users", "users": []any{}}
		list := []any{}
		for _, k := range page {
			list = append(list, byKey[k].Object)
		}
		out["users"] = list
		if next != "" {
			out["nextPageToken"] = next
		}
		return rest.WriteJSON(w, http.StatusOK, out)
	})
	rt.Handle("GET /admin/directory/v1/users/{user}", func(w http.ResponseWriter, req *http.Request) error {
		u, err := r.User(req.PathValue("user"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, u.Object)
	})
	update := func(w http.ResponseWriter, req *http.Request) error {
		obj, err := decodeObject(req)
		if err != nil {
			return err
		}
		u, err := r.UpdateUser(req.PathValue("user"), obj)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, u.Object)
	}
	rt.Handle("PUT /admin/directory/v1/users/{user}", update)
	rt.Handle("PATCH /admin/directory/v1/users/{user}", update)
	rt.Handle("DELETE /admin/directory/v1/users/{user}", func(w http.ResponseWriter, req *http.Request) error {
		if err := r.DeleteUser(req.PathValue("user")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
}

// pageOfSize pages keys by a size already checked.
func pageOfSize(req *http.Request, scope string, keys []string, size int) ([]string, string, error) {
	q := req.URL.Query()
	q.Set("pageSize", strconv.Itoa(size))
	r2 := req.Clone(req.Context())
	r2.URL.RawQuery = q.Encode()
	return pageOf(r2, scope, keys, size, size)
}

// userTime is a user's creation time.
func (u *DirectoryUser) userTime() time.Time {
	s, _ := u.Object["creationTime"].(string)
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
