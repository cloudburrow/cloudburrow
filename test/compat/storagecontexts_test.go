//go:build compat

package compat

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// TestStorageObjectContexts (#1345): object contexts (contexts.custom) and
// the listing filter on them, with the official Go client on both
// transports (storage.NewGRPCClient and the JSON client). A context set at
// upload reads back with its value and times; an update adds, changes and
// deletes single keys (ObjectCustomContextPayload.Delete) and clears them
// all (an empty Custom); copy, rewrite and compose carry them, and body
// contexts override them; Query.Filter takes contexts."KEY":*,
// contexts."KEY"="VALUE" and their negations, and refuses anything else as
// INVALID_ARGUMENT / 400. Measured first: the JSON API refused contexts and
// gRPC answered UNIMPLEMENTED for Object.contexts and ListObjects.filter.
//
// covers: google.storage.v2.Storage/WriteObject, google.storage.v2.Storage/UpdateObject, google.storage.v2.Storage/ListObjects
// covers: google.storage.v2.Storage/RewriteObject, google.storage.v2.Storage/ComposeObject, google.storage.v2.Storage/GetObject
// covers: storage.objects.insert, storage.objects.patch, storage.objects.list, storage.objects.rewrite, storage.objects.compose
// covers: storage.objects.copy
func TestStorageObjectContexts(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, false)
	j := storageClient(t, h)
	ctx := h.Context()
	jb := bucket(t, h, j)
	gb := g.Bucket(jb.BucketName())
	custom := func(kv ...string) *storage.ObjectContexts {
		m := map[string]storage.ObjectCustomContextPayload{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = storage.ObjectCustomContextPayload{Value: kv[i+1]}
		}
		return &storage.ObjectContexts{Custom: m}
	}
	values := func(c *storage.ObjectContexts) map[string]string {
		out := map[string]string{}
		if c != nil {
			for k, v := range c.Custom {
				out[k] = v.Value
			}
		}
		return out
	}
	same := func(got map[string]string, kv ...string) bool {
		if len(got) != len(kv)/2 {
			return false
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if got[kv[i]] != kv[i+1] {
				return false
			}
		}
		return true
	}

	for name, bh := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
		p := strings.ToLower(name) + "-"
		// Upload with contexts.
		w := bh.Object(p + "a").NewWriter(ctx)
		w.Contexts = custom("team", "red", "tier", "gold")
		if _, err := io.WriteString(w, "a"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%s write with contexts: %v", name, err)
		}
		a := w.Attrs()
		if !same(values(a.Contexts), "team", "red", "tier", "gold") {
			t.Fatalf("%s written contexts %v", name, values(a.Contexts))
		}
		if c := a.Contexts.Custom["team"]; c.CreateTime.IsZero() || c.UpdateTime.IsZero() {
			t.Errorf("%s context times %+v", name, c)
		}
		// Both transports read what either wrote.
		for rn, rb := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
			got, err := rb.Object(p + "a").Attrs(ctx)
			if err != nil || !same(values(got.Contexts), "team", "red", "tier", "gold") {
				t.Errorf("%s reads %s's contexts: %v, %v", rn, name, values(got.Contexts), err)
			}
		}
		// Update: change one, add one, delete one.
		up := storage.ObjectAttrsToUpdate{Contexts: &storage.ObjectContexts{Custom: map[string]storage.ObjectCustomContextPayload{
			"team": {Value: "blue"}, "zone": {Value: "east"}, "tier": {Delete: true}}}}
		u, err := bh.Object(p+"a").Update(ctx, up)
		if err != nil {
			t.Fatalf("%s update contexts: %v", name, err)
		}
		if !same(values(u.Contexts), "team", "blue", "zone", "east") {
			t.Errorf("%s updated contexts %v; want team=blue zone=east", name, values(u.Contexts))
		}
		if u.Generation != a.Generation || u.Metageneration != a.Metageneration+1 {
			t.Errorf("%s update: generation %d->%d, metageneration %d->%d", name, a.Generation, u.Generation, a.Metageneration, u.Metageneration)
		}
		if c := u.Contexts.Custom["team"]; !c.UpdateTime.After(c.CreateTime) && !c.UpdateTime.Equal(c.CreateTime) {
			t.Errorf("%s team context times %+v", name, c)
		}

		// A second object without contexts, and one to clear.
		for _, n := range []string{"plain", "cleared"} {
			w := bh.Object(p + n).NewWriter(ctx)
			if n == "cleared" {
				w.Contexts = custom("team", "blue")
			}
			if _, err := io.WriteString(w, n); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		}
		c, err := bh.Object(p+"cleared").Update(ctx, storage.ObjectAttrsToUpdate{Contexts: &storage.ObjectContexts{Custom: map[string]storage.ObjectCustomContextPayload{}}})
		if err != nil || len(values(c.Contexts)) != 0 {
			t.Errorf("%s clear contexts: %v, %v", name, values(c.Contexts), err)
		}

		// A copy (rewrite) carries them; one with body contexts overrides.
		cp, err := bh.Object(p + "copy").CopierFrom(bh.Object(p + "a")).Run(ctx)
		if err != nil || !same(values(cp.Contexts), "team", "blue", "zone", "east") {
			t.Errorf("%s rewrite contexts %v, %v", name, values(cp.Contexts), err)
		}
		cop := bh.Object(p + "copy2").CopierFrom(bh.Object(p + "a"))
		cop.Contexts = custom("only", "this")
		if cp, err := cop.Run(ctx); err != nil || !same(values(cp.Contexts), "only", "this") {
			t.Errorf("%s rewrite with body contexts %v, %v", name, values(cp.Contexts), err)
		}
		// A compose carries the sources' contexts.
		comp, err := bh.Object(p+"composed").ComposerFrom(bh.Object(p+"a"), bh.Object(p+"plain")).Run(ctx)
		if err != nil || !same(values(comp.Contexts), "team", "blue", "zone", "east") {
			t.Errorf("%s compose contexts %v, %v", name, values(comp.Contexts), err)
		}

		// Listing filters.
		list := func(filter string) ([]string, error) {
			var out []string
			it := bh.Objects(ctx, &storage.Query{Prefix: p, Filter: filter})
			for {
				o, err := it.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					return nil, err
				}
				out = append(out, strings.TrimPrefix(o.Name, p))
			}
			sort.Strings(out)
			return out, nil
		}
		for _, c := range []struct{ filter, want string }{
			{`contexts."team":*`, "a,composed,copy"},
			{`contexts."team"="blue"`, "a,composed,copy"},
			{`contexts."team"="red"`, ""},
			{`contexts."only"="this"`, "copy2"},
			{`-contexts."team":*`, "cleared,copy2,plain"},
			{`NOT contexts."zone"="east"`, "cleared,copy2,plain"},
		} {
			got, err := list(c.filter)
			if err != nil || strings.Join(got, ",") != c.want {
				t.Errorf("%s filter %s: %v, %v; want %s", name, c.filter, got, err, c.want)
			}
		}
		for _, f := range []string{`contexts."team"="blue" AND contexts."zone":*`, `name="a"`, `contexts.team`} {
			_, err := list(f)
			var ge *googleapi.Error
			switch {
			case errors.As(err, &ge) && ge.Code == http.StatusBadRequest:
			case grpcstatus.Code(err) == codes.InvalidArgument:
			default:
				t.Errorf("%s filter %s: %v; want 400 / INVALID_ARGUMENT", name, f, err)
			}
		}
		// A reserved or malformed key is refused.
		if _, err := bh.Object(p+"a").Update(ctx, storage.ObjectAttrsToUpdate{Contexts: custom("google:x", "y")}); err == nil {
			t.Errorf("%s accepted a google: context key", name)
		}
	}

	// dropContextGroups=custom on the JSON API's copy drops them.
	hc := &http.Client{}
	u := h.Endpoint(EnvStorage) + "/storage/v1/b/" + jb.BucketName() + "/o/json-a/copyTo/b/" + jb.BucketName() + "/o/dropped?dropContextGroups=custom"
	resp, err := hc.Post(u, "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("copy with dropContextGroups=custom: %s", resp.Status)
	}
	if a, err := jb.Object("dropped").Attrs(ctx); err != nil || a.Contexts != nil && len(a.Contexts.Custom) > 0 {
		t.Errorf("dropContextGroups=custom kept %v, %v", values(a.Contexts), err)
	}
	resp, err = hc.Post(strings.Replace(u, "=custom", "="+url.QueryEscape("google"), 1), "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("dropContextGroups=google: %s; want 400", resp.Status)
	}
}
