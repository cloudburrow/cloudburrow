// Command goprobe is the Go client of TestEnvResolvesNoGoogleName
// (test/compat/envdns_test.go): Google's Go auth libraries under
// `cloudburrow env`. Application Default Credentials through
// golang.org/x/oauth2/google and cloud.google.com/go/auth (the fixture
// GOOGLE_APPLICATION_CREDENTIALS names, refreshed at its token_uri), and the
// metadata server through cloud.google.com/go/compute/metadata. It prints
// "OK go <step>" or "FAIL go <step>: <error>", and runs only inside the
// no-egress container.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2/google"
)

const scope = "https://www.googleapis.com/auth/cloud-platform"

func main() {
	// Never fall back to Google's defaults.
	if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" || os.Getenv("GCE_METADATA_HOST") == "" {
		fmt.Println("FAIL go: GOOGLE_APPLICATION_CREDENTIALS and GCE_METADATA_HOST must be set")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	step("oauth2-adc", func() error {
		c, err := google.FindDefaultCredentials(ctx, scope)
		if err != nil {
			return err
		}
		_, err = c.TokenSource.Token()
		return err
	})
	step("auth-adc", func() error {
		c, err := credentials.DetectDefault(&credentials.DetectOptions{Scopes: []string{scope}})
		if err != nil {
			return err
		}
		if _, err := c.Token(ctx); err != nil {
			return err
		}
		_, err = c.ProjectID(ctx)
		return err
	})
	step("metadata", func() error {
		if !metadata.OnGCEWithContext(ctx) {
			return errors.New("OnGCE is false")
		}
		if _, err := metadata.ProjectIDWithContext(ctx); err != nil {
			return err
		}
		_, err := google.ComputeTokenSource("").Token()
		return err
	})
}

func step(name string, f func() error) {
	if err := f(); err != nil {
		fmt.Printf("FAIL go %s: %v\n", name, err)
		return
	}
	fmt.Printf("OK go %s\n", name)
}
