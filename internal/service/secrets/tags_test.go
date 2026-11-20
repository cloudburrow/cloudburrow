package secrets

import (
	"context"
	"strings"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// fakeBinder keeps bindings by resource, refusing an unknown key.
type fakeBinder struct{ bound map[string]map[string]string }

func (f *fakeBinder) BindTags(resource string, tags map[string]string, validateOnly bool) error {
	for k := range tags {
		if k != "tagKeys/1" {
			return apierror.InvalidArgument("tags: tag key %q does not exist", k)
		}
	}
	if !validateOnly {
		f.bound[resource] = tags
	}
	return nil
}

func (f *fakeBinder) UnbindTags(resource string) error {
	delete(f.bound, resource)
	return nil
}

// Secret.tags are bound through Resource Manager at creation, on the
// secret's full resource name, refused with nothing stored when a key does
// not exist, and unbound when the secret is deleted (#1961).
func TestCreateSecretBindsTags(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	b := &fakeBinder{bound: map[string]map[string]string{}}
	s.SetTagBinder(func() TagBinder { return b })
	ctx := context.Background()
	_, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "bad",
		Secret: &secretmanagerpb.Secret{Replication: automatic(), Tags: map[string]string{"tagKeys/9": "tagValues/9"}}})
	wantGRPCCode(t, err, codes.InvalidArgument)
	if _, err := s.GetSecret("demo", "bad"); err == nil {
		t.Error("a secret whose tags were refused was stored")
	}
	got, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "ok",
		Secret: &secretmanagerpb.Secret{Replication: automatic(), Tags: map[string]string{"tagKeys/1": "tagValues/1"}}})
	if err != nil {
		t.Fatalf("CreateSecret with tags: %v", err)
	}
	const full = "//secretmanager.googleapis.com/projects/demo/secrets/ok"
	if b.bound[full]["tagKeys/1"] != "tagValues/1" {
		t.Errorf("the bindings are %v; want tagKeys/1=tagValues/1 on %s", b.bound, full)
	}
	if len(got.GetTags()) != 0 {
		t.Errorf("CreateSecret returned tags %v; the field is input only", got.GetTags())
	}
	if _, err := g.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: "projects/demo/secrets/ok"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.bound[full]; ok {
		t.Error("the secret's bindings outlived it")
	}
}

// version_aliases at creation are checked as UpdateSecret checks them; a
// new secret has no versions, so even a well-formed alias is refused
// INVALID_ARGUMENT, and nothing is stored (#1961).
func TestCreateSecretChecksVersionAliases(t *testing.T) {
	t.Parallel()
	g, s := newGRPC(t)
	many := map[string]int64{}
	for i := 0; i < MaxVersionAliases+1; i++ {
		many["a"+strings.Repeat("x", i)] = 1
	}
	for want, aliases := range map[string]map[string]int64{
		"must start with a letter":   {"1st": 1},
		"reserved":                   {"latest": 1},
		"no more than 50":            many,
		"must name a positive":       {"current": 0},
		"version 1, which does not ": {"current": 1},
	} {
		_, err := g.CreateSecret(context.Background(), &secretmanagerpb.CreateSecretRequest{Parent: "projects/demo", SecretId: "s",
			Secret: &secretmanagerpb.Secret{Replication: automatic(), VersionAliases: aliases}})
		wantGRPCCode(t, err, codes.InvalidArgument)
		if err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("aliases %v: %v; want it to say %q", aliases, err, want)
		}
	}
	if all, _ := s.ListSecrets("demo"); len(all) != 0 {
		t.Errorf("refused creates stored %d secrets", len(all))
	}
}
