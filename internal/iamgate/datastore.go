package iamgate

import (
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Datastore is the Datastore API's table (Firestore in Datastore mode):
// "Identity and Access Management (IAM)", "Required permissions for API
// methods" (https://docs.cloud.google.com/datastore/docs/access/iam, read
// 2026-11-20), each permission checked on the database the request names
// (project_id and database_id, "(default)" for none), whose project's,
// folders' and organization's policies apply:
//
//   - Lookup datastore.entities.get, and datastore.statistics.get for a
//     __Stat_* key;
//   - RunQuery and RunAggregationQuery datastore.entities.list, with
//     datastore.entities.get unless the query is keys-only; on a __Stat_*
//     kind datastore.statistics.get and list, on __namespace__
//     datastore.namespaces.get and list, kindless entities.get and list
//     and statistics.get and list (a GQL query by its FROM and SELECT);
//   - Commit by each mutation (insert datastore.entities.create, update
//     .update, upsert both, delete .delete), and datastore.databases.get
//     with none; BeginTransaction and Rollback datastore.databases.get;
//   - AllocateIds datastore.entities.allocateIds, and ReserveIds the same
//     (the table does not list it; it reserves IDs as AllocateIds does).
//
// The Datastore Admin API's references name no permission; its methods
// take the permissions of the roles that page lists for them:
// export/import datastore.databases.export and .import, indexes
// datastore.schemas.*, operations datastore.operations.*, on the database
// the call's routing header names. A denial is PERMISSION_DENIED "Missing
// or insufficient permissions.", as Firestore's.
var Datastore = &Service{
	Name: "datastore.googleapis.com",
	Type: func(resource string) string {
		if strings.Contains(resource, "/databases/") {
			return "firestore.googleapis.com/Database"
		}
		return ""
	},
	Methods: datastoreMethods(),
	Deny: func(string, string) error {
		return status.Error(codes.PermissionDenied, "Missing or insufficient permissions.")
	},
}

const (
	dsData  = "/google.datastore.v1.Datastore/"
	dsAdmin = "/google.datastore.admin.v1.DatastoreAdmin/"
)

// datastoreDB is the database a Datastore data request names.
func datastoreDB(m protoreflect.Message) string {
	p := String(m, "project_id")
	if p == "" {
		return ""
	}
	d := String(m, "database_id")
	if d == "" {
		d = "(default)"
	}
	return "projects/" + p + "/databases/" + d
}

// dsOn is a rule of fixed permissions on a data request's database.
func dsOn(perms ...string) Rule {
	return func(m protoreflect.Message) []Check {
		db := datastoreDB(m)
		if db == "" {
			return nil
		}
		return checksOn(db, perms...)
	}
}

// dsAdminOn is a rule of fixed permissions on the database an Admin API
// call's routing header names, in the project its request does (field).
func dsAdminOn(field string, perms ...string) Rule {
	return func(m protoreflect.Message) []Check {
		p := String(m, field)
		if field == "name" {
			p = Project(p)
		}
		if p == "" {
			return nil
		}
		return checksOn("projects/"+p+"/databases/"+Routed, perms...)
	}
}

func checksOn(res string, perms ...string) []Check {
	out := make([]Check, len(perms))
	for i, p := range perms {
		out[i] = Check{p, res}
	}
	return out
}

func datastoreMethods() map[string]Rule {
	return map[string]Rule{
		dsData + "Lookup":              datastoreLookup,
		dsData + "RunQuery":            datastoreQuery,
		dsData + "RunAggregationQuery": datastoreQuery,
		dsData + "BeginTransaction":    dsOn("datastore.databases.get"),
		dsData + "Rollback":            dsOn("datastore.databases.get"),
		dsData + "Commit":              datastoreCommit,
		dsData + "AllocateIds":         dsOn("datastore.entities.allocateIds"),
		dsData + "ReserveIds":          dsOn("datastore.entities.allocateIds"),

		dsAdmin + "ExportEntities": dsAdminOn("project_id", "datastore.databases.export"),
		dsAdmin + "ImportEntities": dsAdminOn("project_id", "datastore.databases.import"),
		dsAdmin + "CreateIndex":    dsAdminOn("project_id", "datastore.schemas.create"),
		dsAdmin + "GetIndex":       dsAdminOn("project_id", "datastore.schemas.get"),
		dsAdmin + "ListIndexes":    dsAdminOn("project_id", "datastore.schemas.list"),
		dsAdmin + "DeleteIndex":    dsAdminOn("project_id", "datastore.schemas.delete"),

		fsOps + "GetOperation":    dsAdminOn("name", "datastore.operations.get"),
		fsOps + "ListOperations":  dsAdminOn("name", "datastore.operations.list"),
		fsOps + "CancelOperation": dsAdminOn("name", "datastore.operations.cancel"),
		fsOps + "DeleteOperation": dsAdminOn("name", "datastore.operations.delete"),
	}
}

// keyKind is the kind of a Key message's last path element.
func keyKind(k protoreflect.Message) string {
	path := List(k, "path")
	if len(path) == 0 {
		return ""
	}
	return String(path[len(path)-1], "kind")
}

func datastoreLookup(m protoreflect.Message) []Check {
	db := datastoreDB(m)
	if db == "" {
		return nil
	}
	out := checksOn(db, "datastore.entities.get")
	for _, k := range List(m, "keys") {
		if strings.HasPrefix(keyKind(k), "__Stat_") {
			return append(out, Check{"datastore.statistics.get", db})
		}
	}
	return out
}

// gqlFrom reads a GQL query's kind and whether it selects only __key__.
var (
	gqlFrom    = regexp.MustCompile("(?i)\\bFROM\\s+(`(?:[^`]|``)*`|[A-Za-z_$][A-Za-z0-9_$]*)")
	gqlKeyOnly = regexp.MustCompile(`(?i)^\s*SELECT\s+__key__\s+(FROM|WHERE|ORDER|LIMIT|OFFSET|$)`)
)

// datastoreQuery is RunQuery's and RunAggregationQuery's checks, by the
// query's kind and projection.
func datastoreQuery(m protoreflect.Message) []Check {
	db := datastoreDB(m)
	if db == "" {
		return nil
	}
	var kind string
	keysOnly := false
	switch {
	case Has(m, "gql_query"):
		gql := String(m, "gql_query.query_string")
		if f := gqlFrom.FindStringSubmatch(gql); f != nil {
			kind = strings.ReplaceAll(strings.Trim(f[1], "`"), "``", "`")
		}
		keysOnly = gqlKeyOnly.MatchString(gql)
	default:
		q := queryOf(m)
		if q == nil {
			return nil
		}
		if kinds := List(q, "kind"); len(kinds) > 0 {
			kind = String(kinds[0], "name")
		}
		proj := List(q, "projection")
		keysOnly = len(proj) == 1 && String(proj[0], "property.name") == "__key__"
	}
	switch {
	case kind == "":
		return checksOn(db, "datastore.entities.get", "datastore.entities.list", "datastore.statistics.get", "datastore.statistics.list")
	case kind == "__namespace__":
		return checksOn(db, "datastore.namespaces.get", "datastore.namespaces.list")
	case strings.HasPrefix(kind, "__Stat_"):
		return checksOn(db, "datastore.statistics.get", "datastore.statistics.list")
	case keysOnly:
		return checksOn(db, "datastore.entities.list")
	}
	return checksOn(db, "datastore.entities.get", "datastore.entities.list")
}

// queryOf is a RunQuery's query, or a RunAggregationQuery's nested one.
func queryOf(m protoreflect.Message) protoreflect.Message {
	for _, path := range [][]string{{"query"}, {"aggregation_query", "nested_query"}} {
		cur := m
		for _, p := range path {
			fd := cur.Descriptor().Fields().ByName(protoreflect.Name(p))
			if fd == nil || fd.Kind() != protoreflect.MessageKind || !cur.Has(fd) {
				cur = nil
				break
			}
			cur = cur.Get(fd).Message()
		}
		if cur != nil {
			return cur
		}
	}
	return nil
}

func datastoreCommit(m protoreflect.Message) []Check {
	db := datastoreDB(m)
	if db == "" {
		return nil
	}
	need := map[string]bool{}
	for _, mu := range List(m, "mutations") {
		switch {
		case Has(mu, "insert"):
			need["datastore.entities.create"] = true
		case Has(mu, "update"):
			need["datastore.entities.update"] = true
		case Has(mu, "upsert"):
			need["datastore.entities.create"], need["datastore.entities.update"] = true, true
		case Has(mu, "delete"):
			need["datastore.entities.delete"] = true
		}
	}
	if len(need) == 0 {
		return checksOn(db, "datastore.databases.get")
	}
	var out []Check
	for _, p := range []string{"datastore.entities.create", "datastore.entities.update", "datastore.entities.delete"} {
		if need[p] {
			out = append(out, Check{p, db})
		}
	}
	return out
}
