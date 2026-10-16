package main

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

// The Cloud KMS port serves KeyManagementService, EkmService, Autokey and
// AutokeyAdmin (#1452), HsmManagement (every method UNIMPLEMENTED), the
// IAMPolicy mixin (#428), the Locations mixin (#1291) and the Operations
// mixin CreateKeyHandle's operations are read through (#1452).
// Reflection is the transport's own, registered on every CloudBurrow gRPC
// server.
func TestTheKMSPortRegistersGooglesKMSServices(t *testing.T) {
	svc := startedKMS(t, kmsConfig(t, "kms"), nil)
	conn, err := grpc.NewClient(svc.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{}}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		got = append(got, s.GetName())
	}
	slices.Sort(got)
	want := []string{"google.cloud.kms.v1.Autokey", "google.cloud.kms.v1.AutokeyAdmin", "google.cloud.kms.v1.EkmService", "google.cloud.kms.v1.HsmManagement",
		"google.cloud.kms.v1.KeyManagementService", "google.cloud.location.Locations", "google.iam.v1.IAMPolicy", "google.longrunning.Operations",
		"grpc.reflection.v1.ServerReflection", "grpc.reflection.v1alpha.ServerReflection"}
	if !slices.Equal(got, want) {
		t.Errorf("services on the KMS port = %v, want %v", got, want)
	}
}
