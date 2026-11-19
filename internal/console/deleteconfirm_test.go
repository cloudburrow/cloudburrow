package console

import (
	"encoding/json"
	"testing"
)

// confirmingProvider names its own delete confirmation.
type confirmingProvider struct {
	mutableProvider
	style string
}

func (c *confirmingProvider) DeleteConfirm() string { return c.style }

// projectsLike creates through a page of the console's own.
type projectsLike struct{ mutableProvider }

func (projectsLike) CreateHref() string { return "/projectcreate" }

// TestDeleteConfirmIsAdvertisedPerKind (#1931): the capability carries how
// Google's console confirms each kind's delete: a documented row of
// DeleteConfirmRules, a provider's own DeleteConfirm, and nothing for the
// default (the name typed back), which is also what an unknown style, or a
// kind that cannot be deleted, gets. A create flow that is a page of the
// console's own (#1932) is advertised as a page at that address.
func TestDeleteConfirmIsAdvertisedPerKind(t *testing.T) {
	DeleteConfirmRules["rule-word"] = DeleteConfirmRule{Style: DeleteConfirmWord, Source: "https://docs.cloud.google.com/x", Verified: true}
	t.Cleanup(func() { delete(DeleteConfirmRules, "rule-word") })
	srv := serve(t,
		&mutableProvider{fakeProvider: fakeProvider{id: "rule-word", title: "Word"}},
		&confirmingProvider{mutableProvider: mutableProvider{fakeProvider: fakeProvider{id: "own-click", title: "Click"}}, style: DeleteConfirmClick},
		&confirmingProvider{mutableProvider: mutableProvider{fakeProvider: fakeProvider{id: "own-bogus", title: "Bogus"}}, style: "please"},
		&mutableProvider{fakeProvider: fakeProvider{id: "default", title: "Default"}},
		fakeProvider{id: "readonly", title: "Read only"},
		&projectsLike{mutableProvider{fakeProvider: fakeProvider{id: "paged", title: "Paged"}}},
	)
	_, body := get(t, srv, "/api/services", nil)
	var got struct {
		Services []struct {
			ID            string `json:"id"`
			DeleteConfirm string `json:"deleteConfirm"`
			Create        *struct {
				Page bool   `json:"page"`
				Href string `json:"href"`
			} `json:"create"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	want := map[string]string{"rule-word": "delete", "own-click": "confirm", "own-bogus": "", "default": "", "readonly": "", "paged": ""}
	seen := 0
	for _, s := range got.Services {
		w, ok := want[s.ID]
		if !ok {
			continue
		}
		seen++
		if s.DeleteConfirm != w {
			t.Errorf("%s: deleteConfirm %q, want %q", s.ID, s.DeleteConfirm, w)
		}
		if s.ID == "paged" && (s.Create == nil || !s.Create.Page || s.Create.Href != "/projectcreate") {
			t.Errorf("a CreateHrefer's create is %+v; want a page at /projectcreate", s.Create)
		}
		if s.ID == "default" && (s.Create == nil || s.Create.Href != "") {
			t.Errorf("a plain Creator's create is %+v; want no href", s.Create)
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d services in %s", seen, len(want), body)
	}
}
