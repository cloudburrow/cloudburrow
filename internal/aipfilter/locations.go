package aipfilter

import (
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
)

// LocationIDs keeps the location IDs whose Location, as mk builds it,
// passes filter: the google.cloud.location.Locations.ListLocations filter,
// which the mixin documents as AIP-160 (for example displayName=tokyo).
// The IDs keep their order, so a caller pages the result as before.
func LocationIDs(filter string, ids []string, mk func(id string) (*locationpb.Location, error)) ([]string, error) {
	var zero *locationpb.Location
	f, err := Compile(filter, zero.ProtoReflect().Descriptor())
	if err != nil || f == nil {
		return ids, err
	}
	var out []string
	for _, id := range ids {
		loc, err := mk(id)
		if err != nil {
			return nil, err
		}
		if f.Match(loc) {
			out = append(out, id)
		}
	}
	return out, nil
}
