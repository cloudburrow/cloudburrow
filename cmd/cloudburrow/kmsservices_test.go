package main

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
)

// The Cloud KMS port serves KeyManagementService and nothing else of
// Google's: IAM, Locations, EKM, Autokey and HSM management stay
// unregistered, so gRPC answers UNIMPLEMENTED for them (#397). Reflection is
// the transport's own, registered on every CloudBurrow gRPC server.
func TestTheKMSPortRegistersOnlyKeyManagementService(t *testing.T) {
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
	want := []string{"google.cloud.kms.v1.KeyManagementService", "grpc.reflection.v1.ServerReflection", "grpc.reflection.v1alpha.ServerReflection"}
	if !slices.Equal(got, want) {
		t.Errorf("services on the KMS port = %v, want %v", got, want)
	}
}
