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

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// firestoreMongoIAM gives Firestore's front what a Firestore with MongoDB
// compatibility token sign-in is checked against (#1738,
// internal/firestoreadmin/mongoiam.go), as Memorystore's IAM auth gives
// its nodes theirs (memorystoreIAM, #1614): the live access tokens these
// credentials minted, and the roles each of their accounts holds on each
// project in the registry's IAM policies (serviceAccount: and user:
// members of the roles firestoreadmin.MongoRoles names). It pushes them
// to the front through Firestore's tunnel every iamEvery when they
// changed, every iamRefresh regardless (a restarted front lost them), and
// as soon as a token is minted, before it is returned.
type firestoreMongoIAM struct {
	creds    *metadata.Credentials
	registry func() *resourcemanager.Registry
	// addr is Firestore's host address, "" while its tunnel is down.
	addr   func() string
	client *http.Client

	mu     sync.Mutex
	last   string
	lastAt time.Time
}

const (
	firestoreIAMEvery   = 5 * time.Second
	firestoreIAMRefresh = 30 * time.Second
)

// payload is what the front is given now.
func (f *firestoreMongoIAM) payload(now time.Time) firestoreadmin.MongoIAM {
	m := firestoreadmin.MongoIAM{Tokens: []firestoreadmin.MongoIAMToken{}, Roles: map[string]map[string][]string{}}
	emails := map[string]bool{}
	for _, t := range f.creds.LiveTokens(now) {
		if t.Email == "" {
			continue
		}
		m.Tokens = append(m.Tokens, firestoreadmin.MongoIAMToken{Hash: t.Hash, Email: strings.ToLower(t.Email), Expires: t.Expires})
		emails[strings.ToLower(t.Email)] = true
	}
	sort.Slice(m.Tokens, func(i, j int) bool { return m.Tokens[i].Hash < m.Tokens[j].Hash })
	var reg *resourcemanager.Registry
	if f.registry != nil {
		reg = f.registry()
	}
	if reg == nil || len(emails) == 0 {
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
		for _, b := range pol.Bindings {
			if _, ok := firestoreadmin.MongoRoles[b.Role]; !ok {
				continue
			}
			for _, member := range b.Members {
				kind, email, ok := strings.Cut(member, ":")
				email = strings.ToLower(email)
				if !ok || (kind != "serviceAccount" && kind != "user") || !emails[email] {
					continue
				}
				if m.Roles[email] == nil {
					m.Roles[email] = map[string][]string{}
				}
				m.Roles[email][p.ProjectID] = append(m.Roles[email][p.ProjectID], b.Role)
			}
		}
	}
	return m
}

// sync pushes the payload when it changed, or when force or the refresh
// interval says to.
func (f *firestoreMongoIAM) sync(ctx context.Context, force bool) error {
	addr := f.addr()
	if addr == "" {
		return nil
	}
	now := time.Now()
	body, err := json.Marshal(f.payload(now))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(append([]byte(addr+"\x00"), body...))
	print := hex.EncodeToString(sum[:])
	f.mu.Lock()
	defer f.mu.Unlock()
	if !force && print == f.last && now.Sub(f.lastAt) < firestoreIAMRefresh {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+addr+firestoreadmin.MongoIAMPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the Firestore front answered %s", resp.Status)
	}
	f.last, f.lastAt = print, now
	return nil
}

// start keeps the front's tokens and roles current until ctx ends, and
// pushes each token as it is minted.
func (f *firestoreMongoIAM) start(ctx context.Context) {
	f.creds.OnIssue(func(metadata.IssuedToken) {
		// Bounded: the client asking for a token waits for this.
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = f.sync(sctx, true)
		cancel()
	})
	go func() {
		t := time.NewTicker(firestoreIAMEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = f.sync(sctx, false)
				cancel()
			}
		}
	}()
}
