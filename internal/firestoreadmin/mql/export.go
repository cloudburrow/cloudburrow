package mql

import (
	"cloud.google.com/go/firestore/apiv1/firestorepb"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Error is a MongoDB command error: its code, code name and message.
type Error = mongoError

// Errorf is a MongoDB error of code and code name.
func Errorf(code int32, name, format string, args ...any) *Error {
	return merr(code, name, format, args...)
}

// BadValue is a BadValue (2) error.
func BadValue(format string, args ...any) *Error { return badValue(format, args...) }

// Env is what a pipeline reads beside its input.
type Env = aggEnv

// NewEnv is an Env that loads other collections with load.
func NewEnv(load func(coll string) ([]bson.D, error)) *Env { return &aggEnv{load: load} }

// Sort is a parsed sort document.
type Sort = sortSpec

// Less reports whether a sorts before b.
func (s sortSpec) Less(a, b bson.D) bool { return s.less(a, b) }

var (
	ToFirestoreDoc      = toFirestoreDoc
	FromFirestoreDoc    = fromFirestoreDoc
	DocID               = docID
	NumericAliases      = numericAliases
	CheckCollectionName = checkCollectionName
	Matches             = matches
	ParseSort           = parseSort
	Project             = project
	ApplyUpdate         = applyUpdate
	IsOperatorUpdate    = isOperatorUpdate
	UpsertBase          = upsertBase
	RunPipeline         = runPipeline
	Lookup              = lookup
	LookupE             = lookupE
	ValuesEqual         = valuesEqual
	TypeName            = bsonTypeName
	CloneDoc            = cloneDoc
	NumberOf            = numberOf
	Truthy              = truthy
	PathValues          = pathValues
)

// MaxDocSize is Google's maximum document size.
const MaxDocSize = mongoMaxDocSize

var _ = firestorepb.Value{}
