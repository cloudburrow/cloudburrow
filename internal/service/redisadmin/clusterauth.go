package redisadmin

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Token auth (#1549), for a Redis cluster (AUTH_MODE_TOKEN_AUTH) and a
// Memorystore for Valkey instance (TOKEN_AUTH), as Google documents it:
// token auth users, each with auth tokens the service generates, which a
// client sends as AUTH <user> <token>. Each user is a Valkey ACL user on
// every node (ACL SETUSER <user> reset on ><token>... ~* &* +@all), set
// again on every node that starts and whenever a user or token changes;
// a user without tokens is on but has no password, so nothing can log in
// as it. The nodes' default user has a password of the cluster's own,
// which only CloudBurrow's admin plane knows (requirepass, masterauth for
// the replicas, AUTH on MIGRATE), so a client without a token is refused
// (NOAUTH, or WRONGPASS for a wrong one).

// tokenUser is a token auth user, as it is kept.
type tokenUser struct {
	Created time.Time   `json:"created"`
	Tokens  []authToken `json:"tokens,omitempty"`
}

// authToken is one of a user's tokens.
type authToken struct {
	ID      string    `json:"id"`
	Token   string    `json:"token"`
	Created time.Time `json:"created"`
}

// tokenUserRE matches a token auth user's name, and an auth token's.
var tokenUserRE = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/(?:clusters|instances)/[^/]+)/tokenAuthUsers/([^/]+)(?:/authTokens/([^/]+))?$`)

// aclUserRE is a username a token auth user or an ACL rule may have: a
// Valkey ACL username without spaces or rule characters.
var aclUserRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

func checkACLUser(field, u string) error {
	if !aclUserRE.MatchString(u) {
		return apierror.InvalidArgument("%s %q must be 1 to 64 letters, digits, '_', '.' or '-', starting with a letter or digit", field, u)
	}
	if u == "default" {
		return apierror.InvalidArgument("%s must not be \"default\": the nodes' default user is the service's own", field)
	}
	return nil
}

// tokenAuth is whether r authenticates clients with token auth users.
func (r *clusterRecord) tokenAuth() bool {
	if r.Valkey != nil {
		return r.Valkey.AuthMode == "TOKEN_AUTH"
	}
	return r.AuthMode == "AUTH_MODE_TOKEN_AUTH"
}

// aclUsers are the ACL users every node of r has, each SETUSER's rules
// after the username, in name order: the token auth users and the ACL
// policy's (aclpolicy.go). Call with mu held, or on a snapshot.
func (c *clusters) aclUsers(r *clusterRecord) map[string][]any {
	out := map[string][]any{}
	for u, tu := range r.TokenUsers {
		rule := []any{"reset", "on"}
		for _, t := range tu.Tokens {
			rule = append(rule, ">"+t.Token)
		}
		out[u] = append(rule, "~*", "&*", "+@all")
	}
	for u, rule := range c.policyUsers(r) {
		out[u] = rule
	}
	return out
}

// syncUsers sets every ACL user of r on nodes, and deletes the users a
// node has that r has not: ACL users are each node's own, not the
// cluster's, so each is told.
func (c *clusters) syncUsers(ctx context.Context, r *clusterRecord, users map[string][]any, nodes []*clusterNode) error {
	names := make([]string, 0, len(users))
	for u := range users {
		names = append(names, u)
	}
	sort.Strings(names)
	for _, n := range nodes {
		cl := c.dial(n.addr, r.password(), r.TransitCA)
		have, err := cl.Do(ctx, "ACL", "USERS")
		if err != nil {
			cl.Close()
			return fmt.Errorf("ACL USERS on node %s: %w", n.Server, err)
		}
		list, _ := have.([]any)
		for _, h := range list {
			u := fmt.Sprint(h)
			if _, keep := users[u]; !keep && u != "default" {
				if _, err := cl.Do(ctx, "ACL", "DELUSER", u); err != nil {
					cl.Close()
					return fmt.Errorf("ACL DELUSER %s on node %s: %w", u, n.Server, err)
				}
			}
		}
		for _, u := range names {
			args := append([]any{"ACL", "SETUSER", u}, users[u]...)
			if _, err := cl.Do(ctx, args...); err != nil {
				cl.Close()
				return fmt.Errorf("ACL SETUSER %s on node %s: %w", u, n.Server, err)
			}
		}
		cl.Close()
	}
	return nil
}

// changeUsers runs change on r (mu held), keeps r, and returns the
// operation that completes, with resp, once every node has r's users.
func (c *clusters) changeUsers(r *clusterRecord, verb string, needToken bool, change func() error, resp func() proto.Message) (string, error) {
	kind := "cluster"
	if c.valkey {
		kind = "instance"
	}
	if needToken && !r.tokenAuth() {
		return "", apierror.FailedPrecondition("%s %s does not use token auth: its authorization mode is not TOKEN_AUTH", kind, r.Name)
	}
	if _, pending := c.busy[r.Name]; pending || r.state.String() != "ACTIVE" {
		return "", apierror.FailedPrecondition("%s %s is %s or has an operation in progress: it is changed only when ACTIVE", kind, r.Name, r.state)
	}
	if err := change(); err != nil {
		return "", err
	}
	if err := c.save(r); err != nil {
		return "", apierror.Internal(err, "keep %s %s", kind, r.Name)
	}
	op := c.newOperation(r.Name, verb)
	c.busy[r.Name] = op
	snap := *r
	nodes := append([]*clusterNode(nil), r.Nodes...)
	users := c.aclUsers(r)
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		err := c.syncUsers(ctx, &snap, users, nodes)
		c.mu.Lock()
		delete(c.busy, r.Name)
		out := resp()
		c.mu.Unlock()
		if err != nil {
			err = fmt.Errorf("the nodes' ACL users did not change: %w", err)
		}
		c.finish(op, out, err)
	}()
	return op, nil
}

// tokenUserState is what a token auth user's or token's state is: ACTIVE,
// or UPDATING (CREATING for a token) while its owner's operation runs.
func (c *clusters) tokenUserState(r *clusterRecord) string {
	if _, pending := c.busy[r.Name]; pending {
		return "UPDATING"
	}
	return "ACTIVE"
}

// tokenUserName and authTokenName are the resource names.
func tokenUserName(owner, user string) string { return owner + "/tokenAuthUsers/" + user }

func authTokenName(owner, user, id string) string {
	return tokenUserName(owner, user) + "/authTokens/" + id
}

// addTokenUser adds user to r. Call with mu held.
func (c *clusters) addTokenUser(r *clusterRecord, user string, resp func() proto.Message) (string, error) {
	if err := checkACLUser("token_auth_user", user); err != nil {
		return "", err
	}
	return c.changeUsers(r, "addTokenAuthUser", true, func() error {
		if _, ok := r.TokenUsers[user]; ok {
			return apierror.AlreadyExists("Resource '%s' already exists", tokenUserName(r.Name, user))
		}
		if _, ok := c.policyUsers(r)[user]; ok {
			return apierror.AlreadyExists("user %q is a user of the cluster's ACL policy", user)
		}
		users := map[string]*tokenUser{}
		for k, v := range r.TokenUsers {
			users[k] = v
		}
		users[user] = &tokenUser{Created: c.now()}
		r.TokenUsers = users
		return nil
	}, resp)
}

// deleteTokenUser removes user from r; one with tokens only with force, as
// Google's does. Call with mu held.
func (c *clusters) deleteTokenUser(r *clusterRecord, user string, force bool, resp func() proto.Message) (string, error) {
	tu, ok := r.TokenUsers[user]
	if !ok {
		return "", apierror.NotFound("Resource '%s' was not found", tokenUserName(r.Name, user))
	}
	if len(tu.Tokens) > 0 && !force {
		return "", apierror.FailedPrecondition("token auth user %s has auth tokens: delete them first, or set force", tokenUserName(r.Name, user))
	}
	return c.changeUsers(r, "deleteTokenAuthUser", false, func() error {
		users := map[string]*tokenUser{}
		for k, v := range r.TokenUsers {
			if k != user {
				users[k] = v
			}
		}
		r.TokenUsers = users
		return nil
	}, resp)
}

// addAuthToken generates a token for user. Call with mu held.
func (c *clusters) addAuthToken(r *clusterRecord, user, id string, resp func() proto.Message) (string, error) {
	tu, ok := r.TokenUsers[user]
	if !ok {
		return "", apierror.NotFound("Resource '%s' was not found", tokenUserName(r.Name, user))
	}
	if id == "" {
		id = newUID()[:16]
	} else if !clusterIDRE.MatchString(id) && !regexp.MustCompile(`^[a-z0-9-]{1,63}$`).MatchString(id) {
		return "", apierror.InvalidArgument("auth token ID %q must be lowercase letters, digits and hyphens", id)
	}
	for _, t := range tu.Tokens {
		if t.ID == id {
			return "", apierror.AlreadyExists("Resource '%s' already exists", authTokenName(r.Name, user, id))
		}
	}
	return c.changeUsers(r, "addAuthToken", true, func() error {
		next := &tokenUser{Created: tu.Created, Tokens: append(append([]authToken(nil), tu.Tokens...),
			authToken{ID: id, Token: newUID() + newUID(), Created: c.now()})}
		users := map[string]*tokenUser{}
		for k, v := range r.TokenUsers {
			users[k] = v
		}
		users[user] = next
		r.TokenUsers = users
		return nil
	}, resp)
}

// deleteAuthToken removes a token. Call with mu held.
func (c *clusters) deleteAuthToken(r *clusterRecord, user, id string, resp func() proto.Message) (string, error) {
	tu, ok := r.TokenUsers[user]
	found := false
	if ok {
		for _, t := range tu.Tokens {
			found = found || t.ID == id
		}
	}
	if !found {
		return "", apierror.NotFound("Resource '%s' was not found", authTokenName(r.Name, user, id))
	}
	return c.changeUsers(r, "deleteAuthToken", false, func() error {
		next := &tokenUser{Created: tu.Created}
		for _, t := range tu.Tokens {
			if t.ID != id {
				next.Tokens = append(next.Tokens, t)
			}
		}
		users := map[string]*tokenUser{}
		for k, v := range r.TokenUsers {
			users[k] = v
		}
		users[user] = next
		r.TokenUsers = users
		return nil
	}, resp)
}

// tokenUsersPage is a page of r's user names, in order.
func tokenUsersPage(r *clusterRecord, token string, size int32) ([]string, string, error) {
	if size < 0 {
		return nil, "", apierror.InvalidArgument("page_size must not be negative")
	}
	names := make([]string, 0, len(r.TokenUsers))
	for u := range r.TokenUsers {
		names = append(names, u)
	}
	sort.Strings(names)
	page, next, err := paging.Page("token-auth-users:"+r.Name, names, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("invalid page_token %q", token)
	}
	return page, next, nil
}

// authTokensPage is a page of user's tokens, in ID order.
func authTokensPage(r *clusterRecord, user, token string, size int32) ([]authToken, string, error) {
	if size < 0 {
		return nil, "", apierror.InvalidArgument("page_size must not be negative")
	}
	tu, ok := r.TokenUsers[user]
	if !ok {
		return nil, "", apierror.NotFound("Resource '%s' was not found", tokenUserName(r.Name, user))
	}
	byID := map[string]authToken{}
	ids := make([]string, 0, len(tu.Tokens))
	for _, t := range tu.Tokens {
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	page, next, err := paging.Page("auth-tokens:"+tokenUserName(r.Name, user), ids, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("invalid page_token %q", token)
	}
	out := make([]authToken, len(page))
	for i, id := range page {
		out[i] = byID[id]
	}
	return out, next, nil
}

// findToken is a user's token by ID.
func findToken(r *clusterRecord, user, id string) (authToken, bool) {
	for _, t := range r.TokenUsers[user].getTokens() {
		if t.ID == id {
			return t, true
		}
	}
	return authToken{}, false
}

func (tu *tokenUser) getTokens() []authToken {
	if tu == nil {
		return nil
	}
	return tu.Tokens
}

// splitTokenName reads owner, user and token ID (or "") from a name.
func splitTokenName(name string, wantToken bool) (owner, user, id string, err error) {
	m := tokenUserRE.FindStringSubmatch(name)
	if m == nil || (wantToken != (m[3] != "")) {
		what := "tokenAuthUsers/{user}"
		if wantToken {
			what += "/authTokens/{token}"
		}
		return "", "", "", apierror.InvalidArgument("name must be {cluster or instance}/%s, not %q", what, name)
	}
	return m[1], m[2], m[3], nil
}

// trimOwner is the name of the cluster or instance a token name names.
func trimOwner(name string) string {
	if i := strings.Index(name, "/tokenAuthUsers/"); i >= 0 {
		return name[:i]
	}
	return name
}

// checkCrossCluster refuses cross-cluster (cross-instance) replication:
// a Valkey cluster-mode replica follows a primary of its own cluster only
// (CLUSTER REPLICATE takes a node ID the cluster's gossip knows), so a
// separate cluster cannot replicate another's shards, and a switchover
// would have to merge the two clusters' gossip.
func checkCrossCluster(asked bool) error {
	if !asked {
		return nil
	}
	return apierror.Unimplemented("cross-cluster replication is not served here: a Valkey cluster-mode replica replicates only a primary of its own cluster (CLUSTER REPLICATE), so one cluster cannot be a secondary of another")
}
