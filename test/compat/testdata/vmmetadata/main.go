// Command vmmetadata runs inside a Compute Engine VM (#2177) and reads the
// VM's metadata server through Google's official client package,
// cloud.google.com/go/compute/metadata, as an application on Compute
// Engine does: OnGCE, the project, the instance's name, ID, zone and
// hostname, an attribute, the network interface's IP, the service
// account's email, a token, and a wait-for-change through Subscribe. It
// prints KEY=VALUE lines (never the token itself) and fails on any error.
//
// It talks only to the metadata server the VM names (GCE_METADATA_HOST,
// or the package's default, metadata.google.internal / 169.254.169.254,
// which the VM answers itself), never to a Google API.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := metadata.NewClient(nil)
	fail := func(what string, err error) {
		fmt.Printf("FAIL %s: %v\n", what, err)
		os.Exit(1)
	}
	if !metadata.OnGCEWithContext(ctx) {
		fail("OnGCE", fmt.Errorf("false"))
	}
	fmt.Println("ongce=true")
	for _, k := range []struct {
		name string
		get  func(context.Context) (string, error)
	}{
		{"project", c.ProjectIDWithContext},
		{"numeric", c.NumericProjectIDWithContext},
		{"name", c.InstanceNameWithContext},
		{"id", c.InstanceIDWithContext},
		{"zone", c.ZoneWithContext},
		{"hostname", c.HostnameWithContext},
		{"ip", c.InternalIPWithContext},
		{"email", func(ctx context.Context) (string, error) { return c.EmailWithContext(ctx, "default") }},
		{"role", func(ctx context.Context) (string, error) { return c.InstanceAttributeValueWithContext(ctx, "role") }},
		{"nic", func(ctx context.Context) (string, error) {
			return c.GetWithContext(ctx, "instance/network-interfaces/0/ip")
		}},
	} {
		v, err := k.get(ctx)
		if err != nil {
			fail(k.name, err)
		}
		fmt.Printf("%s=%s\n", k.name, strings.TrimSpace(v))
	}
	raw, err := c.GetWithContext(ctx, "instance/service-accounts/default/token")
	if err != nil {
		fail("token", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal([]byte(raw), &tok); err != nil || tok.AccessToken == "" {
		fail("token", fmt.Errorf("%v: %d bytes", err, len(raw)))
	}
	fmt.Printf("token=%s expires_in=%d\n", tok.TokenType, tok.ExpiresIn)
	scopes, err := c.ScopesWithContext(ctx, "default")
	if err != nil {
		fail("scopes", err)
	}
	fmt.Printf("scopes=%s\n", strings.Join(scopes, ","))
	// Subscribe is wait_for_change: it returns the current value first,
	// then each change, until the callback says stop.
	if len(os.Args) > 1 && os.Args[1] == "watch" {
		seen := 0
		err := c.SubscribeWithContext(ctx, "instance/attributes/role", func(_ context.Context, v string, ok bool) error {
			seen++
			fmt.Printf("watch%d=%s\n", seen, v)
			if seen >= 2 {
				return errStop
			}
			return nil
		})
		if err != nil && err != errStop {
			fail("subscribe", err)
		}
	}
	fmt.Println("OK")
}

var errStop = fmt.Errorf("stop")
