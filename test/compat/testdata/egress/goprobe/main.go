// Command goprobe is the Go client of TestServedClientsSendNothingOffTheMachine
// (test/compat/egresscontainer_test.go): Cloud KMS over gRPC and REST,
// pointed at the instance, with the ADC fixture GOOGLE_APPLICATION_CREDENTIALS
// names as its only credential. It prints "OK go <transport>" or
// "FAIL go <transport>: <error>" for each, and runs only inside the
// no-egress container.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr, parent := os.Getenv("EGRESS_KMS"), os.Getenv("EGRESS_PARENT")
	// Never fall back to Google's endpoint.
	if addr == "" || parent == "" || os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
		fmt.Println("FAIL go: EGRESS_KMS, EGRESS_PARENT and GOOGLE_APPLICATION_CREDENTIALS must be set")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	g, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(addr),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	report("grpc", g, err, ctx, parent)
	r, err := kms.NewKeyManagementRESTClient(ctx, option.WithEndpoint("http://"+addr))
	report("rest", r, err, ctx, parent)
	// Room for anything a library starts in the background.
	time.Sleep(3 * time.Second)
}

func report(transport string, c *kms.KeyManagementClient, err error, ctx context.Context, parent string) {
	if err == nil {
		defer c.Close()
		it := c.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: parent})
		for {
			if _, err = it.Next(); err != nil {
				break
			}
		}
		if errors.Is(err, iterator.Done) {
			err = nil
		}
	}
	if err != nil {
		fmt.Printf("FAIL go %s: %v\n", transport, err)
		return
	}
	fmt.Printf("OK go %s\n", transport)
}
