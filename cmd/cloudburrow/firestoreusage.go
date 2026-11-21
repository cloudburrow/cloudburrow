package main

// Firestore's usage and query statistics (#2048): the counts the Firestore
// front keeps (internal/firestoreadmin, usage.go), read through the host's
// tunnel to it, for Cloud Monitoring's document metrics and the console's
// Usage and Query insights pages.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

var firestoreUsageClient = &http.Client{Timeout: 10 * time.Second}

// firestoreUsage reads the front at addr's minutes since since.
func firestoreUsage(ctx context.Context, addr string, since time.Time) ([]firestoreadmin.UsageMinute, error) {
	if addr == "" {
		return nil, fmt.Errorf("Firestore is not running")
	}
	u := "http://" + addr + firestoreadmin.UsagePath + "?since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := firestoreUsageClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the Firestore front answered %s", res.Status)
	}
	var out struct {
		Minutes []firestoreadmin.UsageMinute `json:"minutes"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Minutes, nil
}

// firestoreLocations are project's databases' locations, by database ID,
// from the front's Admin API.
func firestoreLocations(ctx context.Context, addr, project string) map[string]string {
	out := map[string]string{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/projects/"+url.PathEscape(project)+"/databases", nil)
	if err != nil {
		return out
	}
	res, err := firestoreUsageClient.Do(req)
	if err != nil {
		return out
	}
	defer res.Body.Close()
	var body struct {
		Databases []struct {
			Name       string `json:"name"`
			LocationID string `json:"locationId"`
		} `json:"databases"`
	}
	if res.StatusCode == http.StatusOK && json.NewDecoder(res.Body).Decode(&body) == nil {
		for _, d := range body.Databases {
			out[d.Name[strings.LastIndexByte(d.Name, '/')+1:]] = d.LocationID
		}
	}
	return out
}

// monitoringFirestore is the Firestore front, as Cloud Monitoring reads it.
type monitoringFirestore struct{ addr func() string }

func (m monitoringFirestore) FirestoreMinutes(project string, from, to time.Time) ([]monitoring.FirestoreMinute, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addr := m.addr()
	if addr == "" {
		return nil, nil
	}
	minutes, err := firestoreUsage(ctx, addr, from)
	if err != nil {
		// Firestore not (yet) reachable: no points, as for a database
		// with no traffic.
		return nil, nil
	}
	var locs map[string]string
	var out []monitoring.FirestoreMinute
	prefix := "projects/" + project + "/databases/"
	for _, um := range minutes {
		if um.Start.After(to) {
			continue
		}
		for name, d := range um.Databases {
			id, ok := strings.CutPrefix(name, prefix)
			if !ok {
				continue
			}
			if locs == nil {
				locs = firestoreLocations(ctx, addr, project)
			}
			fm := monitoring.FirestoreMinute{Start: um.Start, Project: project, Database: id, Location: locs[id],
				Reads: d.Reads, Writes: d.Writes, Deletes: d.Deletes}
			for _, q := range d.Queries {
				var qm monitoring.FirestoreQueryMinute
				qm.Method = q.Method
				for i, r := range q.ResultCounts {
					qm.Results = append(qm.Results, float64(r))
					if i < len(q.ScannedCounts) {
						qm.Scanned = append(qm.Scanned, float64(q.ScannedCounts[i]))
					}
				}
				fm.Queries = append(fm.Queries, qm)
			}
			out = append(out, fm)
		}
	}
	return out, nil
}
