//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"hash/crc32"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	rpccode "google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// kmsClients returns the official KMS client over gRPC and over REST, both
// against CLOUDBURROW_TEST_KMS, which serves both on one port (#414, #415).
func kmsClients(t *testing.T, h *Harness) map[string]*kms.KeyManagementClient {
	t.Helper()
	ctx := h.Context()
	addr := h.Endpoint(EnvKMS)
	g, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err := kms.NewKeyManagementRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*kms.KeyManagementClient{"grpc": g, "rest": r}
}

// crc32c is the Castagnoli CRC Cloud KMS uses for integrity checks.
func crc32c(b []byte) int64 { return int64(crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))) }

// kmsCode is the canonical code of a KMS error as the server sent it. Over
// gRPC that is the status. Over REST, gax maps the HTTP status alone, so
// FAILED_PRECONDITION, which is HTTP 400 as on Google, reaches a Go REST
// caller as INVALID_ARGUMENT; the envelope's "status" still names the real
// code, so it is read from there.
func kmsCode(variant string, err error) codes.Code {
	if variant != "rest" || err == nil {
		return status.Code(err)
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		var env struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(ge.Body), &env) == nil && env.Error.Status != "" {
			if c, ok := rpccode.Code_value[env.Error.Status]; ok {
				return codes.Code(c)
			}
		}
	}
	return status.Code(err)
}
