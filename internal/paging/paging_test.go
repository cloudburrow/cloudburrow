package paging

import (
	"errors"
	"fmt"
	"testing"
)

func keys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("item-%03d", i)
	}
	return out
}

// Walking every page must return each item exactly once. Non-deterministic
// ordering shows up here as a duplicate or a gap.
func TestPagesCoverEveryItemExactlyOnce(t *testing.T) {
	t.Parallel()
	all := keys(250)
	seen := map[string]int{}
	tokenStr := ""
	pages := 0
	for {
		page, next, err := Page("scope", all, tokenStr, 40)
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		for _, k := range page {
			seen[k]++
		}
		pages++
		if next == "" {
			break
		}
		tokenStr = next
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != len(all) {
		t.Errorf("saw %d distinct items, want %d", len(seen), len(all))
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("item %s returned %d times", k, n)
		}
	}
}

// Ordering must not depend on input order, or the same listing could return
// an item twice across pages.
func TestOrderingIsDeterministic(t *testing.T) {
	t.Parallel()
	shuffled := []string{"c", "a", "e", "b", "d"}
	page, _, err := Page("scope", shuffled, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b", "c", "d", "e"}
	for i := range want {
		if page[i] != want[i] {
			t.Fatalf("page = %v, want %v", page, want)
		}
	}
}

// An invalid token must be rejected. Treating it as a first page makes a
// client loop forever without ever seeing an error.
func TestInvalidTokensAreRejected(t *testing.T) {
	t.Parallel()
	for _, tok := range []string{"not-base64!!", "YWJj", "%%%"} {
		if _, _, err := Page("scope", keys(5), tok, 10); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Page(token=%q) = %v, want ErrInvalidToken", tok, err)
		}
	}
}

// A token from a different listing must not silently apply here.
func TestTokenFromAnotherListingIsRejected(t *testing.T) {
	t.Parallel()
	_, next, err := Page("listing-a", keys(50), "", 10)
	if err != nil || next == "" {
		t.Fatalf("setup: %v, next=%q", err, next)
	}
	if _, _, err := Page("listing-b", keys(50), next, 10); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("cross-listing token = %v, want ErrInvalidToken", err)
	}
}

// A deleted cursor item must not restart the listing.
func TestResumeAfterDeletedCursor(t *testing.T) {
	t.Parallel()
	all := keys(10)
	page, next, err := Page("s", all, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	last := page[len(page)-1]

	remaining := []string{}
	for _, k := range all {
		if k != last {
			remaining = append(remaining, k)
		}
	}
	page2, _, err := Page("s", remaining, next, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range page2 {
		if k <= last {
			t.Errorf("page resumed at or before the deleted cursor: got %s, cursor %s", k, last)
		}
	}
}

func TestClampPageSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[int]int{0: DefaultPageSize, -5: DefaultPageSize, 10: 10, MaxPageSize + 1: MaxPageSize} {
		if got := ClampPageSize(in); got != want {
			t.Errorf("ClampPageSize(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestEmptyAndExactPage(t *testing.T) {
	t.Parallel()
	page, next, err := Page("s", nil, "", 10)
	if err != nil || len(page) != 0 || next != "" {
		t.Errorf("empty listing = %v, %q, %v", page, next, err)
	}
	// A page that exactly consumes the listing must not offer a next token.
	page, next, err = Page("s", keys(5), "", 5)
	if err != nil || len(page) != 5 || next != "" {
		t.Errorf("exact page = %d items, next=%q, err=%v", len(page), next, err)
	}
}
