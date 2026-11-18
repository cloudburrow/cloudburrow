package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// bigqueryIAM gives BigQuery's front what its per-project IAM check reads
// (#1767, internal/bigqueryfront/iamcheck.go), as firestoreMongoIAM gives
// Firestore's: the live access tokens these credentials minted, and the
// roles each of their accounts holds on each project in the registry's
// IAM policies (serviceAccount: and user: members of the roles
// bigqueryfront.BigQueryRoles names, and of custom roles with bigquery.
// permissions). A conditional binding grants nothing here. The tokens of
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
		CustomRoles: map[string][]string{}}
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
	for _, p := range projects {
		pol, err := reg.IAMPolicy("projects/" + p.ProjectID)
		if err != nil || pol == nil {
			continue
		}
		for _, bd := range pol.Bindings {
			if bd.Condition != nil {
				continue
			}
			if _, ok := bigqueryfront.BigQueryRoles[bd.Role]; !ok {
				perms, custom := reg.RolePermissions(bd.Role)
				if !custom {
					continue
				}
				var bq []string
				for _, p := range perms {
					if strings.HasPrefix(p, "bigquery.") {
						bq = append(bq, p)
					}
				}
				if len(bq) == 0 {
					continue
				}
				m.CustomRoles[bd.Role] = bq
			}
			for _, member := range bd.Members {
				kind, email, ok := strings.Cut(member, ":")
				email = strings.ToLower(email)
				if !ok || (kind != "serviceAccount" && kind != "user") || !emails[email] {
					continue
				}
				if m.Roles[email] == nil {
					m.Roles[email] = map[string][]string{}
				}
				m.Roles[email][p.ProjectID] = append(m.Roles[email][p.ProjectID], bd.Role)
			}
		}
	}
	return m
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
