package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func secretsFixture(t *testing.T) secretsProvider {
	t.Helper()
	return secretsProvider{svc: &secretsService{store: secrets.NewStore(store.NewMemory())}}
}

// TestSecretCreateDoesNotLeaveAnEmptySecretBehind.
//
// A secret with no versions holds nothing, so the first version is part of the
// create. When adding it fails, reporting the create as successful would leave
// a secret that cannot be read and a console that claimed otherwise.
func TestSecretCreateDoesNotLeaveAnEmptySecretBehind(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()

	// An empty payload is what the store refuses, which is the failure this
	// test needs: the secret is created and then the version is not.
	_, err := p.Create(ctx, "demo", map[string]string{
		"secretId": "api-key", "payload": "",
	})
	if err == nil {
		t.Fatal("an empty payload was accepted")
	}
	if _, err := p.svc.Store().GetSecret("demo", "api-key"); err == nil {
		t.Fatal("the half-made secret was left behind")
	}

	name, err := p.Create(ctx, "demo", map[string]string{
		"secretId": "api-key", "payload": "s3cret",
		"labels": `{"env":"dev"}`,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasSuffix(name, "/secrets/api-key") {
		t.Fatalf("name = %q", name)
	}
	sec, err := p.svc.Store().GetSecret("demo", "api-key")
	if err != nil {
		t.Fatal(err)
	}
	if sec.Labels["env"] != "dev" {
		t.Fatalf("labels = %v", sec.Labels)
	}
	versions, err := p.svc.Store().ListVersions("demo", "api-key")
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions = %v, %v", versions, err)
	}
}

// TestVersionActionsFollowTheVersionsState.
//
// Enabling an enabled version and destroying a destroyed one are buttons that
// exist only to fail, so neither is offered.
func TestVersionActionsFollowTheVersionsState(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	if _, err := p.Create(ctx, "demo", map[string]string{
		"secretId": "api-key", "payload": "s3cret",
	}); err != nil {
		t.Fatal(err)
	}

	ids := func(path ...string) []string {
		var out []string
		for _, a := range p.DetailActions(ctx, "demo", path) {
			out = append(out, a.ID)
		}
		return out
	}

	if got := strings.Join(ids("api-key", "1"), ","); got != "disable,destroy" {
		t.Fatalf("enabled version offers %q", got)
	}
	if err := p.ActAt(ctx, "demo", []string{"api-key", "1"}, "disable", nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids("api-key", "1"), ","); got != "enable,destroy" {
		t.Fatalf("disabled version offers %q", got)
	}
	if err := p.ActAt(ctx, "demo", []string{"api-key", "1"}, "destroy", nil); err != nil {
		t.Fatal(err)
	}
	if got := ids("api-key", "1"); len(got) != 0 {
		t.Fatalf("destroyed version offers %v, want nothing", got)
	}
	// The secret itself offers a new version, at every point.
	if got := strings.Join(ids("api-key"), ","); got != "addversion" {
		t.Fatalf("secret offers %q", got)
	}
}

// TestRevealRefusesWhatTheAPIWouldRefuse.
//
// A disabled version is unreadable through the API, and a console that read the
// stored bytes directly would be a way around a state the API enforces.
func TestRevealRefusesWhatTheAPIWouldRefuse(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	if _, err := p.Create(ctx, "demo", map[string]string{
		"secretId": "api-key", "payload": "s3cret",
	}); err != nil {
		t.Fatal(err)
	}

	label, value, err := p.Reveal(ctx, "demo", []string{"api-key", "1"})
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if value != "s3cret" || label != "api-key version 1" {
		t.Fatalf("reveal = %q, %q", label, value)
	}

	if err := p.ActAt(ctx, "demo", []string{"api-key", "1"}, "disable", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Reveal(ctx, "demo", []string{"api-key", "1"}); err == nil {
		t.Fatal("a disabled version was revealed")
	}

	// Only a version has a value. A secret is a container, and offering the
	// control on it would be a button that could not work.
	if p.CanReveal([]string{"api-key"}) {
		t.Fatal("a secret claims to have a value of its own")
	}
	if !p.CanReveal([]string{"api-key", "1"}) {
		t.Fatal("a version claims to have no value")
	}
}

// TestSecretListingsNeverCarryPayloads.
//
// Every version's bytes are in hand when the detail page is built. Putting them
// in the response would hand out every credential a project holds to anyone who
// opened the page.
func TestSecretListingsNeverCarryPayloads(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	if _, err := p.Create(ctx, "demo", map[string]string{
		"secretId": "api-key", "payload": "s3cret-payload-marker",
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range [][]string{{"api-key"}, {"api-key", "1"}} {
		d, err := p.Detail(ctx, "demo", path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detailText(d), "s3cret-payload-marker") {
			t.Fatalf("the payload appears in the detail for %v", path)
		}
	}
	// The listing does report that there is something there, which is the part
	// that can be said without handing the value over.
	d, err := p.Detail(ctx, "demo", []string{"api-key", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detailText(d), "21 bytes") {
		t.Fatalf("the version does not report its size:\n%s", detailText(d))
	}
}

// detailText renders everything a Detail would put on screen, so a test can
// assert that a value is absent from all of it rather than from the one field
// it thought to check.
func detailText(d console.Detail) string {
	var b strings.Builder
	for _, prop := range d.Summary {
		b.WriteString(prop.Label + "=" + prop.Value + "\n")
	}
	b.WriteString(d.Unavailable + "\n" + d.Prompt + "\n")
	for _, sec := range d.Sections {
		b.WriteString(sec.Label + "\n" + sec.Text + "\n" + sec.Note + "\n" + sec.Unavailable + "\n")
		for _, g := range sec.Groups {
			b.WriteString(g.Heading + "\n")
			for _, prop := range g.Properties {
				b.WriteString(prop.Label + "=" + prop.Value + "\n")
			}
		}
		b.WriteString(sec.Listing.Note + "\n")
		for _, item := range sec.Listing.Items {
			b.WriteString(item.Name + " " + item.Status + "\n")
			for k, v := range item.Fields {
				b.WriteString(k + "=" + v + "\n")
			}
		}
	}
	return b.String()
}

// TestTaskHeadersAreRedacted.
//
// A queued task's headers are stored, so a page that rendered them would be a
// place to read credentials out of. The key is kept, because the shape of the
// request is what the page is for.
func TestTaskHeadersAreRedacted(t *testing.T) {
	got := redactHeaders(map[string]string{
		"Content-Type":        "application/json",
		"Authorization":       "Bearer ya29.abcdef",
		"X-Api-Key":           "live_1234",
		"X-Session-Token":     "t-9",
		"Proxy-Authorization": "Basic abc",
		"X-Request-Id":        "r-1",
	})
	for _, key := range []string{"Authorization", "X-Api-Key", "X-Session-Token",
		"Proxy-Authorization"} {
		if got[key] != "[redacted]" {
			t.Errorf("%s = %q, want redacted", key, got[key])
		}
	}
	if got["Content-Type"] != "application/json" || got["X-Request-Id"] != "r-1" {
		t.Errorf("an ordinary header was redacted: %v", got)
	}
	if len(got) != 6 {
		t.Errorf("redaction dropped a header, so the request's shape is lost: %v", got)
	}
}
