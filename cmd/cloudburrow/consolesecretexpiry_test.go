package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestConsoleSecretExpiry (#1225): Create secret takes an expire time or a
// TTL, the page shows the expiry, and Edit secret moves and clears it.
func TestConsoleSecretExpiry(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	at := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "a", "payload": "x",
		"expireTime": at.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "demo", []string{"projects/demo/secrets/a"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detailText(d), at.Format(time.RFC3339)) {
		t.Errorf("the page does not show the expire time %s", at.Format(time.RFC3339))
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "b", "payload": "x", "ttl": "24h"}); err != nil {
		t.Fatal(err)
	}
	b, _ := p.svc.Store().GetSecret("demo", "b")
	if until := time.Until(b.Expire); until < 23*time.Hour || until > 25*time.Hour {
		t.Errorf("a 24h TTL expires in %s", until)
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "c", "payload": "x",
		"ttl": "1h", "expireTime": at.Format(time.RFC3339)}); err == nil {
		t.Error("both an expire time and a TTL were accepted")
	}
	if _, err := p.Create(ctx, "demo", map[string]string{"secretId": "c", "payload": "x", "ttl": "soon"}); err == nil {
		t.Error("a malformed TTL was accepted")
	}

	if err := p.Edit(ctx, "demo", []string{"projects/demo/secrets/a"}, map[string]string{"ttl": "2h"}); err != nil {
		t.Fatal(err)
	}
	a, _ := p.svc.Store().GetSecret("demo", "a")
	if until := time.Until(a.Expire); until < time.Hour || until > 3*time.Hour {
		t.Errorf("Edit with a 2h TTL: expires in %s", until)
	}
	if err := p.Edit(ctx, "demo", []string{"projects/demo/secrets/a"}, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if a, _ = p.svc.Store().GetSecret("demo", "a"); !a.Expire.IsZero() {
		t.Errorf("Edit with neither field left expire %s", a.Expire)
	}
	d, _ = p.Detail(ctx, "demo", []string{"projects/demo/secrets/a"})
	if !strings.Contains(detailText(d), "Never") {
		t.Error("a secret without an expiry does not say Never")
	}
}

// TestConsoleSecretListFilter (#1228): the list screen's filter is the
// API's, evaluated by the same code; a refused filter is reported on the
// box with no rows.
func TestConsoleSecretListFilter(t *testing.T) {
	p := secretsFixture(t)
	ctx := context.Background()
	for id, env := range map[string]string{"app-db": "prod", "app-cache": "dev", "other": ""} {
		v := map[string]string{"secretId": id, "payload": "x"}
		if env != "" {
			v["labels"] = `{"env":"` + env + `"}`
		}
		if _, err := p.Create(ctx, "demo", v); err != nil {
			t.Fatal(err)
		}
	}
	l, err := p.ListFiltered(ctx, "demo", "labels.env=prod OR name:other")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range l.Items {
		got = append(got, lastSegment(r.Name))
	}
	if strings.Join(got, " ") != "app-db other" {
		t.Errorf("filtered rows = %v", got)
	}
	if l.ServerFilter == nil || l.ServerFilter.Value != "labels.env=prod OR name:other" || l.ServerFilter.Help == "" {
		t.Errorf("serverFilter = %+v", l.ServerFilter)
	}
	l, err = p.ListFiltered(ctx, "demo", `name ~ "app.*"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 0 || l.ServerFilter == nil || !strings.Contains(l.ServerFilter.Error, "~") {
		t.Errorf("a refused filter = %d rows, %+v", len(l.Items), l.ServerFilter)
	}
	l, _ = p.List(ctx, "demo")
	if len(l.Items) != 3 || l.ServerFilter == nil {
		t.Errorf("unfiltered list = %d rows, filter box %v", len(l.Items), l.ServerFilter)
	}
}
