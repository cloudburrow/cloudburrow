package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/datastore"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Per-project resets for the Spanner, Bigtable, Firestore and Datastore
// emulators (#1540), so a purged project's data goes as it does on Google.

// ResetProject deletes one project's data through the front's
// frontstate.ResetPath?project=: the front drops its records and copies for
// the project, and deletes the project's instances, tables, databases or
// documents in the emulator next to it, through the emulator's own API.
func (s *frontStateResetter) ResetProject(ctx context.Context, project string) error {
	if s.tunnel == nil || s.tunnel.HostAddr() == "" {
		return fmt.Errorf("the %s tunnel is not running", s.name)
	}
	u := "http://" + s.tunnel.HostAddr() + frontstate.ResetPath + "?project=" + url.QueryEscape(project)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("reset project %s in the %s front: %s", project, s.name, resp.Status)
	}
	return nil
}

// datastoreResetter deletes a project's entities from the Datastore
// emulator, every namespace's, through the Datastore API itself: the
// emulator keeps entities by project and has no per-project reset of its
// own (its /reset clears every project). It is a ProjectResetter only in
// spirit: Reset, every project, reaches the projects CloudBurrow knows, as
// Pub/Sub's does.
type datastoreResetter struct {
	tunnel   *netfwd.Forwarder
	projects func() []string
}

func (d *datastoreResetter) Name() string { return "datastore" }

func (d *datastoreResetter) Reset(ctx context.Context) error {
	var errs []error
	for _, p := range d.projects() {
		if err := d.ResetProject(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// ResetProject deletes every entity of project, in every namespace.
func (d *datastoreResetter) ResetProject(ctx context.Context, project string) error {
	if d.tunnel == nil || d.tunnel.HostAddr() == "" {
		return errors.New("the datastore tunnel is not running")
	}
	c, err := datastore.NewClient(ctx, project, localOpts(d.tunnel.HostAddr())...)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	namespaces := []string{""}
	nsKeys, err := c.GetAll(ctx, datastore.NewQuery("__namespace__").KeysOnly(), nil)
	if err == nil {
		for _, k := range nsKeys {
			if k.Name != "" {
				namespaces = append(namespaces, k.Name)
			}
		}
	}
	for _, ns := range namespaces {
		keys, err := c.GetAll(ctx, datastore.NewQuery("").Namespace(ns).KeysOnly(), nil)
		if err != nil {
			return fmt.Errorf("list the entities of namespace %q: %w", ns, err)
		}
		var mine []*datastore.Key
		for _, k := range keys {
			if !strings.HasPrefix(k.Kind, "__") {
				mine = append(mine, k)
			}
		}
		// A commit takes at most 500 mutations.
		for len(mine) > 0 {
			n := min(len(mine), 500)
			if err := c.DeleteMulti(ctx, mine[:n]); err != nil {
				return fmt.Errorf("delete the entities of namespace %q: %w", ns, err)
			}
			mine = mine[n:]
		}
	}
	return nil
}
