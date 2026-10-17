package main

// Edit managed folder (#1505): a managed folder's one settable field is its
// rapidCacheConfig, the Rapid Caches of the bucket it names and whether each
// ingests on write. The form holds the policies one per line, prefilled from
// managedFolders.list or get, and sends managedFolders.update with each
// policy kept or set and each one taken off the list removed (a null entry),
// or a null rapidCacheConfig when none is left.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// rapidPoliciesText is a managed folder's policies, one per line: the Rapid
// Cache's ID (its zone), then "ingest-on-write" when it ingests on write.
func rapidPoliciesText(f *storagev1.ManagedFolder) string {
	if f == nil || f.RapidCacheConfig == nil {
		return ""
	}
	keys := make([]string, 0, len(f.RapidCacheConfig.Policies))
	for k := range f.RapidCacheConfig.Policies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		line := k
		if ingestEnabled(f.RapidCacheConfig.Policies[k].IngestOnWrite) {
			line += " ingest-on-write"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func ingestEnabled(v string) bool {
	v = strings.ToLower(v)
	return v == "enabled" || v == "ingest_on_write_enabled"
}

// editManagedFolderAction is Edit managed folder, prefilled from f.
func editManagedFolderAction(f *storagev1.ManagedFolder) console.Action {
	return console.Action{ID: "editmanagedfolder", Label: "Edit managed folder", Fields: []console.Field{
		{Name: "rapidCaches", Label: "Rapid cache policies", Type: "textarea", Default: rapidPoliciesText(f),
			Help: "One per line: a Rapid Cache of the bucket, by its zone (us-central1-a), then ingest-on-write to have it ingest " +
				"the objects written under this folder. A line taken off removes that policy; an empty list removes them all. " + noLocalCache},
	}}
}

// managedFolderAt is the bucket and managed folder name an Edit managed
// folder path names: a managed folder's row, or an HNS folder's that is one.
func managedFolderAt(path []string) (bucket, name string, ok bool) {
	if len(path) != 3 || (path[0] != managedFolderPage && path[0] != folderPage) {
		return "", "", false
	}
	return path[1], path[2], true
}

// editManagedFolder is managedFolders.update of the folder's
// rapidCacheConfig.
func (p storageProvider) editManagedFolder(ctx context.Context, path []string, values map[string]string) error {
	bucket, name, ok := managedFolderAt(path)
	if !ok {
		return errors.New("a managed folder is edited from its row")
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return err
	}
	cur, err := s.ManagedFolders.Get(bucket, name).Context(ctx).Do()
	if err != nil {
		return err
	}
	want := map[string]storagev1.RapidCachePolicy{}
	for _, line := range strings.Split(values["rapidCaches"], "\n") {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		pol := storagev1.RapidCachePolicy{RapidCacheId: parts[0]}
		for _, opt := range parts[1:] {
			if opt != "ingest-on-write" {
				return fmt.Errorf("%q after %s is not ingest-on-write", opt, parts[0])
			}
			pol.IngestOnWrite = "enabled"
		}
		want[parts[0]] = pol
	}
	upd := &storagev1.ManagedFolder{}
	if len(want) == 0 {
		upd.NullFields = []string{"RapidCacheConfig"}
	} else {
		cfg := &storagev1.RapidCacheConfig{Policies: want}
		if cur.RapidCacheConfig != nil {
			for k := range cur.RapidCacheConfig.Policies {
				if _, kept := want[k]; !kept {
					cfg.NullFields = append(cfg.NullFields, "Policies."+k)
				}
			}
		}
		upd.RapidCacheConfig = cfg
	}
	_, err = s.ManagedFolders.Update(bucket, name, upd).IfMetagenerationMatch(cur.Metageneration).Context(ctx).Do()
	return err
}
