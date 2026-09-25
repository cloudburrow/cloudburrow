// Command pubsubfake serves Google's in-memory Pub/Sub fake (pstest) on a
// fixed address, so CI can run the builtin Cloud Storage server's
// notification tests (#506) without the Pub/Sub emulator image. It is test
// infrastructure: never built into cloudburrow, never shipped.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"cloud.google.com/go/pubsub/v2/pstest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8095", "address to serve on")
	flag.Parse()
	srv := pstest.NewServerWithAddress(*listen)
	fmt.Println("pubsubfake listening on", srv.Addr)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	_ = srv.Close()
}
