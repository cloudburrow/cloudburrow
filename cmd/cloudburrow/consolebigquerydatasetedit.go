package main

// BigQuery: Edit dataset (#1205).
//
// datasets.patch saves since #1205 (bigquery-emulator patch 0010 and the
// front's internal/bigqueryfront/datasetupdate.go,
// TestBigQueryDatasetUpdatesAreSaved), so the dataset's page edits what it
// saves: the friendly name, description, labels, default table and
// partition expirations, default collation, time travel window and access.
// The form is drawn from the dataset as it is and sent as the Go client's
// Dataset.Update of the fields that changed, under the etag the form was
// drawn with, so a change made elsewhere in between is refused (412) rather
// than overwritten.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const actEditDataset = "editdataset"

// bigqueryTimeTravelOptions are the windows BigQuery takes, in hours.
var bigqueryTimeTravelOptions = []string{"48", "72", "96", "120", "144", "168"}

// bigqueryCollationOptions are the default collations BigQuery takes: none,
// or case-insensitive.
var bigqueryCollationOptions = []string{"", "und:ci"}

// datasetAccessEntry is one access entry as the form shows it: the REST
// resource's role and one entity field.
type datasetAccessEntry struct {
	Role         string `json:"role"`
	UserByEmail  string `json:"userByEmail,omitempty"`
	GroupByEmail string `json:"groupByEmail,omitempty"`
	Domain       string `json:"domain,omitempty"`
	SpecialGroup string `json:"specialGroup,omitempty"`
	IAMMember    string `json:"iamMember,omitempty"`
}

func formatAccess(entries []*bigquery.AccessEntry) string {
	var out []datasetAccessEntry
	for _, a := range entries {
		e := datasetAccessEntry{Role: string(a.Role)}
		switch a.EntityType {
		case bigquery.UserEmailEntity:
			e.UserByEmail = a.Entity
		case bigquery.GroupEmailEntity:
			e.GroupByEmail = a.Entity
		case bigquery.DomainEntity:
			e.Domain = a.Entity
		case bigquery.SpecialGroupEntity:
			e.SpecialGroup = a.Entity
		case bigquery.IAMMemberEntity:
			e.IAMMember = a.Entity
		default:
			continue // a view, routine or dataset entry: not edited here
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return ""
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}

// parseAccess reads the access field: a JSON array of entries, each a role
// and exactly one of userByEmail, groupByEmail, domain, specialGroup and
// iamMember. Empty is no entries.
func parseAccess(raw string) ([]*bigquery.AccessEntry, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var in []datasetAccessEntry
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, fmt.Errorf("access: not a JSON array of entries: %v", err)
	}
	out := make([]*bigquery.AccessEntry, 0, len(in))
	for i, e := range in {
		var kinds []struct {
			t bigquery.EntityType
			v string
		}
		for _, k := range []struct {
			t bigquery.EntityType
			v string
		}{{bigquery.UserEmailEntity, e.UserByEmail}, {bigquery.GroupEmailEntity, e.GroupByEmail},
			{bigquery.DomainEntity, e.Domain}, {bigquery.SpecialGroupEntity, e.SpecialGroup}, {bigquery.IAMMemberEntity, e.IAMMember}} {
			if k.v != "" {
				kinds = append(kinds, k)
			}
		}
		if e.Role == "" || len(kinds) != 1 {
			return nil, fmt.Errorf("access entry %d: give a role and exactly one of userByEmail, groupByEmail, domain, specialGroup and iamMember", i+1)
		}
		out = append(out, &bigquery.AccessEntry{Role: bigquery.AccessRole(e.Role), EntityType: kinds[0].t, Entity: kinds[0].v})
	}
	return out, nil
}

// formatHours shows an expiration as whole hours, "" for none.
func formatHours(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(d/time.Hour), 10)
}

// parseHours reads an expiration in whole hours; empty is none.
func parseHours(label, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: %q is not a whole number of hours", label, raw)
	}
	return time.Duration(n) * time.Hour, nil
}

// datasetEditFields are Edit dataset's inputs, drawn from the dataset.
func datasetEditFields(md *bigquery.DatasetMetadata) []console.Field {
	travel := ""
	if md.MaxTimeTravel > 0 {
		travel = strconv.FormatInt(int64(md.MaxTimeTravel/time.Hour), 10)
	}
	return []console.Field{
		{Name: "friendlyName", Label: "Friendly name", Type: "text", Default: md.Name},
		{Name: "description", Label: "Description", Type: "textarea", Default: md.Description,
			Help: "Empty it to clear the description."},
		{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(md.Labels),
			Help: "One key=value per line. A label removed from the list is deleted from the dataset."},
		{Name: "defaultTableExpiration", Label: "Default table expiration (hours)", Type: "text", Pattern: `^[0-9]*$`,
			Default: formatHours(md.DefaultTableExpiration),
			Help:    "Tables made in the dataset from now on expire this many hours after they are made; at least 1. Empty: never."},
		{Name: "defaultPartitionExpiration", Label: "Default partition expiration (hours)", Type: "text", Pattern: `^[0-9]*$`,
			Default: formatHours(md.DefaultPartitionExpiration),
			Help:    "Partitions of new partitioned tables expire this many hours after their partition time. Empty: never."},
		{Name: "defaultCollation", Label: "Default collation", Type: "select", Options: bigqueryCollationOptions,
			Default: md.DefaultCollation, Help: "und:ci compares the STRING columns of new tables case-insensitively. Empty: case-sensitive."},
		{Name: "maxTimeTravelHours", Label: "Time travel window (hours)", Type: "select", Options: bigqueryTimeTravelOptions,
			Default: travel, Help: "How far back FOR SYSTEM_TIME AS OF can read the dataset's tables, 48 to 168 hours."},
		{Name: "access", Label: "Access", Type: "textarea", Default: formatAccess(md.Access),
			Help: `A JSON array of entries, each a role (READER, WRITER, OWNER) and one of userByEmail, groupByEmail, ` +
				`domain, specialGroup and iamMember, such as [{"role": "READER", "specialGroup": "projectReaders"}]. ` +
				"Saved as given; CloudBurrow enforces no access."},
	}
}

// datasetMetadata reads a dataset for its page's form.
func (p bigqueryProvider) datasetMetadata(ctx context.Context, datasetID string) (*bigquery.DatasetMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Dataset(datasetID).Metadata(ctx)
}

// datasetUpdate is the Dataset.Update that takes md to the form's values,
// and whether it changes anything.
func datasetUpdate(md *bigquery.DatasetMetadata, values map[string]string) (bigquery.DatasetMetadataToUpdate, bool, error) {
	var u bigquery.DatasetMetadataToUpdate
	changed := false
	if v, ok := values["friendlyName"]; ok && strings.TrimSpace(v) != md.Name {
		u.Name = strings.TrimSpace(v)
		changed = true
	}
	if v, ok := values["description"]; ok {
		if strings.TrimSpace(v) == "" {
			v = ""
		}
		if v != md.Description {
			u.Description = v
			changed = true
		}
	}
	if v, ok := values["labels"]; ok {
		labels, err := console.ParseMap(v)
		if err != nil {
			return u, false, fmt.Errorf("labels: %w", err)
		}
		if !maps.Equal(labels, md.Labels) {
			for k := range md.Labels {
				if _, keep := labels[k]; !keep {
					u.DeleteLabel(k)
				}
			}
			for k, val := range labels {
				if old, had := md.Labels[k]; !had || old != val {
					u.SetLabel(k, val)
				}
			}
			changed = true
		}
	}
	if v, ok := values["defaultTableExpiration"]; ok {
		d, err := parseHours("Default table expiration", v)
		if err != nil {
			return u, false, err
		}
		if d != md.DefaultTableExpiration {
			u.DefaultTableExpiration = d
			if d == 0 {
				u.DefaultTableExpiration = bigquery.NeverExpire
			}
			changed = true
		}
	}
	if v, ok := values["defaultPartitionExpiration"]; ok {
		d, err := parseHours("Default partition expiration", v)
		if err != nil {
			return u, false, err
		}
		if d != md.DefaultPartitionExpiration {
			u.DefaultPartitionExpiration = d
			if d == 0 {
				u.DefaultPartitionExpiration = bigquery.NeverExpire
			}
			changed = true
		}
	}
	if v, ok := values["defaultCollation"]; ok && v != md.DefaultCollation {
		u.DefaultCollation = v
		changed = true
	}
	if v, ok := values["maxTimeTravelHours"]; ok && strings.TrimSpace(v) != "" {
		h, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return u, false, fmt.Errorf("time travel window: %q is not a number of hours", v)
		}
		if d := time.Duration(h) * time.Hour; d != md.MaxTimeTravel {
			u.MaxTimeTravel = d
			changed = true
		}
	}
	if v, ok := values["access"]; ok {
		access, err := parseAccess(v)
		if err != nil {
			return u, false, err
		}
		if formatAccess(access) != formatAccess(md.Access) {
			if access == nil {
				access = []*bigquery.AccessEntry{}
			}
			u.Access = access
			changed = true
		}
	}
	return u, changed, nil
}

// editDataset saves Edit dataset's form: datasets.patch of what changed,
// under the dataset's etag.
func (p bigqueryProvider) editDataset(ctx context.Context, datasetID string, values map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	ds := c.Dataset(datasetID)
	md, err := ds.Metadata(ctx)
	if err != nil {
		return bigqueryRefusal(err)
	}
	u, changed, err := datasetUpdate(md, values)
	if err != nil {
		return err
	}
	if !changed {
		return errors.New("nothing to change: the form holds the dataset as it is")
	}
	_, err = ds.Update(ctx, u, md.ETag)
	return bigqueryRefusal(err)
}
