package secrets

import (
	"context"
	"fmt"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestVersionAliasesAreSetReadAndResolved covers version_aliases (#1850):
// UpdateSecret sets them, GetSecret returns them, GetSecretVersion and
// AccessSecretVersion resolve them, and the version calls that change a
// version refuse an alias.
func TestVersionAliasesAreSetReadAndResolved(t *testing.T) {
	g, st := newGRPC(t)
	ctx := context.Background()
	if _, err := st.CreateSecret("demo", "k", nil, nil, "automatic"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two"} {
		if _, err := st.AddVersion("demo", "k", []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	update := func(aliases map[string]int64) error {
		_, err := g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
			Secret:     &secretmanagerpb.Secret{Name: "projects/demo/secrets/k", VersionAliases: aliases},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version_aliases"}},
		})
		return err
	}
	if err := update(map[string]int64{"current": 2, "previous": 1}); err != nil {
		t.Fatal(err)
	}
	sec, err := g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/demo/secrets/k"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sec.GetVersionAliases(); got["current"] != 2 || got["previous"] != 1 || len(got) != 2 {
		t.Fatalf("aliases = %v", got)
	}
	acc, err := g.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: "projects/demo/secrets/k/versions/previous"})
	if err != nil {
		t.Fatal(err)
	}
	if string(acc.GetPayload().GetData()) != "one" || acc.GetName() != "projects/demo/secrets/k/versions/1" {
		t.Fatalf("access by alias = %q %q", acc.GetName(), acc.GetPayload().GetData())
	}
	v, err := g.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: "projects/demo/secrets/k/versions/current"})
	if err != nil || v.GetName() != "projects/demo/secrets/k/versions/2" {
		t.Fatalf("get by alias = %v, %v", v.GetName(), err)
	}
	_, err = g.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: "projects/demo/secrets/k/versions/missing"})
	wantGRPCCode(t, err, codes.NotFound)
	_, err = g.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: "projects/demo/secrets/k/versions/current"})
	wantGRPCCode(t, err, codes.InvalidArgument)
	_, err = g.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: "projects/demo/secrets/k/versions/current"})
	wantGRPCCode(t, err, codes.InvalidArgument)

	// The documented rules: a letter first, at most 63 characters, never
	// latest or NEW in any case, at most 50, and a version that exists.
	tooMany := map[string]int64{}
	for i := 0; i <= MaxVersionAliases; i++ {
		tooMany[fmt.Sprintf("a%d", i)] = 1
	}
	for name, aliases := range map[string]map[string]int64{
		"digit first": {"1st": 1},
		"too long":    {"a" + fmt.Sprintf("%063d", 0): 1},
		"latest":      {"Latest": 1},
		"new":         {"new": 1},
		"dot":         {"a.b": 1},
		"no version":  {"later": 9},
		"too many":    tooMany,
	} {
		if err := update(aliases); err == nil {
			t.Errorf("%s: %v was accepted", name, aliases)
		} else {
			wantGRPCCode(t, err, codes.InvalidArgument)
		}
	}
	// A mask naming them with none clears them.
	if err := update(nil); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetSecret("demo", "k")
	if len(got.VersionAliases) != 0 {
		t.Fatalf("aliases after clearing = %v", got.VersionAliases)
	}
}

// TestVersionAliasFilter matches version_aliases by name in a list filter.
func TestVersionAliasFilter(t *testing.T) {
	_, st := newGRPC(t)
	for _, id := range []string{"a", "b"} {
		if _, err := st.CreateSecret("demo", id, nil, nil, "automatic"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddVersion("demo", id, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpdateSecretFields("demo", "a", SecretUpdate{VersionAliases: map[string]int{"prod": 1}, UpdateVersionAliases: true}); err != nil {
		t.Fatal(err)
	}
	for filter, want := range map[string]int{"version_aliases:*": 1, "version_aliases:prod": 1, "NOT version_aliases:*": 1} {
		got, err := st.ListSecretsFiltered("demo", filter)
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		if len(got) != want {
			t.Errorf("%s: %d secrets, want %d", filter, len(got), want)
		}
	}
}
