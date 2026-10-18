package kms

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Key deletion and retired resources (#1212), as service.proto documents
// them:
//   - DeleteCryptoKeyVersion permanently deletes a version that is DESTROYED
//     (or IMPORT_FAILED or GENERATION_FAILED, which no version here reaches);
//   - DeleteCryptoKey permanently deletes a key once every one of its versions
//     has been deleted, and leaves a RetiredResource that keeps its name from
//     being reused.
//
// Both are long-running on Google; the deletion is immediate here, so each
// returns its Operation already done, which the client's Wait returns without
// polling. The codes for a refused deletion are UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto

const retiredPrefix = "kms/retired/"

// retiredResourceType is the resource type of a retired CryptoKey
// (resources.proto RetiredResource.resource_type).
const retiredResourceType = "cloudkms.googleapis.com/CryptoKey"

type retiredResource struct {
	Name     string    `json:"name"`
	Original string    `json:"originalResource"`
	Type     string    `json:"resourceType"`
	Deleted  time.Time `json:"deleteTime"`
}

func toRetired(r retiredResource) *kmspb.RetiredResource {
	return &kmspb.RetiredResource{Name: r.Name, OriginalResource: r.Original, ResourceType: r.Type, DeleteTime: timestamppb.New(r.Deleted)}
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", apierror.Internal(err, "generate an ID")
	}
	return hex.EncodeToString(b), nil
}

// locationOf is the projects/{p}/locations/{l} prefix of a parsed name.
func locationOf(name string) string { return strings.Join(strings.Split(name, "/")[:4], "/") }

// doneOperation is a finished long-running operation.
func doneOperation(location string, metadata proto.Message) (*longrunningpb.Operation, error) {
	id, err := randomID()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	var md *anypb.Any
	if metadata != nil {
		var err error
		if md, err = anypb.New(metadata); err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "encode metadata"))
		}
	}
	resp, err := anypb.New(&emptypb.Empty{})
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode response"))
	}
	return &longrunningpb.Operation{Name: location + "/operations/" + id, Metadata: md, Done: true,
		Result: &longrunningpb.Operation_Response{Response: resp}}, nil
}

// isRetired reports whether a deleted CryptoKey had name.
func (s *Server) isRetired(name string) (bool, error) {
	keys, err := s.db.List(retiredPrefix + strings.ReplaceAll(locationOf(name), "/", "~") + "~")
	if err != nil {
		return false, apierror.Wrap(apierror.Internal(err, "list"))
	}
	for _, key := range keys {
		var r retiredResource
		found, err := s.get(key, &r)
		if err != nil {
			return false, apierror.Wrap(err)
		}
		if found && r.Original == name {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) DeleteCryptoKeyVersion(_ context.Context, req *kmspb.DeleteCryptoKeyVersionRequest) (*longrunningpb.Operation, error) {
	keyName, number, err := parseCryptoKeyVersion("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, req.GetName()), &v, "CryptoKeyVersion", req.GetName()); err != nil {
		return nil, err
	}
	if st := v.state(); st != kmspb.CryptoKeyVersion_DESTROYED {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"CryptoKeyVersion %s is %s: only a DESTROYED version can be deleted", v.Name, st))
	}
	// "Successfully imported CryptoKeyVersions cannot be deleted at this
	// time" (service.proto).
	if v.ImportJob != "" {
		return nil, apierror.Wrap(apierror.FailedPrecondition("CryptoKeyVersion %s was imported: an imported version cannot be deleted", v.Name))
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, keyName), &k, "CryptoKey", keyName); err != nil {
		return nil, err
	}
	// A destroyed primary stays the primary until it is deleted; then the key
	// has none, and its number is never reused (k.Next is kept).
	if k.Primary == number {
		k.Primary = 0
		if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	if err := s.db.Delete(dbKey(versionPrefix, v.Name)); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "delete %s", v.Name))
	}
	return doneOperation(locationOf(v.Name), &kmspb.DeleteCryptoKeyVersionMetadata{})
}

func (s *Server) DeleteCryptoKey(_ context.Context, req *kmspb.DeleteCryptoKeyRequest) (*longrunningpb.Operation, error) {
	if err := parseCryptoKey("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetName()), &k, "CryptoKey", req.GetName()); err != nil {
		return nil, err
	}
	versions, err := s.list(versionPrefix, k.Name)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if len(versions) > 0 {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"CryptoKey %s still has %d versions: every version must be deleted with DeleteCryptoKeyVersion first", k.Name, len(versions)))
	}
	// delete-kms-resources: "The key doesn't have automatic key rotation
	// scheduled." and "The key wasn't created by Cloud KMS Autokey."
	// (https://cloud.google.com/kms/docs/delete-kms-resources, read
	// 2026-10-17); the code is UNVERIFIED, as for the versions above.
	// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/kms/docs/delete-kms-resources
	if k.RotationPeriod > 0 {
		return nil, apierror.Wrap(apierror.FailedPrecondition(
			"CryptoKey %s has automatic rotation scheduled: clear its rotation schedule before deleting it", k.Name))
	}
	if autokey, err := s.autokeyKey(k.Name); err != nil {
		return nil, apierror.Wrap(err)
	} else if autokey {
		return nil, apierror.Wrap(apierror.FailedPrecondition("CryptoKey %s was created by Cloud KMS Autokey and cannot be deleted", k.Name))
	}
	id, err := randomID()
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	r := retiredResource{Name: locationOf(k.Name) + "/retiredResources/" + id, Original: k.Name, Type: retiredResourceType, Deleted: s.now().UTC()}
	// The retired record first, so a failure between never frees the name.
	if err := s.put(dbKey(retiredPrefix, r.Name), r); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.db.Delete(dbKey(keyPrefix, k.Name)); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "delete %s", k.Name))
	}
	return doneOperation(locationOf(k.Name), &kmspb.DeleteCryptoKeyMetadata{RetiredResource: r.Name})
}

func parseRetired(field, name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[4] != "retiredResources" || parts[5] == "" {
		return badName(field, name, "RetiredResource", "want projects/{project}/locations/{location}/retiredResources/{retired_resource}")
	}
	return parseLocation(field, strings.Join(parts[:4], "/"))
}

func (s *Server) GetRetiredResource(_ context.Context, req *kmspb.GetRetiredResourceRequest) (*kmspb.RetiredResource, error) {
	if err := parseRetired("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var r retiredResource
	if err := s.load(dbKey(retiredPrefix, req.GetName()), &r, "RetiredResource", req.GetName()); err != nil {
		return nil, err
	}
	return toRetired(r), nil
}

func (s *Server) ListRetiredResources(_ context.Context, req *kmspb.ListRetiredResourcesRequest) (*kmspb.ListRetiredResourcesResponse, error) {
	if err := parseLocation("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	names, err := s.list(retiredPrefix, req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := orderedPage("kms-retired:"+req.GetParent(), names, byName, false, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("%v", err))
	}
	resp := &kmspb.ListRetiredResourcesResponse{NextPageToken: next, TotalSize: int64(len(names))}
	for _, n := range page {
		var r retiredResource
		found, err := s.get(dbKey(retiredPrefix, n), &r)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if found {
			resp.RetiredResources = append(resp.RetiredResources, toRetired(r))
		}
	}
	return resp, nil
}

// autokeyKey reports whether a KeyHandle names key: Autokey created it.
func (s *Server) autokeyKey(key string) (bool, error) {
	handles, err := s.db.List(keyHandlePrefix)
	if err != nil {
		return false, apierror.Internal(err, "list key handles")
	}
	for _, dbk := range handles {
		var h keyHandle
		found, err := s.get(dbk, &h)
		if err != nil {
			return false, err
		}
		if found && h.KmsKey == key {
			return true, nil
		}
	}
	return false, nil
}
