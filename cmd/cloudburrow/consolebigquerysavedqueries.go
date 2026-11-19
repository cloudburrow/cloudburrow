package main

// BigQuery Studio's saved queries (#1861). Google keeps a saved query as a
// Dataform code asset (Save > Save query, with a Name and a Region; it is
// listed in the Files pane and shared with Code Owner, Code Editor and Code
// Viewer roles, https://docs.cloud.google.com/bigquery/docs/work-with-saved-queries,
// read 2026-11-19). CloudBurrow serves neither Dataform nor the BigQuery
// Studio assets API, so a saved query here is CloudBurrow's own: a record
// in the instance's state directory, listed under the Explorer's Queries
// node and reachable by no API. Each save is a new version, as Google's is.
// Manage permissions is not offered: there is no IAM on a record that only
// the console reads.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// bqSavedQueriesFile is the record's name in the instance directory.
const bqSavedQueriesFile = "bigquery-saved-queries.json"

// bqSavedQueryLimit bounds a saved statement, as the editor's own body is.
const bqSavedQueryLimit = 1 << 16

var bqSavedMu sync.Mutex

type bqSavedQuery struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Query   string `json:"query"`
	Version int    `json:"version"`
	Created string `json:"created"`
	Updated string `json:"updated"`
}

type bqSavedQueryStore struct{ path string }

func (s bqSavedQueryStore) read() ([]bqSavedQuery, error) {
	if s.path == "" {
		return nil, errors.New("saved queries are kept in the instance's state directory, which this console has none of")
	}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []bqSavedQuery
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("the saved queries record %s: %w", s.path, err)
	}
	return all, nil
}

func (s bqSavedQueryStore) write(all []bqSavedQuery) error {
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// list is the project's saved queries, by name.
func (s bqSavedQueryStore) list(project string) ([]bqSavedQuery, error) {
	bqSavedMu.Lock()
	defer bqSavedMu.Unlock()
	all, err := s.read()
	if err != nil {
		return nil, err
	}
	out := []bqSavedQuery{}
	for _, q := range all {
		if q.Project == project {
			out = append(out, q)
		}
	}
	slices.SortFunc(out, func(a, b bqSavedQuery) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, nil
}

// save is Save query: a new saved query without an ID, a new version of
// the one with it. A name is unique in its project, as a file's in a folder.
func (s bqSavedQueryStore) save(project string, in bqSavedQuery, now time.Time) (bqSavedQuery, error) {
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "":
		return in, errors.New("a saved query needs a name")
	case len(in.Name) > 256:
		return in, errors.New("a saved query's name is at most 256 characters")
	case len(in.Query) > bqSavedQueryLimit:
		return in, fmt.Errorf("a saved query is at most %d bytes", bqSavedQueryLimit)
	}
	if in.Region != "" && !bqKnownLocation(in.Region) {
		return in, fmt.Errorf("%q is not a BigQuery region", in.Region)
	}
	bqSavedMu.Lock()
	defer bqSavedMu.Unlock()
	all, err := s.read()
	if err != nil {
		return in, err
	}
	stamp := now.UTC().Format(time.RFC3339)
	for _, q := range all {
		if q.Project == project && q.ID != in.ID && strings.EqualFold(q.Name, in.Name) {
			return in, fmt.Errorf("a saved query named %q already exists", q.Name)
		}
	}
	if in.ID != "" {
		for i, q := range all {
			if q.Project != project || q.ID != in.ID {
				continue
			}
			if in.Region != "" && q.Region != "" && in.Region != q.Region {
				return in, errors.New("a saved query's region cannot be changed after it is created")
			}
			q.Name, q.Query, q.Updated = in.Name, in.Query, stamp
			q.Version++
			all[i] = q
			return q, s.write(all)
		}
		return in, fmt.Errorf("no saved query %s", in.ID)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return in, err
	}
	q := bqSavedQuery{ID: hex.EncodeToString(id[:]), Project: project, Name: in.Name, Region: in.Region,
		Query: in.Query, Version: 1, Created: stamp, Updated: stamp}
	return q, s.write(append(all, q))
}

func (s bqSavedQueryStore) remove(project, id string) error {
	bqSavedMu.Lock()
	defer bqSavedMu.Unlock()
	all, err := s.read()
	if err != nil {
		return err
	}
	for i, q := range all {
		if q.Project == project && q.ID == id {
			return s.write(slices.Delete(all, i, i+1))
		}
	}
	return fmt.Errorf("no saved query %s", id)
}
