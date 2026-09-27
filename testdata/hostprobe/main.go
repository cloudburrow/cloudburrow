// Command hostprobe exercises the services the CLI serves from inside a
// pod, through the official clients, at the cloudburrow-host addresses
// (#575): what a Cloud Run container does when it reads a secret and
// enqueues a task.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func opts(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func main() {
	if err := run(); err != nil {
		fmt.Printf("HOST PROBE: FAIL %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	project, secretsAddr, tasksAddr := os.Getenv("PROJECT"), os.Getenv("SECRETS_ADDR"), os.Getenv("TASKS_ADDR")
	id := os.Getenv("PROBE_ID")

	sm, err := secretmanager.NewClient(ctx, opts(secretsAddr)...)
	if err != nil {
		return err
	}
	defer sm.Close()
	name := "projects/" + project + "/secrets/" + id
	payload, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/latest"})
	if err != nil {
		return fmt.Errorf("read the secret the host wrote: %w", err)
	}

	tc, err := cloudtasks.NewClient(ctx, opts(tasksAddr)...)
	if err != nil {
		return err
	}
	defer tc.Close()
	queue := "projects/" + project + "/locations/us-central1/queues/" + id
	task, err := tc.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: queue, Task: &cloudtaskspb.Task{
		ScheduleTime: nil,
		MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
			Url: "http://127.0.0.1:1/never", HttpMethod: cloudtaskspb.HttpMethod_POST, Body: payload.GetPayload().GetData()}},
	}})
	if err != nil {
		return fmt.Errorf("create a task: %w", err)
	}
	fmt.Printf("HOST PROBE: OK secret=%s via %s task=%s via %s\n", payload.GetName(), secretsAddr, task.GetName(), tasksAddr)
	return nil
}
