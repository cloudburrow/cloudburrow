package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// A deleted service account in a dataset's access list (#1751).
//
// IAM documents that a deleted principal's bindings name it as
// deleted:serviceAccount:EMAIL?uid=ID, that undeleting the account restores
// them and that purging it drops them
// (https://docs.cloud.google.com/iam/docs/allow-policies). A dataset's
// access entries are its IAM policy's bindings, and the datasets reference
// (https://docs.cloud.google.com/bigquery/docs/reference/rest/v2/datasets)
// says which field holds which member: userByEmail "maps to IAM policy
// member user:EMAIL or serviceAccount:EMAIL", and iamMember is "some other
// type of member that appears in the IAM Policy but isn't a user, group,
// domain, or special group".
//
// So when the host sends a rewrite from serviceAccount:EMAIL to the
// deleted member, every entry granting a role to EMAIL (as userByEmail, or
// as iamMember serviceAccount:EMAIL) becomes an iamMember entry naming the
// deleted member, its role and condition kept. Undelete turns it back into
// userByEmail EMAIL, the field the reference names for a service account,
// and purge drops it. An entry that would repeat another (the same role,
// member and condition) is dropped.
//
// UNVERIFIED (re-read 2026-11-17): neither page shows a deleted principal in an
// access list, so the iamMember shape, that undelete answers userByEmail
// (rather than the field the entry was granted with), and that the rewrite
// leaves the dataset's lastModifiedTime as it was are CloudBurrow's
// reading of the mapping above, not measured against BigQuery.
//
// The engine keeps the datasets, so the front reads each project's
// datasets from it and saves each one whose list changed with
// datasets.update (patch 0011 saves it).

// rewriteDatasetAccess rewrites from to to (dropping it when to is empty)
// in the access list of every dataset next keeps.
func rewriteDatasetAccess(ctx context.Context, next http.Handler, from, to string) error {
	var projects struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if status, err := engineJSON(ctx, next, http.MethodGet, "/projects", nil, &projects); err != nil {
		if status == http.StatusNotFound {
			return nil
		}
		return err
	}
	var errs []error
	for _, p := range projects.Projects {
		if p.ID == "" {
			continue
		}
		base := "/projects/" + url.PathEscape(p.ID) + "/datasets"
		var list struct {
			Datasets []struct {
				DatasetReference struct {
					DatasetID string `json:"datasetId"`
				} `json:"datasetReference"`
			} `json:"datasets"`
		}
		if _, err := engineJSON(ctx, next, http.MethodGet, base+"?all=true", nil, &list); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, d := range list.Datasets {
			id := d.DatasetReference.DatasetID
			if id == "" {
				continue
			}
			path := base + "/" + url.PathEscape(id)
			var content map[string]json.RawMessage
			if _, err := engineJSON(ctx, next, http.MethodGet, path, nil, &content); err != nil {
				errs = append(errs, err)
				continue
			}
			access, changed, err := rewriteAccessList(content["access"], from, to)
			if err != nil || !changed {
				errs = append(errs, err)
				continue
			}
			content["access"] = access
			body, err := json.Marshal(content)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := engineJSON(ctx, next, http.MethodPut, path, body, nil); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// rewriteAccessList returns the access list raw with from rewritten to to
// (above), and whether it changed.
func rewriteAccessList(raw json.RawMessage, from, to string) (json.RawMessage, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || isNull(raw) {
		return raw, false, nil
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return raw, false, fmt.Errorf("a dataset's access list: %w", err)
	}
	fromEmail, fromIsAccount := strings.CutPrefix(from, "serviceAccount:")
	changed := false
	out := make([]map[string]json.RawMessage, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		matches := accessString(e["iamMember"]) == from
		if fromIsAccount && strings.EqualFold(accessString(e["userByEmail"]), fromEmail) {
			matches = true
		}
		if matches {
			changed = true
			delete(e, "iamMember")
			delete(e, "userByEmail")
			switch email, ok := strings.CutPrefix(to, "serviceAccount:"); {
			case to == "":
				continue // purged
			case ok:
				e["userByEmail"], _ = json.Marshal(email)
			default:
				e["iamMember"], _ = json.Marshal(to)
			}
		}
		key, _ := json.Marshal(e) // sorted keys: one entry, one key
		if seen[string(key)] {
			changed = true
			continue
		}
		seen[string(key)] = true
		out = append(out, e)
	}
	if !changed {
		return raw, false, nil
	}
	b, err := json.Marshal(out)
	return b, err == nil, err
}

// accessString is v as a JSON string, or "".
func accessString(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// engineJSON sends method path (with body) to the engine next, and decodes
// its 200 answer into v when v is not nil.
func engineJSON(ctx context.Context, next http.Handler, method, path string, body []byte, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := newRecorder()
	next.ServeHTTP(rec, req)
	if rec.status != 0 && rec.status != http.StatusOK {
		return rec.status, fmt.Errorf("%s %s: %d", method, path, rec.status)
	}
	if v == nil {
		return http.StatusOK, nil
	}
	return http.StatusOK, json.Unmarshal(rec.body.Bytes(), v)
}
