package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	adminpb "cloud.google.com/go/iam/admin/apiv1/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/iamcond"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// bigqueryIAM gives BigQuery's front what its per-project IAM check reads
// (#1767, internal/bigqueryfront/iamcheck.go), as firestoreMongoIAM gives
// Firestore's: the live access tokens these credentials minted, and the
// roles each of their accounts holds on each project in the registry's
// IAM policies (serviceAccount: and user: members of the roles
// bigqueryfront.BigQueryRoles names, and of custom roles with bigquery.
// permissions), with those inherited from the project's folders and
// organization, and conditional bindings with their expressions (#1774),
// which the front evaluates; every custom role with bigquery. permissions,
// and the effective tags of each project and the tags bound to BigQuery's
// datasets and tables (#1780), which those conditions read. The tokens of
// the instance's own account, which the ADC fixture holds, are left out:
// its requests keep their behaviour, every permission. It pushes them
// every bigqueryIAMEvery when they changed, every bigqueryIAMRefresh
// regardless (a restarted front lost them), and as soon as a token is
// minted, before it is returned.
type bigqueryIAM struct {
	creds    *metadata.Credentials
	registry func() *resourcemanager.Registry
	// addr is BigQuery's host address, "" while its tunnel is down.
	addr   func() string
	client *http.Client

	mu     sync.Mutex
	last   string
	lastAt time.Time
}

const (
	bigqueryIAMEvery   = 5 * time.Second
	bigqueryIAMRefresh = 30 * time.Second
)

// payload is what the front is given now.
func (b *bigqueryIAM) payload(now time.Time) bigqueryfront.BigQueryIAM {
	m := bigqueryfront.BigQueryIAM{Tokens: []bigqueryfront.BigQueryIAMToken{}, Roles: map[string]map[string][]string{},
		CustomRoles: map[string][]string{}, Conditional: map[string]map[string][]bigqueryfront.BigQueryIAMCondition{},
		Tags: map[string][]iamcond.Tag{}, TagValues: map[string]iamcond.Tag{}}
	emails := map[string]bool{}
	for _, t := range b.creds.LiveTokens(now) {
		// The instance's own account (the ADC fixture's) is served as
		// before, with every permission: its tokens are not given.
		if t.Email == "" || strings.EqualFold(t.Email, b.creds.Email) {
			continue
		}
		m.Tokens = append(m.Tokens, bigqueryfront.BigQueryIAMToken{Hash: t.Hash, Email: strings.ToLower(t.Email), Expires: t.Expires})
		emails[strings.ToLower(t.Email)] = true
	}
	sort.Slice(m.Tokens, func(i, j int) bool { return m.Tokens[i].Hash < m.Tokens[j].Hash })
	var reg *resourcemanager.Registry
	if b.registry != nil {
		reg = b.registry()
	}
	if reg == nil {
		return m
	}
	projects, err := reg.List()
	if err != nil {
		return m
	}
	// group: and domain: members name the accounts of a Cloud Identity
	// group or of the instance's domain (#1812); the front matches a
	// dataset's groupByEmail and domain entries by Principals.
	ix := newMemberIndex(reg, emails)
	m.Principals = principalsOf(reg, emails)
	// A project's roles are its own policy's and, inherited, those of its
	// folders and its organization (Resource Manager's hierarchy: "the
	// effective policy for a resource is the union of the policy set on
	// that resource and the policy inherited from its parent"). A
	// conditional binding (#1774) is given with its expression, which the
	// front evaluates for the resource it checks (internal/iamcond).
	policies := map[string]*iampolicy.Stored{}
	policyOf := func(resource string) *iampolicy.Stored {
		if pol, ok := policies[resource]; ok {
			return pol
		}
		pol, err := reg.IAMPolicy(resource)
		if err != nil {
			pol = nil
		}
		policies[resource] = pol
		return pol
	}
	orgs := map[string]bool{}
	for _, p := range projects {
		resources := []string{"projects/" + p.ProjectID}
		if anc, err := reg.Ancestry(p.ProjectID); err == nil {
			for _, a := range anc[1:] {
				switch a.Type {
				case "folder":
					resources = append(resources, "folders/"+a.ID)
				case "organization":
					resources = append(resources, "organizations/"+a.ID)
					orgs["organizations/"+a.ID] = true
				}
			}
		}
		// A project's effective tags (its own and its ancestors'), which
		// its datasets and tables inherit (#1780).
		if tags, err := reg.EffectiveTags(resourcemanager.CRMPrefix + "projects/" + p.ProjectID); err == nil && len(tags) > 0 {
			m.Tags["projects/"+p.ProjectID] = condTags(tags)
		}
		for _, res := range resources {
			pol := policyOf(res)
			if pol == nil {
				continue
			}
			b.addBindings(&m, reg, ix, p.ProjectID, pol.Bindings)
		}
	}
	b.addTags(&m, reg, projects)
	// Every custom role with BigQuery permissions, of each project and of
	// the organization, so that a role a table's or a routine's own policy
	// names grants too (#1780), not only those the hierarchy's policies
	// name.
	parents := make([]string, 0, len(projects)+len(orgs))
	for _, p := range projects {
		parents = append(parents, "projects/"+p.ProjectID)
	}
	for o := range orgs {
		parents = append(parents, o)
	}
	sort.Strings(parents)
	for _, parent := range parents {
		token := ""
		for {
			resp, err := reg.ListRoles(&adminpb.ListRolesRequest{Parent: parent, PageSize: 1000, PageToken: token})
			if err != nil {
				break
			}
			for _, role := range resp.GetRoles() {
				if _, done := m.CustomRoles[role.GetName()]; done {
					continue
				}
				if bq := bigqueryPerms(reg, role.GetName()); len(bq) > 0 {
					m.CustomRoles[role.GetName()] = bq
				}
			}
			if token = resp.GetNextPageToken(); token == "" {
				break
			}
		}
	}
	return m
}

// bigqueryPerms are the bigquery. permissions custom role grants now.
func bigqueryPerms(reg *resourcemanager.Registry, role string) []string {
	perms, _ := reg.RolePermissions(role)
	var bq []string
	for _, p := range perms {
		if strings.HasPrefix(p, "bigquery.") {
			bq = append(bq, p)
		}
	}
	return bq
}

// addTags gives the front the tags bound to BigQuery's datasets and tables
// (Resource Manager tag bindings on //bigquery.googleapis.com/projects/P/
// datasets/D[/tables/T], each resource's own, keyed by that name with the
// project named by its ID), and every tag value by its namespaced name, by
// which a dataset's or table's resourceTags name theirs (#1780).
func (b *bigqueryIAM) addTags(m *bigqueryfront.BigQueryIAM, reg *resourcemanager.Registry, projects []resourcemanager.Project) {
	keys, err := reg.AllTagKeys()
	if err != nil {
		return
	}
	byValue := map[string]iamcond.Tag{}
	for _, k := range keys {
		values, err := reg.TagValues(k.GetName())
		if err != nil {
			continue
		}
		for _, v := range values {
			t := iamcond.Tag{NamespacedKey: k.GetNamespacedName(), ShortValue: v.GetShortName(), KeyID: k.GetName(), ValueID: v.GetName()}
			byValue[v.GetName()] = t
			m.TagValues[v.GetNamespacedName()] = t
		}
	}
	bindings, err := reg.TagBindingsUnder(bigqueryfront.BigQueryTagPrefix)
	if err != nil {
		return
	}
	ids := map[string]string{}
	for _, p := range projects {
		ids[p.ProjectID] = p.ProjectID
		if p.Number != 0 {
			ids[strconv.FormatInt(p.Number, 10)] = p.ProjectID
		}
	}
	for _, bd := range bindings {
		t, ok := byValue[bd.GetTagValue()]
		if !ok {
			continue
		}
		rest, ok := strings.CutPrefix(bd.GetParent(), bigqueryfront.BigQueryTagPrefix+"projects/")
		if !ok {
			continue
		}
		id, tail, _ := strings.Cut(rest, "/")
		if pid, ok := ids[id]; ok {
			id = pid
		}
		name := bigqueryfront.BigQueryTagPrefix + "projects/" + id + "/" + tail
		m.Tags[name] = append(m.Tags[name], t)
	}
}

// addBindings adds bindings' roles, on project, to m for the accounts
// emails names.
func (b *bigqueryIAM) addBindings(m *bigqueryfront.BigQueryIAM, reg *resourcemanager.Registry, ix memberIndex,
	project string, bindings []iampolicy.Binding) {
	for _, bd := range bindings {
		if _, ok := bigqueryfront.BigQueryRoles[bd.Role]; !ok {
			if !resourcemanager.IsCustomRole(bd.Role) {
				continue
			}
			bq := bigqueryPerms(reg, bd.Role)
			if len(bq) == 0 {
				continue
			}
			m.CustomRoles[bd.Role] = bq
		}
		for _, member := range bd.Members {
			for _, email := range ix.accounts(member) {
				b.grantMember(m, email, project, bd)
			}
		}
	}
}

// grantMember gives email bd's role on project, or its condition.
func (b *bigqueryIAM) grantMember(m *bigqueryfront.BigQueryIAM, email, project string, bd iampolicy.Binding) {
	if bd.Condition != nil {
		if m.Conditional[email] == nil {
			m.Conditional[email] = map[string][]bigqueryfront.BigQueryIAMCondition{}
		}
		m.Conditional[email][project] = append(m.Conditional[email][project],
			bigqueryfront.BigQueryIAMCondition{Role: bd.Role, Expression: bd.Condition.Expression})
		return
	}
	if m.Roles[email] == nil {
		m.Roles[email] = map[string][]string{}
	}
	if !slices.Contains(m.Roles[email][project], bd.Role) {
		m.Roles[email][project] = append(m.Roles[email][project], bd.Role)
	}
}

// sync pushes the payload when it changed, or when force or the refresh
// interval says to.
func (b *bigqueryIAM) sync(ctx context.Context, force bool) error {
	addr := b.addr()
	if addr == "" {
		return nil
	}
	now := time.Now()
	body, err := json.Marshal(b.payload(now))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte(addr+"\x00"), body...))
	print := hex.EncodeToString(sum[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	if !force && print == b.last && now.Sub(b.lastAt) < bigqueryIAMRefresh {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+addr+bigqueryfront.BigQueryIAMPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the BigQuery front answered %s", resp.Status)
	}
	b.last, b.lastAt = print, now
	return nil
}

// start keeps the front's tokens and roles current until ctx ends, and
// pushes each token as it is minted.
func (b *bigqueryIAM) start(ctx context.Context) {
	b.creds.OnIssue(func(metadata.IssuedToken) {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = b.sync(sctx, true)
		cancel()
	})
	go func() {
		t := time.NewTicker(bigqueryIAMEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = b.sync(sctx, false)
				cancel()
			}
		}
	}()
}
