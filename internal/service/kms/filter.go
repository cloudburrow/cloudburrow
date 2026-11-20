package kms

import (
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
)

// The list filter language of https://docs.cloud.google.com/kms/docs/sorting-and-filtering
// (#408, #1943): comparisons `field op value` with = != : < > <= >=;
// negation with NOT or -; AND (a space is equivalent) and OR, where "OR
// takes precedence over AND", and parentheses. That is AIP-160, so a filter
// is compiled by the shared internal/aipfilter against the listed
// resource's message and evaluated on each rendered resource: any field of
// it (name, labels.KEY, purpose, primary.state, version_template.algorithm,
// create_time, ...) can be filtered on. An unknown field or a syntax error
// is INVALID_ARGUMENT (UNVERIFIED wording).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/kms/docs/sorting-and-filtering

// parseFilter compiles filter for resources like sample. An empty filter is
// nil, which matches everything.
func parseFilter(filter string, sample proto.Message) (*aipfilter.Filter, error) {
	return aipfilter.Compile(filter, sample.ProtoReflect().Descriptor())
}
