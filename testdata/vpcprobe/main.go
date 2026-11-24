// Command vpcprobe is a Cloud Run container for the VPC egress tests
// (#2140): GET /?target=HOST:PORT fetches http://HOST:PORT/ and answers
// with its body, or 502 with the error, so a test sees whether the
// revision reaches an address on a VPC network.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	client := &http.Client{Timeout: 5 * time.Second}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target == "" {
			fmt.Fprintln(w, "VPC PROBE: give ?target=HOST:PORT")
			return
		}
		resp, err := client.Get("http://" + target + "/")
		if err != nil {
			http.Error(w, "VPC PROBE: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(w, "VPC PROBE: %d %s", resp.StatusCode, b)
	})
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	_ = http.ListenAndServe(":"+port, nil)
}
