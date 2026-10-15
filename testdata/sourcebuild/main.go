// Command sourcebuild is the source TestRunSubmitBuildDockerDeploys (#1268)
// uploads to Cloud Storage and builds through Cloud Run's SubmitBuild: a
// static HTTP server, copied into a FROM scratch image, so the build pulls
// nothing.
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "built from source by SubmitBuild")
	})
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
