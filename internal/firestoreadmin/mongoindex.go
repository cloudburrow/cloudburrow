package firestoreadmin

// Firestore with MongoDB compatibility (#1703): createIndexes,
// listIndexes and dropIndexes. A MongoDB index is a composite index of the
// Admin API with the MONGODB_COMPATIBLE_API scope, on the collection's
// collection group, so `gcloud firestore indexes composite list` and the
// console show it too. As every index here, it is READY at once and
// changes no query's result; a unique index is enforced on writes.
//
// Google's limits ("MongoDB compatibility: Quotas and Limits"): 1000
// indexes a database, 100 fields an index; wildcard indexes are not
// supported (behavior differences).

import (
	"fmt"
	"sort"
	"strings"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/mql"
)

const (
	mongoMaxIndexes     = 1000
	mongoMaxIndexFields = 100
)

// defaultIndexName is MongoDB's name for an index of keys.
func defaultIndexName(keys bson.D) string {
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s_%v", k.Key, k.Value))
	}
	return strings.Join(parts, "_")
}

// mongoIndexName is a MongoDB index's name. a.mu is held.
func (a *Admin) mongoIndexName(x *adminpb.Index) string {
	if n, ok := a.mongoIndexNames[x.GetName()]; ok {
		return n
	}
	return defaultIndexName(indexKeys(x))
}

// indexKeys is an index's key document.
func indexKeys(x *adminpb.Index) bson.D {
	var keys bson.D
	for _, f := range x.GetFields() {
		if f.GetFieldPath() == "__name__" {
			continue
		}
		if f.GetSearchConfig().GetTextSpec() != nil {
			keys = append(keys, bson.E{Key: f.GetFieldPath(), Value: "text"})
			continue
		}
		if isTextIndex(x) {
			// A text index's partition field is not one of its keys.
			continue
		}
		var dir int32 = 1
		if f.GetOrder() == adminpb.Index_IndexField_DESCENDING {
			dir = -1
		}
		keys = append(keys, bson.E{Key: f.GetFieldPath(), Value: dir})
	}
	return keys
}

// mongoIndexes are a collection's MongoDB indexes, in name order. a.mu is
// held.
func (a *Admin) mongoIndexes(db, coll string) []*adminpb.Index {
	prefix := db + "/collectionGroups/" + coll + "/indexes/"
	var out []*adminpb.Index
	for n, x := range a.indexes {
		if strings.HasPrefix(n, prefix) && x.GetApiScope() == adminpb.Index_MONGODB_COMPATIBLE_API {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return a.mongoIndexName(out[i]) < a.mongoIndexName(out[j]) })
	return out
}

func (s *MongoServer) createIndexes(mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	specs, ok := mql.Lookup(cmd, "indexes").(bson.A)
	if !ok || len(specs) == 0 {
		return nil, mql.BadValue("Must specify at least one index to create")
	}
	a := s.a
	defer a.persist()
	a.mu.Lock()
	defer a.mu.Unlock()
	before := len(a.mongoIndexes(mc.db.name, coll))
	total := 0
	for _, x := range a.indexes {
		if strings.HasPrefix(x.GetName(), mc.db.name+"/") {
			total++
		}
	}
	for _, v := range specs {
		spec, ok := v.(bson.D)
		if !ok {
			return nil, mql.BadValue("Index specification must be an object")
		}
		keys, ok := mql.Lookup(spec, "key").(bson.D)
		if !ok || len(keys) == 0 {
			return nil, mql.Errorf(67, "CannotCreateIndex", "The index key pattern must be a non-empty object")
		}
		if len(keys) > mongoMaxIndexFields {
			return nil, mql.Errorf(67, "CannotCreateIndex", "An index can have at most %d fields.", mongoMaxIndexFields)
		}
		in, err := textIndexSpec(keys, spec)
		if err != nil {
			return nil, err
		}
		if in != nil {
			name, _ := mql.Lookup(spec, "name").(string)
			if name == "" {
				name = defaultIndexName(keys)
			}
			for _, x := range a.mongoIndexes(mc.db.name, coll) {
				if isTextIndex(x) {
					if a.mongoIndexName(x) == name && mql.ValuesEqual(indexKeys(x), keys) {
						name = ""
						break
					}
					return nil, mql.Errorf(85, "IndexOptionsConflict", "An equivalent index already exists with a different name and options. A collection can have only one text index; it has %s.", a.mongoIndexName(x))
				}
			}
			if name == "" {
				continue
			}
			if total >= mongoMaxIndexes {
				return nil, mql.Errorf(67, "CannotCreateIndex", "A database can have at most %d indexes.", mongoMaxIndexes)
			}
			in.Name = mc.db.name + "/collectionGroups/" + coll + "/indexes/CICAg" + randomID(6)
			a.indexes[in.Name] = in
			a.mongoIndexNames[in.Name] = name
			total++
			continue
		}
		in = &adminpb.Index{QueryScope: adminpb.Index_COLLECTION_GROUP, ApiScope: adminpb.Index_MONGODB_COMPATIBLE_API,
			State: adminpb.Index_READY, Density: adminpb.Index_DENSE, Unique: mql.Truthy(mql.Lookup(spec, "unique"))}
		if mql.Truthy(mql.Lookup(spec, "sparse")) {
			in.Density = adminpb.Index_SPARSE_ANY
		}
		for _, k := range keys {
			if strings.Contains(k.Key, "$**") {
				return nil, mql.Errorf(67, "CannotCreateIndex", "Wildcard indexes are not supported.")
			}
			n, isNum := mql.NumberOf(k.Value)
			if !isNum || (n != 1 && n != -1) {
				return nil, mql.Errorf(67, "CannotCreateIndex", "Index key %s: %v is not supported by CloudBurrow; use 1 or -1.", k.Key, k.Value)
			}
			order := adminpb.Index_IndexField_ASCENDING
			if n < 0 {
				order = adminpb.Index_IndexField_DESCENDING
			}
			in.Fields = append(in.Fields, &adminpb.Index_IndexField{FieldPath: k.Key, ValueMode: &adminpb.Index_IndexField_Order_{Order: order}})
		}
		name, _ := mql.Lookup(spec, "name").(string)
		if name == "" {
			name = defaultIndexName(keys)
		}
		exists := false
		for _, x := range a.mongoIndexes(mc.db.name, coll) {
			if a.mongoIndexName(x) == name {
				if !mql.ValuesEqual(indexKeys(x), keys) || x.GetUnique() != in.Unique {
					return nil, mql.Errorf(86, "IndexKeySpecsConflict", "An existing index has the same name as the requested index: %s", name)
				}
				exists = true
			}
		}
		if exists {
			continue
		}
		if total >= mongoMaxIndexes {
			return nil, mql.Errorf(67, "CannotCreateIndex", "A database can have at most %d indexes.", mongoMaxIndexes)
		}
		in.Name = mc.db.name + "/collectionGroups/" + coll + "/indexes/CICAg" + randomID(6)
		a.indexes[in.Name] = in
		a.mongoIndexNames[in.Name] = name
		total++
	}
	return bson.D{{Key: "numIndexesBefore", Value: int32(before)}, {Key: "numIndexesAfter", Value: int32(len(a.mongoIndexes(mc.db.name, coll)))},
		{Key: "createdCollectionAutomatically", Value: false}}, nil
}

func (s *MongoServer) listIndexes(mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	a := s.a
	a.mu.Lock()
	var out []bson.D
	for _, x := range a.mongoIndexes(mc.db.name, coll) {
		d := bson.D{{Key: "v", Value: int32(2)}, {Key: "key", Value: indexKeys(x)}, {Key: "name", Value: a.mongoIndexName(x)}}
		if isTextIndex(x) {
			ti := textOf(x)
			lang := ti.Language
			if lang == "" {
				lang = "english"
			}
			over := ti.LanguageOverride
			if over == "" || over == "\x00" {
				over = "language"
			}
			weights := bson.D{}
			for _, f := range ti.Fields {
				weights = append(weights, bson.E{Key: f, Value: int32(1)})
			}
			d = append(d, bson.E{Key: "weights", Value: weights}, bson.E{Key: "default_language", Value: lang},
				bson.E{Key: "language_override", Value: over}, bson.E{Key: "textIndexVersion", Value: int32(3)})
			if ti.Partition != "" {
				d = append(d, bson.E{Key: "firestoreOptions", Value: bson.D{{Key: "customPartitionFields", Value: bson.A{ti.Partition}}}})
			}
		}
		if x.GetUnique() {
			d = append(d, bson.E{Key: "unique", Value: true})
		}
		if x.GetDensity() == adminpb.Index_SPARSE_ANY {
			d = append(d, bson.E{Key: "sparse", Value: true})
		}
		out = append(out, d)
	}
	a.mu.Unlock()
	ns := mc.db.id + "." + coll
	first, rest := batch(out, 0)
	return cursorReply(s.newCursor(mc, ns, rest), ns, "firstBatch", first), nil
}

func (s *MongoServer) dropIndexes(mc *mongoConn, cmd bson.D) (bson.D, error) {
	coll, err := collName(cmd)
	if err != nil {
		return nil, err
	}
	spec := mql.Lookup(cmd, "index")
	var names []string
	all := false
	switch x := spec.(type) {
	case string:
		if x == "*" {
			all = true
		} else {
			names = []string{x}
		}
	case bson.A:
		for _, n := range x {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
	case bson.D:
		names = []string{defaultIndexName(x)}
	default:
		return nil, mql.BadValue("dropIndexes needs an index name, an array of names, a key pattern or \"*\"")
	}
	a := s.a
	a.mu.Lock()
	was := len(a.mongoIndexes(mc.db.name, coll))
	for _, n := range names {
		found := false
		for _, x := range a.mongoIndexes(mc.db.name, coll) {
			if a.mongoIndexName(x) == n {
				found = true
			}
		}
		if !found {
			a.mu.Unlock()
			return nil, mql.Errorf(27, "IndexNotFound", "index not found with name [%s]", n)
		}
	}
	a.mu.Unlock()
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	a.dropMongoIndexes(mc.db.name, coll, func(x *adminpb.Index) bool { return all || want[a.mongoIndexName(x)] })
	return bson.D{{Key: "nIndexesWas", Value: int32(was)}}, nil
}

// dropMongoIndexes removes a collection's MongoDB indexes that drop
// selects, and is how many the collection had.
func (a *Admin) dropMongoIndexes(db, coll string, drop func(*adminpb.Index) bool) int {
	defer a.persist()
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := a.mongoIndexes(db, coll)
	for _, x := range idx {
		if drop(x) {
			delete(a.indexes, x.GetName())
			delete(a.mongoIndexNames, x.GetName())
		}
	}
	return len(idx)
}
