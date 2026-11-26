package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/awsprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/azureprov"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
)

// byocFakesEnv names a JSON file (test/byocfakes writes it) that points
// bring your own cloud at local fakes of AWS and Azure instead of the
// clouds, for the tests and the first-run walk-through (#2209): every
// endpoint must be on this machine's loopback, so nothing a fake is given
// can leave it. The fakes' own test credentials then live in a directory
// store, never the OS keychain.
const byocFakesEnv = "CLOUDBURROW_BYOC_FAKES"

// byocFakes is that file.
type byocFakes struct {
	// AWS is awsfake's base URL; Azure azurefake's (TLS, its certificate
	// in AzureCA, a PEM file).
	AWS     string `json:"aws"`
	Azure   string `json:"azure"`
	AzureCA string `json:"azure_ca"`
	// OpenURL completes a Microsoft sign-in as the user's browser would:
	// the fakes' process POSTs the authorize URL to azurefake.
	OpenURL string `json:"open_url"`
	// SSHConfig maps the fakes' VM addresses to the stand-in hosts.
	SSHConfig string `json:"ssh_config"`
	// Keychain is the directory store's path.
	Keychain string `json:"keychain"`
	// PublicIP is this machine's public address as the fakes see it.
	PublicIP string `json:"public_ip"`
}

// loadByocFakes reads byocFakesEnv's file; ok is false when it is unset.
func loadByocFakes() (byocFakes, bool, error) {
	path := os.Getenv(byocFakesEnv)
	if path == "" {
		return byocFakes{}, false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return byocFakes{}, false, fmt.Errorf("%s: %w", byocFakesEnv, err)
	}
	var f byocFakes
	if err := json.Unmarshal(b, &f); err != nil {
		return byocFakes{}, false, fmt.Errorf("%s: %w", byocFakesEnv, err)
	}
	for _, u := range []string{f.AWS, f.Azure, f.OpenURL} {
		if err := loopbackURL(u); err != nil {
			return byocFakes{}, false, fmt.Errorf("%s: %w: the fakes must run on this machine", byocFakesEnv, err)
		}
	}
	if f.Keychain == "" {
		return byocFakes{}, false, fmt.Errorf("%s names no keychain directory for the fakes' credentials", byocFakesEnv)
	}
	return f, true, nil
}

// loopbackURL refuses a URL whose host is not a loopback address.
func loopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a URL", raw)
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s is not a loopback address", u.Host)
	}
	return nil
}

// byocSSHConfig is the ssh config every connection to a VM uses: the
// fakes' mapping of their addresses to the stand-ins, or "" (the user's
// own) otherwise.
func byocSSHConfig() string {
	f, ok, err := loadByocFakes()
	if err != nil || !ok {
		return ""
	}
	return f.SSHConfig
}

// fakeCloudOptions is the manager's options against the fakes.
func fakeCloudOptions(f byocFakes, o byoc.Options) (byoc.Options, error) {
	pem, err := os.ReadFile(f.AzureCA)
	if err != nil {
		return o, fmt.Errorf("%s: azure_ca: %w", byocFakesEnv, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return o, fmt.Errorf("%s: azure_ca holds no certificate", byocFakesEnv)
	}
	client := &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	open := func(authURL string) error {
		body, _ := json.Marshal(map[string]string{"url": authURL})
		resp, err := http.Post(f.OpenURL, "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the fakes did not open the sign-in page (%s)", resp.Status)
		}
		return nil
	}
	o.Keychain = keychain.Dir{Path: f.Keychain}
	o.Drivers = []byoc.Driver{
		awsprov.NewDriver(awsprov.Options{Endpoint: f.AWS, PollInterval: 200 * time.Millisecond, ClientName: "CloudBurrow"}),
		azureprov.New(azureprov.Options{AuthorityHost: f.Azure + "/", ResourceManager: f.Azure + "/", Audience: "https://management.fake.example/",
			PricesURL: f.Azure + "/api/retail/prices", HTTPClient: client, DisableInstanceDiscovery: true, PollInterval: 200 * time.Millisecond,
			OpenBrowser: open}),
	}
	ip := f.PublicIP
	if ip == "" {
		ip = "203.0.113.77"
	}
	o.IPLookup = func(context.Context) (string, error) { return ip, nil }
	o.OpenBrowser = open
	if o.Dir == "" {
		return o, errors.New("no state directory")
	}
	return o, nil
}
