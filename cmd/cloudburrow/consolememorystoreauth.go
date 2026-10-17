package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// Token auth, backups and ACL policies on the Memorystore Redis Cluster
// and Memorystore for Valkey pages (#1549): a cluster's or instance's page
// lists its token auth users and their tokens, and its backups, and
// offers Add token auth user, Add auth token, Delete auth token, Delete
// token auth user, Back up now, Export backup and Delete backup; a Redis
// cluster's page also sets its ACL policy. Create takes Token
// authentication and a backup to restore from.

// tokenSection is the Token auth tab: one row per token (or per user
// without one), with the token, as GetAuthToken returns it.
func tokenSection(users map[string][]string) console.Section {
	l := console.Listing{Columns: []string{"User", "Token"}, NameColumn: "Token auth user / token", Noun: "tokens",
		Note: "Clients log in as AUTH <user> <token>; a client without a token is refused."}
	for u, toks := range users {
		if len(toks) == 0 {
			l.Items = append(l.Items, console.Resource{Name: u, Status: "ACTIVE", Fields: map[string]string{"User": u, "Token": "—"}})
		}
		for _, t := range toks {
			id, tok, _ := strings.Cut(t, "\x00")
			l.Items = append(l.Items, console.Resource{Name: u + "/" + id, Status: "ACTIVE", Fields: map[string]string{"User": u, "Token": tok}})
		}
	}
	l.Total = len(l.Items)
	return console.Section{ID: "token-auth", Label: "Token auth", Listing: l}
}

func backupSection(rows []console.Resource) console.Section {
	l := console.Listing{Columns: []string{"Created", "Type", "Files", "Size"}, NameColumn: "Backup ID", Noun: "backups", Items: rows, Total: len(rows),
		Note: "An RDB snapshot of each shard's primary, kept in this instance's state; Create can restore one into a new cluster."}
	return console.Section{ID: "backups", Label: "Backups", Listing: l}
}

// memorystoreAuthActions are the page's actions: token auth's when on.
func memorystoreAuthActions(tokenAuth bool, cluster bool) []console.Action {
	var out []console.Action
	if tokenAuth {
		user := console.Field{Name: "user", Label: "Username", Type: "text", Required: true, Pattern: `^[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,63}$`}
		out = append(out,
			console.Action{ID: "add-token-user", Label: "Add token auth user", Fields: []console.Field{user}},
			console.Action{ID: "add-token", Label: "Add auth token", Fields: []console.Field{user}},
			console.Action{ID: "delete-token", Label: "Delete auth token", Destructive: true, Confirm: "Clients using the token can no longer log in.",
				Fields: []console.Field{user, {Name: "token", Label: "Token ID", Type: "text", Required: true}}},
			console.Action{ID: "delete-token-user", Label: "Delete token auth user", Destructive: true, Confirm: "The user and its tokens are deleted on every node.",
				Fields: []console.Field{user}})
	}
	backupID := console.Field{Name: "backup", Label: "Backup ID", Type: "text", Required: true, Pattern: `^[a-z0-9][a-z0-9\-]{0,62}$`}
	out = append(out,
		console.Action{ID: "backup", Label: "Back up now", Fields: []console.Field{{Name: "backup", Label: "Backup ID", Type: "text", Pattern: `^[a-z0-9][a-z0-9\-]{0,62}$`,
			Help: "Optional: the time, by default."}}},
		console.Action{ID: "export-backup", Label: "Export backup", Fields: []console.Field{backupID,
			{Name: "bucket", Label: "Bucket", Type: "text", Required: true, Help: "A bucket of this instance's Cloud Storage: the files go under the backup ID."}}},
		console.Action{ID: "delete-backup", Label: "Delete backup", Destructive: true, Confirm: "The backup's files are deleted.", Fields: []console.Field{backupID}})
	if cluster {
		rules := console.Field{Name: "rules", Label: "Rules", Type: "map", Required: true,
			Help: "One username=rule per line, the rule a Valkey ACL rule such as on >secret ~app:* +@read."}
		policyID := console.Field{Name: "policyId", Label: "ACL policy ID", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{2,61}[a-z0-9]$`}
		out = append(out,
			console.Action{ID: "acl-policy", Label: "Set ACL policy", Fields: []console.Field{{Name: "policy", Label: "ACL policy", Type: "text",
				Help: "projects/{project}/locations/{location}/aclPolicies/{policy}; empty detaches it."}}},
			console.Action{ID: "create-acl-policy", Label: "Create ACL policy", Fields: []console.Field{policyID, rules}},
			console.Action{ID: "update-acl-policy", Label: "Update ACL policy", Confirm: "The rules are set on every node of every cluster the policy is attached to.",
				Fields: []console.Field{policyID, rules}},
			console.Action{ID: "delete-acl-policy", Label: "Delete ACL policy", Destructive: true, Confirm: "The policy is deleted; one attached to a cluster is refused.",
				Fields: []console.Field{policyID}})
	}
	return out
}

// --- Redis Cluster ---

func (p redisClusterProvider) authSections(ctx context.Context, api *redisadmin.ClusterServer, c *clusterpb.Cluster) []console.Section {
	var out []console.Section
	if c.GetAuthorizationMode() == clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH {
		users := map[string][]string{}
		if resp, err := api.ListTokenAuthUsers(ctx, &clusterpb.ListTokenAuthUsersRequest{Parent: c.GetName(), PageSize: 1000}); err == nil {
			for _, u := range resp.GetTokenAuthUsers() {
				id := u.GetName()[strings.LastIndex(u.GetName(), "/")+1:]
				users[id] = []string{}
				toks, _ := api.ListAuthTokens(ctx, &clusterpb.ListAuthTokensRequest{Parent: u.GetName(), PageSize: 1000})
				for _, t := range toks.GetAuthTokens() {
					users[id] = append(users[id], t.GetName()[strings.LastIndex(t.GetName(), "/")+1:]+"\x00"+t.GetToken())
				}
			}
		}
		out = append(out, tokenSection(users))
	}
	var rows []console.Resource
	if col := c.GetBackupCollection(); col != "" {
		if resp, err := api.ListBackups(ctx, &clusterpb.ListBackupsRequest{Parent: col, PageSize: 1000}); err == nil {
			for _, b := range resp.GetBackups() {
				rows = append(rows, console.Resource{Name: b.GetName()[strings.LastIndex(b.GetName(), "/")+1:], Status: b.GetState().String(), Fields: map[string]string{
					"Created": b.GetCreateTime().AsTime().Format("2006-01-02 15:04:05"), "Type": b.GetBackupType().String(),
					"Files": strconv.Itoa(len(b.GetBackupFiles())), "Size": fmt.Sprintf("%d bytes", b.GetTotalSizeBytes())}})
			}
		}
	}
	out = append(out, backupSection(rows))
	// The location's ACL policies, which the page's actions create, update,
	// delete and attach.
	loc := c.GetName()[:strings.Index(c.GetName(), "/clusters/")]
	pl := console.Listing{Columns: []string{"Rules", "Attached to", "Revision"}, NameColumn: "ACL policy", Noun: "ACL policies",
		Note: "Each rule is a Valkey ACL user on every node of each cluster the policy is attached to."}
	if resp, err := api.ListAclPolicies(ctx, &clusterpb.ListAclPoliciesRequest{Parent: loc, PageSize: 1000}); err == nil {
		for _, pol := range resp.GetAclPolicies() {
			var rules, at []string
			for _, r := range pol.GetRules() {
				rules = append(rules, r.GetUsername()+": "+r.GetRule())
			}
			for _, a := range pol.GetClusterAclPolicyAttachments() {
				at = append(at, memorystoreID(a.GetCluster()))
			}
			pl.Items = append(pl.Items, console.Resource{Name: pol.GetName()[strings.LastIndex(pol.GetName(), "/")+1:], Status: pol.GetState().String(),
				Fields: map[string]string{"Rules": strings.Join(rules, "; "), "Attached to": orDash(strings.Join(at, ", ")), "Revision": pol.GetEtag()}})
		}
	}
	pl.Total = len(pl.Items)
	return append(out, console.Section{ID: "acl-policies", Label: "ACL policies", Listing: pl})
}

// aclRules reads the Rules field: username=rule per line.
func aclRules(v string) ([]*clusterpb.AclRule, error) {
	m, err := console.ParseMap(v)
	if err != nil {
		return nil, err
	}
	var out []*clusterpb.AclRule
	for u, r := range m {
		out = append(out, &clusterpb.AclRule{Username: u, Rule: r})
	}
	return out, nil
}

// DetailActions implements console.PathActor.
func (p redisClusterProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	api, _, err := p.api()
	if err != nil {
		return nil
	}
	name, err := redisClusterName(project, strings.Join(path, "/"))
	if err != nil {
		return nil
	}
	c, err := api.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: name})
	if err != nil {
		return nil
	}
	return memorystoreAuthActions(c.GetAuthorizationMode() == clusterpb.AuthorizationMode_AUTH_MODE_TOKEN_AUTH, true)
}

// ActAt implements console.PathActor, each operation waited for.
func (p redisClusterProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	api, srv, err := p.api()
	if err != nil {
		return err
	}
	name, err := redisClusterName(project, strings.Join(path, "/"))
	if err != nil {
		return err
	}
	c, err := api.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: name})
	if err != nil {
		return err
	}
	user := name + "/tokenAuthUsers/" + strings.TrimSpace(values["user"])
	backup := c.GetBackupCollection() + "/backups/" + strings.TrimSpace(values["backup"])
	var op interface{ GetName() string }
	switch action {
	case "add-token-user":
		op, err = api.AddTokenAuthUser(ctx, &clusterpb.AddTokenAuthUserRequest{Cluster: name, TokenAuthUser: strings.TrimSpace(values["user"])})
	case "add-token":
		op, err = api.AddAuthToken(ctx, &clusterpb.AddAuthTokenRequest{TokenAuthUser: user, AuthToken: &clusterpb.AuthToken{}})
	case "delete-token":
		op, err = api.DeleteAuthToken(ctx, &clusterpb.DeleteAuthTokenRequest{Name: user + "/authTokens/" + strings.TrimSpace(values["token"])})
	case "delete-token-user":
		op, err = api.DeleteTokenAuthUser(ctx, &clusterpb.DeleteTokenAuthUserRequest{Name: user, Force: true})
	case "backup":
		req := &clusterpb.BackupClusterRequest{Name: name}
		if b := strings.TrimSpace(values["backup"]); b != "" {
			req.BackupId = proto.String(b)
		}
		op, err = api.BackupCluster(ctx, req)
	case "export-backup":
		op, err = api.ExportBackup(ctx, &clusterpb.ExportBackupRequest{Name: backup,
			Destination: &clusterpb.ExportBackupRequest_GcsBucket{GcsBucket: strings.TrimSpace(values["bucket"])}})
	case "delete-backup":
		op, err = api.DeleteBackup(ctx, &clusterpb.DeleteBackupRequest{Name: backup})
	case "create-acl-policy", "update-acl-policy", "delete-acl-policy":
		loc := name[:strings.Index(name, "/clusters/")]
		pname := loc + "/aclPolicies/" + strings.TrimSpace(values["policyId"])
		var rules []*clusterpb.AclRule
		if action != "delete-acl-policy" {
			if rules, err = aclRules(values["rules"]); err != nil {
				return err
			}
		}
		switch action {
		case "create-acl-policy":
			_, err = api.CreateAclPolicy(ctx, &clusterpb.CreateAclPolicyRequest{Parent: loc, AclPolicyId: strings.TrimSpace(values["policyId"]),
				AclPolicy: &clusterpb.AclPolicy{Rules: rules}})
			return err
		case "update-acl-policy":
			op, err = api.UpdateAclPolicy(ctx, &clusterpb.UpdateAclPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"rules"}},
				AclPolicy: &clusterpb.AclPolicy{Name: pname, Rules: rules}})
		default:
			op, err = api.DeleteAclPolicy(ctx, &clusterpb.DeleteAclPolicyRequest{Name: pname})
		}
	case "acl-policy":
		op, err = api.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"acl_policy"}},
			Cluster: &clusterpb.Cluster{Name: name, AclPolicy: strings.TrimSpace(values["policy"])}})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return err
	}
	return waitRedisOp(ctx, srv, op.GetName())
}

// --- Memorystore for Valkey ---

func (p valkeyProvider) authSections(ctx context.Context, api *redisadmin.MemorystoreServer, i *memorystorepb.Instance) []console.Section {
	var out []console.Section
	if i.GetAuthorizationMode() == memorystorepb.Instance_TOKEN_AUTH {
		users := map[string][]string{}
		if resp, err := api.ListTokenAuthUsers(ctx, &memorystorepb.ListTokenAuthUsersRequest{Parent: i.GetName(), PageSize: 1000}); err == nil {
			for _, u := range resp.GetTokenAuthUsers() {
				id := u.GetName()[strings.LastIndex(u.GetName(), "/")+1:]
				users[id] = []string{}
				toks, _ := api.ListAuthTokens(ctx, &memorystorepb.ListAuthTokensRequest{Parent: u.GetName(), PageSize: 1000})
				for _, t := range toks.GetAuthTokens() {
					users[id] = append(users[id], t.GetName()[strings.LastIndex(t.GetName(), "/")+1:]+"\x00"+t.GetToken())
				}
			}
		}
		out = append(out, tokenSection(users))
	}
	var rows []console.Resource
	if col := i.GetBackupCollection(); col != "" {
		if resp, err := api.ListBackups(ctx, &memorystorepb.ListBackupsRequest{Parent: col, PageSize: 1000}); err == nil {
			for _, b := range resp.GetBackups() {
				rows = append(rows, console.Resource{Name: b.GetName()[strings.LastIndex(b.GetName(), "/")+1:], Status: b.GetState().String(), Fields: map[string]string{
					"Created": b.GetCreateTime().AsTime().Format("2006-01-02 15:04:05"), "Type": b.GetBackupType().String(),
					"Files": strconv.Itoa(len(b.GetBackupFiles())), "Size": fmt.Sprintf("%d bytes", b.GetTotalSizeBytes())}})
			}
		}
	}
	return append(out, backupSection(rows))
}

// DetailActions implements console.PathActor.
func (p valkeyProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	api, _, err := p.api()
	if err != nil {
		return nil
	}
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return nil
	}
	i, err := api.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		return nil
	}
	return memorystoreAuthActions(i.GetAuthorizationMode() == memorystorepb.Instance_TOKEN_AUTH, false)
}

// ActAt implements console.PathActor, each operation waited for.
func (p valkeyProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	api, srv, err := p.api()
	if err != nil {
		return err
	}
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return errors.New("no such instance")
	}
	i, err := api.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		return err
	}
	user := name + "/tokenAuthUsers/" + strings.TrimSpace(values["user"])
	backup := i.GetBackupCollection() + "/backups/" + strings.TrimSpace(values["backup"])
	var op interface{ GetName() string }
	switch action {
	case "add-token-user":
		op, err = api.AddTokenAuthUser(ctx, &memorystorepb.AddTokenAuthUserRequest{Instance: name, TokenAuthUser: strings.TrimSpace(values["user"])})
	case "add-token":
		op, err = api.AddAuthToken(ctx, &memorystorepb.AddAuthTokenRequest{TokenAuthUser: user, AuthToken: &memorystorepb.AuthToken{}})
	case "delete-token":
		op, err = api.DeleteAuthToken(ctx, &memorystorepb.DeleteAuthTokenRequest{Name: user + "/authTokens/" + strings.TrimSpace(values["token"])})
	case "delete-token-user":
		op, err = api.DeleteTokenAuthUser(ctx, &memorystorepb.DeleteTokenAuthUserRequest{Name: user, Force: true})
	case "backup":
		req := &memorystorepb.BackupInstanceRequest{Name: name}
		if b := strings.TrimSpace(values["backup"]); b != "" {
			req.BackupId = proto.String(b)
		}
		op, err = api.BackupInstance(ctx, req)
	case "export-backup":
		op, err = api.ExportBackup(ctx, &memorystorepb.ExportBackupRequest{Name: backup,
			Destination: &memorystorepb.ExportBackupRequest_GcsBucket{GcsBucket: strings.TrimSpace(values["bucket"])}})
	case "delete-backup":
		op, err = api.DeleteBackup(ctx, &memorystorepb.DeleteBackupRequest{Name: backup})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return err
	}
	return waitRedisOp(ctx, srv, op.GetName())
}
