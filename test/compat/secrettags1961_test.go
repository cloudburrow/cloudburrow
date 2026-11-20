//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSecretManagerTagsAndAliasesAtCreation (#1961), through the official
// secretmanager/apiv1 and resourcemanager/apiv3 clients: CreateSecret with
// tags (a key by its namespaced name with a value's short name, and a key
// by its permanent ID with a value's ID) binds them to the secret's full
// resource name, //secretmanager.googleapis.com/projects/PROJECT/secrets/ID,
// where ListTagBindings and ListEffectiveTags find them; GetSecret returns
// no tags (input only). An unknown key is INVALID_ARGUMENT with no secret
// stored, and DeleteSecret removes the bindings. version_aliases at
// creation are checked as UpdateSecret checks them: a reserved or badly
// formed alias, and one naming a version a new secret cannot have, are
// INVALID_ARGUMENT.
//
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/DeleteSecret, google.cloud.resourcemanager.v3.TagBindings/ListTagBindings, google.cloud.resourcemanager.v3.TagBindings/ListEffectiveTags
func TestSecretManagerTagsAndAliasesAtCreation(t *testing.T) {
	h := New(t)
	sm := secretsClient(t, h)
	c := newTagClients(t, h)
	ctx := h.Context()

	mkKey := func(short string) *rmpb.TagKey {
		op, err := c.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: short}})
		if err != nil {
			t.Fatal(err)
		}
		k, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = c.keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: k.GetName()}) })
		return k
	}
	mkValue := func(k *rmpb.TagKey, short string) *rmpb.TagValue {
		op, err := c.values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: k.GetName(), ShortName: short}})
		if err != nil {
			t.Fatal(err)
		}
		v, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = c.values.DeleteTagValue(context.Background(), &rmpb.DeleteTagValueRequest{Name: v.GetName()})
		})
		return v
	}
	env := mkKey(v1ID(h, "senv-"))
	prod := mkValue(env, "prod")
	team := mkKey(v1ID(h, "steam-"))
	core := mkValue(team, "core")

	id := "tagged-" + h.Project()
	name := secretsParent(h) + "/secrets/" + id
	full := "//secretmanager.googleapis.com/" + name
	bound := func() map[string]bool {
		out := map[string]bool{}
		for it := c.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: full}); ; {
			b, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListTagBindings(%s): %v", full, err)
			}
			out[b.GetTagValue()] = true
		}
	}

	_, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: autoReplication(), Tags: map[string]string{"tagKeys/999999999": "tagValues/999999999"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateSecret with an unknown tag key = %v; want INVALID_ARGUMENT", err)
	}
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Fatalf("the secret refused for its tags exists: %v", err)
	}

	t.Cleanup(func() { _ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
	created, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: autoReplication(), Tags: map[string]string{
			env.GetNamespacedName(): "prod",
			team.GetName():          core.GetName(),
		}}})
	if err != nil {
		t.Fatalf("CreateSecret with tags: %v", err)
	}
	if len(created.GetTags()) != 0 {
		t.Errorf("CreateSecret returned tags %v; the field is input only", created.GetTags())
	}
	if got := bound(); !got[prod.GetName()] || !got[core.GetName()] || len(got) != 2 {
		t.Errorf("ListTagBindings(%s) = %v; want %s and %s", full, got, prod.GetName(), core.GetName())
	}
	eff := map[string]bool{}
	for it := c.bindings.ListEffectiveTags(ctx, &rmpb.ListEffectiveTagsRequest{Parent: full}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListEffectiveTags: %v", err)
		}
		if !e.GetInherited() {
			eff[e.GetNamespacedTagValue()] = true
		}
	}
	if !eff[prod.GetNamespacedName()] || !eff[core.GetNamespacedName()] {
		t.Errorf("ListEffectiveTags(%s) = %v; want %s and %s, not inherited", full, eff, prod.GetNamespacedName(), core.GetNamespacedName())
	}
	if got, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); err != nil || len(got.GetTags()) != 0 {
		t.Errorf("GetSecret = tags %v, %v; want none (input only)", got.GetTags(), err)
	}
	if err := sm.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if got := bound(); len(got) != 0 {
		t.Errorf("after DeleteSecret the bindings are %v; want none", got)
	}

	for _, tc := range []struct {
		aliases map[string]int64
		says    string
	}{
		{map[string]int64{"latest": 1}, "reserved"},
		{map[string]int64{"1st": 1}, "must start with a letter"},
		{map[string]int64{"current": 1}, "does not exist"},
	} {
		_, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id + "-a",
			Secret: &secretmanagerpb.Secret{Replication: autoReplication(), VersionAliases: tc.aliases}})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), tc.says) {
			t.Errorf("CreateSecret version_aliases %v = %v; want INVALID_ARGUMENT saying %q", tc.aliases, err, tc.says)
		}
	}
}

// TestTerraformSecretTags (#1961): a google_secret_manager_secret whose
// tags name a google_tags_tag_key's namespaced name and its
// google_tags_tag_value's short name applies; ListTagBindings on the
// secret's full resource name finds the value; the plan is clean (tags are
// input only); destroy removes the secret and its binding.
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.resourcemanager.v3.TagBindings/ListTagBindings
func TestTerraformSecretTags(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	sm := secretsClient(t, h)
	c := newTagClients(t, h)
	ctx := h.Context()
	short := v1ID(h, "tfsk-")
	id := "tf-tags-" + h.Project()
	full := "//secretmanager.googleapis.com/projects/" + h.Project() + "/secrets/" + id
	m.write(fmt.Sprintf(`
resource "google_tags_tag_key" "k" {
  parent     = %q
  short_name = %q
}
resource "google_tags_tag_value" "v" {
  parent     = google_tags_tag_key.k.id
  short_name = "prod"
}
resource "google_secret_manager_secret" "s" {
  project   = %q
  secret_id = %q
  tags = {
    (google_tags_tag_key.k.namespaced_name) = google_tags_tag_value.v.short_name
  }
  replication {
    auto {}
  }
}
`, localOrg, short, h.Project(), id))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	b, err := c.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: full}).Next()
	if err != nil || b.GetTagValueNamespacedName() != "100000000001/"+short+"/prod" {
		t.Errorf("after apply the secret's binding is %v, %v; want 100000000001/%s/prod", b, err, short)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/" + h.Project() + "/secrets/" + id}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSecret after destroy = %v; want NOT_FOUND", err)
	}
	if _, err := c.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: full}).Next(); err != iterator.Done {
		t.Errorf("after destroy the secret still has a binding: %v", err)
	}
}
