//go:build browser

package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/chromedp/chromedp"
)

// ownRules let a Firebase user read and write their own users document.
const ownRules = `rules_version = '2';
service cloud.firestore {
  match /databases/{database}/documents {
    match /users/{uid} {
      allow read, write: if request.auth != null && request.auth.uid == uid;
    }
  }
}`

// firebaseToken is an unsigned Firebase ID token for uid, as the Firestore
// emulator accepts one (firebase.google.com/docs/emulator-suite/connect_firestore).
func firebaseToken(project, uid string) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims := fmt.Sprintf(`{"iss":"https://securetoken.google.com/%s","aud":"%s","sub":"%s","user_id":"%s","iat":%d,"exp":%d,"auth_time":%d,"firebase":{"sign_in_provider":"password","identities":{}}}`,
		project, project, uid, uid, time.Now().Unix(), time.Now().Add(time.Hour).Unix(), time.Now().Unix())
	return enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc([]byte(claims)) + "."
}

// clientGet is a Firebase client SDK's get of a document through the
// Firestore REST API, as uid ("" for an unauthenticated client).
func clientGet(t *testing.T, addr, project, doc, uid string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/projects/"+project+"/databases/(default)/documents/"+doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Goog-Api-Client", "gl-js/ fire/12.0.0")
	if uid != "" {
		req.Header.Set("Authorization", "Bearer "+firebaseToken(project, uid))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// rulesDatabase is a project whose (default) database holds users/alice
// and three items, written with the official server client library, and
// one query run over them.
func rulesDatabase(t *testing.T) (project, addr string) {
	t.Helper()
	needService(t, "firestore")
	addr = strings.TrimPrefix(strings.TrimSpace(os.Getenv(envFirestore)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: every change is read back through the APIs themselves", envFirestore)
	}
	project = uniqueProject(t)
	ctx := context.Background()
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	c, err := firestore.NewClient(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Doc("users/alice").Set(ctx, map[string]any{"name": "Alice"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := c.Doc(fmt.Sprintf("items/i%d", i)).Set(ctx, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Doc("items/i3").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	docs, err := c.Collection("items").Where("n", ">", 0).Documents(ctx).GetAll()
	if err != nil || len(docs) != 2 {
		t.Fatalf("the query = %d documents, %v; want 2", len(docs), err)
	}
	return project, addr
}

// TestFirestoreRulesUsageAndInsightsThroughThePage (#2048), in the
// emulators shard: Google's Security, Usage and Query insights pages
// (firebase.google.com/docs/firestore/security/get-started,
// docs.cloud.google.com/firestore/native/docs/monitor-usage and
// …/query-insights).
//   - Security in the menu opens (default)'s Rules tab; Edit rules, then
//     Publish, releases the rules, which the page then shows, and which a
//     Firebase client's request is then held to: alice reads her own
//     document, bob is refused, and the server client library still reads
//     it, as on Google;
//   - the Rules Playground simulates get /users/alice as bob (denied) and as
//     alice (allowed);
//   - Usage says how many documents were read, written and deleted;
//   - Query insights lists the query run, normalized, with its results.
func TestFirestoreRulesUsageAndInsightsThroughThePage(t *testing.T) {
	project, addr := rulesDatabase(t)
	p := open(t)
	q := "?project=" + project

	p.navigate("/firestore-databases" + q)
	p.waitFor(`document.querySelectorAll("#product-nav-pages a").length >= 11`)
	p.clickText("#product-nav-pages a", "Security")
	p.waitFor(`location.pathname === "/firestore-databases/(default)" && new URLSearchParams(location.search).get("tab") === "rules" &&
		document.querySelector("#section-actions-rules button") !== null`)
	if code := clientGet(t, addr, project, "users/alice", "bob"); code != http.StatusOK {
		t.Errorf("before any rules are published, a client get = %d, want 200", code)
	}

	p.clickText("#section-actions-rules button", "Edit rules")
	p.waitFor(`document.querySelector(".modal.is-open #f-source") !== null`)
	p.setField(".modal.is-open #f-source", ownRules)
	p.clickText(`.modal.is-open button[type="submit"]`, "Publish")
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#view").textContent.includes("request.auth.uid == uid")`)

	if code := clientGet(t, addr, project, "users/alice", "alice"); code != http.StatusOK {
		t.Errorf("after Publish, alice's get of her own document = %d, want 200", code)
	}
	if code := clientGet(t, addr, project, "users/alice", "bob"); code != http.StatusForbidden {
		t.Errorf("after Publish, bob's get of alice's document = %d, want 403", code)
	}
	if code := clientGet(t, addr, project, "users/alice", ""); code != http.StatusForbidden {
		t.Errorf("after Publish, an unauthenticated client's get = %d, want 403", code)
	}
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	sc, err := firestore.NewClient(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	if _, err := sc.Doc("users/alice").Get(context.Background()); err != nil {
		t.Errorf("after Publish, the server client library's get = %v; want it to bypass rules", err)
	}

	for _, c := range []struct{ uid, verdict string }{{"bob", "denied"}, {"alice", "allowed"}} {
		p.clickText("#section-actions-rules button", "Rules Playground")
		p.waitFor(`document.querySelector(".modal.is-open #f-location") !== null`)
		p.setField(".modal.is-open #f-method", "get")
		p.setField(".modal.is-open #f-location", "/users/alice")
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-authenticated`)))
		p.setField(".modal.is-open #f-uid", c.uid)
		p.clickText(`.modal.is-open button[type="submit"]`, "Run")
		p.waitFor(fmt.Sprintf(`document.body.textContent.includes("Simulated get %s")`, c.verdict))
		p.navigate("/firestore-databases/(default)" + q + "&tab=rules")
		p.waitFor(`document.querySelector("#section-actions-rules button") !== null`)
	}

	// Usage: 1 + 3 written, one deleted, 2 queried, 1 looked up.
	p.clickText("#product-nav-pages a", "Usage")
	p.waitFor(`new URLSearchParams(location.search).get("tab") === "usage" && /Last 60 minutes: [1-9]\d* reads, [4-9]\d* writes, [1-9]\d* deletes/.test(document.querySelector("#view").textContent)`)

	p.clickText("#product-nav-pages a", "Insights")
	p.waitFor(`new URLSearchParams(location.search).get("tab") === "insights" &&
		document.querySelector("#view").textContent.includes("/items | where n > ?")`)
}

// TestFirestoreRulesScreensMeetWCAG (#2048): the Rules tab, its Edit rules
// and Rules Playground dialogs, Usage and Query insights.
func TestFirestoreRulesScreensMeetWCAG(t *testing.T) {
	project, _ := rulesDatabase(t)
	base := "/firestore-databases/(default)?project=" + project
	ready := settled + ` && document.querySelector("#section-actions-rules button") !== null`
	auditScreens(t, []a11yPage{
		{"Firestore Rules tab", base + "&tab=rules", ready, ""},
		{"Edit rules dialog", base + "&tab=rules", ready,
			`[...document.querySelectorAll("#section-actions-rules button")].find((b) => b.textContent.trim() === "Edit rules").click()`},
		{"Rules Playground dialog", base + "&tab=rules", ready,
			`[...document.querySelectorAll("#section-actions-rules button")].find((b) => b.textContent.trim() === "Rules Playground").click()`},
		{"Firestore Usage tab", base + "&tab=usage", settled + ` && document.querySelector("#view").textContent.includes("Last 60 minutes")`, ""},
		{"Firestore Query insights tab", base + "&tab=insights", settled + ` && document.querySelector("#view").textContent.includes("/items")`, ""},
	})
}
