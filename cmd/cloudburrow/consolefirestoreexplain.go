package main

// Query Explain of an Enterprise edition database's MongoDB queries
// (#1738), as Google's console runs it: the front runs the explain command
// (internal/firestoreadmin/mongoexplainhttp.go) and the console shows the
// plan and, when asked, the execution stats.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

func explainQueryFields() []console.Field {
	return []console.Field{
		{Name: "collection", Label: "Collection", Type: "text", Required: true, Pattern: firestoreCollectionPattern},
		{Name: "command", Label: "Command", Type: "select", Options: []string{"find", "aggregate"}, Default: "find"},
		{Name: "query", Label: "Filter or pipeline", Type: "textarea", Default: "{}",
			Help: "MongoDB Extended JSON: a find's filter, such as {\"year\": {\"$gt\": 1900}}, or an aggregate's pipeline, such as [{\"$match\": {\"genre\": \"sf\"}}]."},
		{Name: "sort", Label: "Sort (find, optional)", Type: "text", Help: "Extended JSON, such as {\"year\": -1}."},
		{Name: "limit", Label: "Limit (find, optional)", Type: "text", Pattern: `^[0-9]*$`},
		{Name: "verbosity", Label: "Verbosity", Type: "select", Options: []string{"queryPlanner", "executionStats", "allPlansExecution"},
			Default: "executionStats", Help: "queryPlanner plans the query without running it; executionStats and allPlansExecution run it and add its stats."},
	}
}

// explainCommand is the explained command's Extended JSON from the form.
func explainCommand(values map[string]string) (string, error) {
	coll := strings.TrimSpace(values["collection"])
	if coll == "" {
		return "", errors.New("a collection is required")
	}
	q := strings.TrimSpace(values["query"])
	cmd := bson.D{}
	switch values["command"] {
	case "", "find":
		if q == "" {
			q = "{}"
		}
		var filter bson.D
		if err := bson.UnmarshalExtJSON([]byte(q), false, &filter); err != nil {
			return "", fmt.Errorf("the filter is not an Extended JSON document: %w", err)
		}
		cmd = bson.D{{Key: "find", Value: coll}, {Key: "filter", Value: filter}}
		if s := strings.TrimSpace(values["sort"]); s != "" {
			var sort bson.D
			if err := bson.UnmarshalExtJSON([]byte(s), false, &sort); err != nil {
				return "", fmt.Errorf("the sort is not an Extended JSON document: %w", err)
			}
			cmd = append(cmd, bson.E{Key: "sort", Value: sort})
		}
		if l := strings.TrimSpace(values["limit"]); l != "" {
			var n int64
			if _, err := fmt.Sscan(l, &n); err != nil {
				return "", fmt.Errorf("limit %q is not a number", l)
			}
			cmd = append(cmd, bson.E{Key: "limit", Value: n})
		}
	case "aggregate":
		if q == "" || q == "{}" {
			q = "[]"
		}
		var wrap bson.D
		if err := bson.UnmarshalExtJSON([]byte(`{"p":`+q+`}`), false, &wrap); err != nil {
			return "", fmt.Errorf("the pipeline is not an Extended JSON array: %w", err)
		}
		pipeline, ok := wrap[0].Value.(bson.A)
		if !ok {
			return "", errors.New("the pipeline is not an Extended JSON array")
		}
		cmd = bson.D{{Key: "aggregate", Value: coll}, {Key: "pipeline", Value: pipeline}, {Key: "cursor", Value: bson.D{}}}
	default:
		return "", fmt.Errorf("command %q is not find or aggregate", values["command"])
	}
	raw, err := bson.MarshalExtJSON(cmd, false, false)
	return string(raw), err
}

// explainQuery runs Query Explain on the database db and lists the
// explain's sections.
func (p firestoreDatabasesProvider) explainQuery(ctx context.Context, db string, values map[string]string) (*console.Listing, error) {
	cmd, err := explainCommand(values)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(firestoreadmin.MongoExplainRequest{Database: db, Command: cmd, Verbosity: values["verbosity"]})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+p.endpoint+firestoreadmin.MongoExplainPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Firestore: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Firestore answered %s", resp.Status)
	}
	var out firestoreadmin.MongoExplainResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	var doc bson.D
	if err := bson.UnmarshalExtJSON([]byte(out.Explain), false, &doc); err != nil {
		return nil, err
	}
	list := &console.Listing{Columns: []string{"Value"}, NameColumn: "Section", Noun: "explain sections"}
	for _, e := range doc {
		raw, err := bson.MarshalExtJSONIndent(bson.D{{Key: e.Key, Value: e.Value}}, false, false, "", "  ")
		if err != nil {
			return nil, err
		}
		v := strings.TrimSpace(string(raw))
		list.Items = append(list.Items, console.Resource{Name: e.Key, Fields: map[string]string{"Value": v}})
	}
	list.Total = len(list.Items)
	list.Note = "Query Explain of " + cmd + ", " + values["verbosity"] + ", run by Firestore with MongoDB compatibility's explain command."
	return list, nil
}
