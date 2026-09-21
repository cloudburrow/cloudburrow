// Command spannerprobe exercises the official Cloud Spanner client from inside
// the cluster.
//
// #37 asks for verification "from host and pod", and those are different
// claims: the host reaches the emulator through a port-forward, while a pod
// reaches it through cluster DNS and the service network. A host-only test
// leaves the path an actual application uses unverified.
//
// It creates its own instance, database and schema so that what it proves is
// the admin and data APIs working from here, not that something else set them
// up earlier.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"cloud.google.com/go/spanner"
)

func main() {
	if err := run(); err != nil {
		fmt.Printf("SPANNER POD PROBE: FAIL %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := os.Getenv("SPANNER_EMULATOR_HOST")
	project := os.Getenv("SPANNER_PROJECT")
	instanceID := os.Getenv("SPANNER_INSTANCE")
	if addr == "" || project == "" || instanceID == "" {
		return fmt.Errorf("SPANNER_EMULATOR_HOST, SPANNER_PROJECT and SPANNER_INSTANCE are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// No credentials anywhere: the emulator takes none, and a pod that needed
	// them would not be running offline.
	opts := []option.ClientOption{
		option.WithEndpoint(addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}

	instAdmin, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("instance admin client: %w", err)
	}
	defer instAdmin.Close()

	op, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent:     "projects/" + project,
		InstanceId: instanceID,
		Instance: &instancepb.Instance{
			Config:      fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", project),
			DisplayName: "pod probe",
			NodeCount:   1,
		},
	})
	if err != nil {
		return fmt.Errorf("create instance: %w", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		return fmt.Errorf("await instance: %w", err)
	}

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("database admin client: %w", err)
	}
	defer dbAdmin.Close()

	dbOp, err := dbAdmin.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent:          fmt.Sprintf("projects/%s/instances/%s", project, instanceID),
		CreateStatement: "CREATE DATABASE poddb",
		ExtraStatements: []string{
			`CREATE TABLE Probe (Id INT64 NOT NULL, Note STRING(MAX)) PRIMARY KEY (Id)`,
		},
	})
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	if _, err := dbOp.Wait(ctx); err != nil {
		return fmt.Errorf("await database: %w", err)
	}

	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/poddb", project, instanceID)
	c, err := spanner.NewClient(ctx, dbName, opts...)
	if err != nil {
		return fmt.Errorf("spanner client: %w", err)
	}
	defer c.Close()

	// A read-write transaction rather than a bare mutation: a transaction is
	// the thing a Spanner application is written around, and the criterion
	// names it separately from queries for that reason.
	if _, err := c.ReadWriteTransaction(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		return txn.BufferWrite([]*spanner.Mutation{
			spanner.Insert("Probe", []string{"Id", "Note"}, []any{int64(1), "written from a pod"}),
		})
	}); err != nil {
		return fmt.Errorf("read-write transaction: %w", err)
	}

	row, err := c.Single().ReadRow(ctx, "Probe", spanner.Key{int64(1)}, []string{"Note"})
	if err != nil {
		return fmt.Errorf("read row: %w", err)
	}
	var note string
	if err := row.Column(0, &note); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if note != "written from a pod" {
		return fmt.Errorf("Note = %q, want %q", note, "written from a pod")
	}

	fmt.Printf("SPANNER POD PROBE: OK endpoint=%s database=%s note=%q\n", addr, dbName, note)
	return nil
}
