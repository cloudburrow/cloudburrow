package redisadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// IAM auth (#1614), for a Redis cluster (AUTH_MODE_IAM_AUTH) and a
// Memorystore for Valkey instance (IAM_AUTH), as Google documents it
// (https://docs.cloud.google.com/memorystore/docs/cluster/about-iam-auth,
// .../valkey/about-iam-auth, .../cluster/manage-iam-auth, read 2026-11-17):
// a client sends an access token as its AUTH password, with no username,
// the username "default" or the principal's email; the principal must hold
// roles/redis.dbConnectionUser (a Redis cluster) or
// roles/memorystore.dbConnectionUser (a Valkey instance) on the project;
// an invalid or expired token, or a principal without the role, is
// refused "-WRONGPASS invalid username-password pair or user is disabled",
// and a client that sends none "-NOAUTH Authentication required". A
// connection already authenticated keeps working after its token expires.
//
// The token is checked the only way a token CloudBurrow issued can be:
// against the instance's own log of the access tokens it minted
// (internal/metadata's issued.go: each token's SHA-256, the account it was
// minted for, its expiry), never by asking Google. The nodes' default user
// has a password of the cluster's own (as with token auth, clusterauth.go)
// and, beside it, the hash of every live token of every principal the
// project's IAM policy grants the role (ACL SETUSER default resetpass
// >admin #<sha256>...), and each such principal is an ACL user named by
// its email with its own tokens' hashes. They are set on every node when it
// starts, as soon as a token is minted (before the token is returned, so
// the client's first AUTH finds it), and on a tick that drops expired
// tokens and follows policy changes (Google: "may take a few minutes").
//
// A binding's condition (#1742, #1745) is evaluated per cluster or
// instance, as Google evaluates one inherited from the project: request.time
// and resource tags (the cluster's or instance's effective tags, its own
// bindings and the project's) are evaluated, and a resource.name, .type or
// .service part grants nothing, because Memorystore is not among the
// services Google's "Resource attributes for IAM Conditions" lists ("Other
// services and resource types do not recognize resource attributes").
//
// Not served: roles other than the two named (Google's basic roles are not expanded
// here). A cluster or instance kept from before #1614 with IAM auth, whose
// nodes' default user has no password, still takes any AUTH string, until
// it is created again.

// IssuedToken is an access token the instance minted: its SHA-256 in hex,
// the account it was minted for ("" when none could be checked) and when it
// expires.
type IssuedToken struct {
	Hash    string
	Email   string
	Expires time.Time
}

// IAMSource is what IAM auth checks a token against: the instance's minted
// tokens, and the members a project's IAM policy grants a role.
type IAMSource struct {
	// Tokens are the access tokens live at now.
	Tokens func(now time.Time) []IssuedToken
	// Members are the members ("serviceAccount:x@y", "user:x@y") of
	// project's bindings of role that grant it on resource (the cluster's
	// or instance's name): a conditional binding counts where its
	// condition holds for resource.
	Members func(project, resource, role string) []string
}

// iamEvery is how often the nodes' IAM users are set again: expired tokens
// dropped, policy changes followed.
var iamEvery = 5 * time.Second

// iamRefresh is how long a node's IAM users are trusted to be still set
// (a node restarted on its own loses them) before they are set again.
const iamRefresh = 30 * time.Second

// iamSyncTimeout bounds one sync of the nodes' IAM users, so a token
// request waits at most about twice this for a sync already running.
const iamSyncTimeout = 5 * time.Second

// iamState is a clusters' IAM auth bookkeeping.
type iamState struct {
	src IAMSource
	// run serialises the syncs.
	run sync.Mutex
	// synced is, by record name, the fingerprint of the users last set on
	// its nodes, and when.
	synced map[string]iamSynced
}

type iamSynced struct {
	print string
	at    time.Time
}

// UseIAM makes IAM auth check the tokens src knows, on the Redis clusters
// and Valkey instances, and starts keeping their nodes' users current.
// Without it, an IAM auth cluster's nodes take only their own password, so
// every client is refused.
func (s *Server) UseIAM(src IAMSource) {
	for _, c := range []*clusters{s.clusters, s.valkey} {
		if c == nil {
			continue
		}
		c.mu.Lock()
		c.iam = &iamState{src: src, synced: map[string]iamSynced{}}
		c.mu.Unlock()
		go c.watchIAM(c.bg, iamEvery)
	}
}

// TokenIssued sets the nodes' users of every IAM auth cluster and instance
// that tok's account may connect to, before it returns: the OnIssue hook of
// the instance's credentials.
func (s *Server) TokenIssued(tok IssuedToken) {
	if tok.Email == "" {
		return
	}
	for _, c := range []*clusters{s.clusters, s.valkey} {
		if c == nil {
			continue
		}
		// Bounded: the client asking for a token waits for this, and a
		// node that is down (a failover) must not hold it up long.
		ctx, cancel := context.WithTimeout(c.bg, iamSyncTimeout)
		c.syncIAM(ctx, tok.Email)
		cancel()
	}
}

// iamAuth is whether r's nodes check IAM tokens.
func (r *clusterRecord) iamAuth() bool {
	if r.AdminPassword == "" {
		return false
	}
	if r.Valkey != nil {
		return r.Valkey.AuthMode == "IAM_AUTH"
	}
	return r.AuthMode == "AUTH_MODE_IAM_AUTH"
}

// connectRole is the role Google documents a principal needs to connect
// to r's nodes with IAM auth.
func (r *clusterRecord) connectRole() string {
	if r.Valkey != nil {
		return "roles/memorystore.dbConnectionUser"
	}
	return "roles/redis.dbConnectionUser"
}

// project is r's project ID.
func (r *clusterRecord) project() string {
	p := strings.TrimPrefix(r.Name, "projects/")
	p, _, _ = strings.Cut(p, "/")
	return p
}

// iamUsers are the ACL users IAM auth gives r's nodes: the default user
// with its password and every allowed token's hash, and one user per
// allowed principal. nil when r does not use IAM auth. It may be called
// with mu held: the source takes locks of its own only.
func (c *clusters) iamUsers(r *clusterRecord) map[string][]any {
	if !r.iamAuth() {
		return nil
	}
	def := []any{"on", "resetpass", ">" + r.AdminPassword}
	out := map[string][]any{}
	if c.iam == nil || c.iam.src.Tokens == nil || c.iam.src.Members == nil {
		out["default"] = def
		return out
	}
	allowed := map[string]bool{}
	for _, m := range c.iam.src.Members(r.project(), r.Name, r.connectRole()) {
		kind, email, ok := strings.Cut(m, ":")
		if ok && (kind == "serviceAccount" || kind == "user") {
			allowed[strings.ToLower(email)] = true
		}
	}
	byEmail := map[string][]any{}
	for _, t := range c.iam.src.Tokens(c.now()) {
		email := strings.ToLower(t.Email)
		if email == "" || !allowed[email] {
			continue
		}
		def = append(def, "#"+t.Hash)
		byEmail[email] = append(byEmail[email], "#"+t.Hash)
	}
	out["default"] = def
	for email, hashes := range byEmail {
		rule := append([]any{"reset", "on"}, hashes...)
		out[email] = append(rule, "~*", "&*", "+@all")
	}
	return out
}

// fingerprint is a digest of users, to tell when they changed.
func fingerprint(users map[string][]any) string {
	names := make([]string, 0, len(users))
	for u := range users {
		names = append(names, u)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, u := range names {
		fmt.Fprintf(h, "%s\x00%v\x00", u, users[u])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// watchIAM sets the IAM users of c's records every every, until ctx ends.
func (c *clusters) watchIAM(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sctx, cancel := context.WithTimeout(ctx, iamSyncTimeout)
			c.syncIAM(sctx, "")
			cancel()
		}
	}
}

// syncIAM sets the ACL users of every ACTIVE IAM auth record of c on its
// nodes, when they changed or were last set iamRefresh ago; with email,
// only the records email's account is granted on, and whenever they
// changed. A record with an operation in progress is left to the next
// tick (its own operation sets its nodes' users when it starts any).
func (c *clusters) syncIAM(ctx context.Context, email string) {
	c.mu.Lock()
	st := c.iam
	c.mu.Unlock()
	if st == nil {
		return
	}
	st.run.Lock()
	defer st.run.Unlock()
	type job struct {
		snap  clusterRecord
		users map[string][]any
		nodes []*clusterNode
		print string
	}
	var jobs []job
	c.mu.Lock()
	now := c.now()
	for name, r := range c.byName {
		if !r.iamAuth() || r.state.String() != "ACTIVE" || len(r.Nodes) == 0 {
			continue
		}
		if _, pending := c.busy[name]; pending {
			continue
		}
		users := c.aclUsers(r)
		if email != "" {
			if _, granted := users[strings.ToLower(email)]; !granted {
				continue
			}
		}
		print := fingerprint(users)
		last := st.synced[name]
		if last.print == print && (email != "" || now.Sub(last.at) < iamRefresh) {
			continue
		}
		var live []*clusterNode
		for _, n := range r.Nodes {
			if n.addr != "" {
				live = append(live, n)
			}
		}
		jobs = append(jobs, job{*r, users, live, print})
	}
	c.mu.Unlock()
	for _, j := range jobs {
		if err := c.syncUsers(ctx, &j.snap, j.users, j.nodes); err != nil {
			// A node that is down is set when it starts again.
			continue
		}
		c.mu.Lock()
		st.synced[j.snap.Name] = iamSynced{print: j.print, at: c.now()}
		c.mu.Unlock()
	}
}

// switchAuth sets users on nodes, after an update; when IAM auth was turned
// on or off it first gives the replicas the new password to sync with, or
// gives the default user back no password. snap carries the new password;
// old is the one the nodes have now.
func (c *clusters) switchAuth(ctx context.Context, snap *clusterRecord, old string, changed bool, users map[string][]any, nodes []*clusterNode) error {
	if !changed {
		return c.syncUsers(ctx, snap, users, nodes)
	}
	if snap.AdminPassword != "" {
		// Turned on: a node whose default user has no password takes any
		// AUTH, the new password among them.
		for _, n := range nodes {
			cl := c.dial(n.addr, snap.AdminPassword, snap.TransitCA)
			_, err := cl.Do(ctx, "CONFIG", "SET", "masterauth", snap.AdminPassword)
			cl.Close()
			if err != nil {
				return fmt.Errorf("CONFIG SET masterauth on node %s: %w", n.Server, err)
			}
		}
		return c.syncUsers(ctx, snap, users, nodes)
	}
	// Turned off: dialled with the password the nodes still have.
	withOld := *snap
	withOld.AdminPassword = old
	if users == nil {
		users = map[string][]any{}
	}
	users["default"] = []any{"on", "resetpass", "nopass"}
	return c.syncUsers(ctx, &withOld, users, nodes)
}
