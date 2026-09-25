package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// seed writes objects straight into the metadata store, which is what a
// finalize does, so large listings need no uploads.
func seed(t *testing.T, s *Server, bucket string, names ...string) {
	t.Helper()
	err := s.meta.Update(func(tx Tx) error {
		for _, n := range names {
			o := objectRecord{Bucket: bucket, Name: n, Generation: s.nextGeneration(), Metageneration: 1, ContentType: "text/plain", StorageClass: "STANDARD"}
			if err := putObject(tx, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func listAll(t *testing.T, it *gcs.ObjectIterator) (names, prefixes []string) {
	t.Helper()
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if a.Prefix != "" {
			prefixes = append(prefixes, a.Prefix)
		} else {
			names = append(names, a.Name)
		}
	}
}

func listServer(t *testing.T, bucket string) (*gcs.Client, *Server) {
	t.Helper()
	c, _ := sdk(t)
	if err := c.Bucket(bucket).Create(context.Background(), "demo-project", nil); err != nil {
		t.Fatal(err)
	}
	return c, lastServer
}

// Prefix and delimiter: direct children and one synthetic prefix per level.
func TestStorageListWithPrefixInProcess(t *testing.T) {
	c, s := listServer(t, "lst")
	seed(t, s, "lst", "a.txt", "dir/one", "dir/two", "dir/sub/three", "dir/sub/four", "dir/", "other")
	names, prefixes := listAll(t, c.Bucket("lst").Objects(context.Background(), &gcs.Query{Prefix: "dir/", Delimiter: "/"}))
	if strings.Join(names, ",") != "dir/,dir/one,dir/two" || strings.Join(prefixes, ",") != "dir/sub/" {
		t.Errorf("prefix dir/ delimiter / = items %v prefixes %v", names, prefixes)
	}
	names, prefixes = listAll(t, c.Bucket("lst").Objects(context.Background(), &gcs.Query{Delimiter: "/"}))
	if strings.Join(names, ",") != "a.txt,other" || strings.Join(prefixes, ",") != "dir/" {
		t.Errorf("delimiter / = items %v prefixes %v", names, prefixes)
	}
	names, prefixes = listAll(t, c.Bucket("lst").Objects(context.Background(), &gcs.Query{Delimiter: "/", IncludeTrailingDelimiter: true}))
	if strings.Join(names, ",") != "a.txt,dir/,other" || strings.Join(prefixes, ",") != "dir/" {
		t.Errorf("includeTrailingDelimiter = items %v prefixes %v", names, prefixes)
	}
}

// 2,500 objects arrive across three pages, in order, with no duplicates.
func TestStorageListPaginates(t *testing.T) {
	c, s := listServer(t, "many")
	var names []string
	for i := 0; i < 2500; i++ {
		names = append(names, fmt.Sprintf("obj-%05d", i))
	}
	seed(t, s, "many", names...)
	pager := iterator.NewPager(c.Bucket("many").Objects(context.Background(), nil), 1000, "")
	var got []string
	pages := 0
	for {
		var page []*gcs.ObjectAttrs
		tok, err := pager.NextPage(&page)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, a := range page {
			got = append(got, a.Name)
		}
		if tok == "" {
			break
		}
	}
	if pages != 3 || len(got) != 2500 || !sort.StringsAreSorted(got) || got[0] != "obj-00000" || got[2499] != "obj-02499" {
		t.Errorf("%d pages, %d names (sorted %v)", pages, len(got), sort.StringsAreSorted(got))
	}
	dup := map[string]bool{}
	for _, n := range got {
		if dup[n] {
			t.Fatalf("%s listed twice", n)
		}
		dup[n] = true
	}
}

// startOffset (inclusive), endOffset (exclusive) and matchGlob.
func TestStorageListOffsetsAndGlob(t *testing.T) {
	c, s := listServer(t, "glob")
	seed(t, s, "glob", "a/x.txt", "a/y.csv", "a/b/z.txt", "b.txt", "c.txt", "d.jpg", "e.txt")
	ctx := context.Background()
	names, _ := listAll(t, c.Bucket("glob").Objects(ctx, &gcs.Query{StartOffset: "b.txt", EndOffset: "e.txt"}))
	if strings.Join(names, ",") != "b.txt,c.txt,d.jpg" {
		t.Errorf("offsets = %v", names)
	}
	for glob, want := range map[string]string{
		"*.txt":       "b.txt,c.txt,e.txt",
		"**.txt":      "a/b/z.txt,a/x.txt,b.txt,c.txt,e.txt",
		"a/*":         "a/x.txt,a/y.csv",
		"a/**":        "a/b/z.txt,a/x.txt,a/y.csv",
		"?.txt":       "b.txt,c.txt,e.txt",
		"[bc].txt":    "b.txt,c.txt",
		"[!bc].*":     "d.jpg,e.txt",
		"*.{txt,jpg}": "b.txt,c.txt,d.jpg,e.txt",
		"a/{x,y}.*":   "a/x.txt,a/y.csv",
	} {
		names, _ := listAll(t, c.Bucket("glob").Objects(ctx, &gcs.Query{MatchGlob: glob}))
		if strings.Join(names, ",") != want {
			t.Errorf("matchGlob %s = %v, want %s", glob, names, want)
		}
	}
}

// Synthetic prefixes count toward maxResults, and a page ending on a
// prefix resumes after everything under it.
func TestStorageListPrefixesCountTowardMaxResults(t *testing.T) {
	_, s := listServer(t, "cap")
	seed(t, s, "cap", "a", "d1/x", "d1/y", "d2/x", "z")
	h := lastHTTP
	var resp struct {
		Items    []struct{ Name string } `json:"items"`
		Prefixes []string                `json:"prefixes"`
		Next     string                  `json:"nextPageToken"`
	}
	var names []string
	token := ""
	for page := 0; page < 5; page++ {
		url := h.URL + "/storage/v1/b/cap/o?delimiter=/&maxResults=2"
		if token != "" {
			url += "&pageToken=" + token
		}
		code, body := raw(t, "GET", url, "")
		if code != 200 {
			t.Fatal(body)
		}
		resp.Items, resp.Prefixes, resp.Next = nil, nil, ""
		_ = json.Unmarshal([]byte(body), &resp)
		if n := len(resp.Items) + len(resp.Prefixes); n > 2 {
			t.Errorf("page %d has %d entries, over maxResults=2", page, n)
		}
		for _, i := range resp.Items {
			names = append(names, i.Name)
		}
		names = append(names, resp.Prefixes...)
		if resp.Next == "" {
			break
		}
		token = resp.Next
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a,d1/,d2/,z" {
		t.Errorf("across pages = %v", names)
	}
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/cap/o?softDeleted=true", ""); code != 501 {
		t.Errorf("softDeleted=true = %d %s; want 501 until #499", code, body)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/absent/o", ""); code != 404 {
		t.Errorf("a missing bucket = %d", code)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/cap/o?matchGlob=[ab", ""); code != 400 {
		t.Errorf("a bad glob = %d", code)
	}
}
