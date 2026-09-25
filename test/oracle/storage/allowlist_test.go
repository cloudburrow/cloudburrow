//go:build oracle

package storageoracle

import (
	"strings"
	"testing"
)

// allowlist explains every known difference between the testbench and the
// builtin server. An entry is "<step> <path>" or "* <path>" for any step, and
// it covers everything under its path. Any other difference fails, and so does
// an entry that explained nothing in a full run (TestMain).
//
// The testbench is a reference, not ground truth. An entry is not evidence
// that the builtin server is wrong, and agreement is not evidence that it is
// right: where they differ, Google's docs decide (#497). O: is the Object
// resource in the discovery document the server embeds
// (internal/service/storage/storage-api.json, schemas.Object).
var allowlist = map[string]string{
	// Fields the testbench leaves out and the docs define.
	"* body.mediaLink":                        "testbench omits it; O: mediaLink is the object's media download link",
	"* body.resource.mediaLink":               "as * body.mediaLink, in a rewrite's resource",
	"* body.selfLink":                         "testbench omits it; O: selfLink is the link to this object",
	"* body.resource.selfLink":                "as * body.selfLink, in a rewrite's resource",
	"* body.storageClass":                     "testbench omits it; O: storageClass is the object's storage class, the bucket's default here",
	"* body.resource.storageClass":            "as * body.storageClass, in a rewrite's resource",
	"* body.timeStorageClassUpdated":          "testbench omits it; O: \"When the object is initially created, it will be set to timeCreated\"",
	"* body.resource.timeStorageClassUpdated": "as * body.timeStorageClassUpdated, in a rewrite's resource",
	"compose body.componentCount":             "testbench omits it; O: componentCount counts the components \"accumulated by compose operations\"",
	"get composed body.componentCount":        "as compose body.componentCount",
	"insert media body.contentType": "testbench drops the Content-Type of an uploadType=media request; the docs' simple " +
		"upload takes the object's content type from that header (https://cloud.google.com/storage/docs/uploading-objects)",
	"get body.contentType":           "follows from insert media body.contentType",
	"get rewritten body.contentType": "follows from insert media body.contentType: the rewrite copies it",
	"* body.resource.contentType":    "follows from insert media body.contentType: the rewrite copies it",
	"compose body.md5Hash": "testbench gives a composite object an MD5; Google's docs say composite objects have no MD5 " +
		"hash, only a CRC32C (https://cloud.google.com/storage/docs/composite-objects), and the builtin server omits it (#495)",
	"get composed body.md5Hash": "as compose body.md5Hash",
	"* header:X-Goog-Storage-Class": "testbench omits it; x-goog-storage-class is a documented response header " +
		"(https://cloud.google.com/storage/docs/xml-api/reference-headers)",
	"* header:X-Goog-Stored-Content-Encoding": "testbench omits it; x-goog-stored-content-encoding is a documented response " +
		"header (https://cloud.google.com/storage/docs/xml-api/reference-headers)",

	// ACLs, which the builtin server does not implement.
	"* body.acl": "testbench returns object ACLs (projection=full on patch, compose and a finished upload); the builtin " +
		"server implements no ACL methods and refuses acl in a body by name, as with uniform bucket-level access",
	"* body.owner":          "as * body.acl: owner is returned with ACLs, under projection=full",
	"* body.resource.acl":   "as * body.acl, in a rewrite's resource",
	"* body.resource.owner": "as * body.owner, in a rewrite's resource",

	// Holds are not compared (notCompared).
	"* body.eventBasedHold":          "testbench writes eventBasedHold: false on some objects; holds are built to the docs alone (#500)",
	"* body.resource.eventBasedHold": "as * body.eventBasedHold, in a rewrite's resource",

	// Protocol details the docs leave open.
	"resumable start header:Location": "the session URI is opaque: a client uses it as returned " +
		"(https://cloud.google.com/storage/docs/performing-resumable-uploads). The builtin server keeps name= in it; the testbench does not",
	"resumable one-shot start header:Location": "as resumable start header:Location",
	"resumable query with X-GUploader-No-308 status": "the builtin server honours X-GUploader-No-308 on a status query as on a " +
		"chunk (200 with X-Http-Status-Code-Override: 308); the testbench honours it on a chunk only (rest_server.py:1232-1236). " +
		"Google does not document the header; the builtin behaviour is UNVERIFIED",
	"resumable query with X-GUploader-No-308 header:X-Http-Status-Code-Override": "as resumable query with X-GUploader-No-308 status",
	"* header:Accept-Ranges": "the builtin server sends Accept-Ranges: bytes on a download (RFC 9110 §14.3); the testbench " +
		"does not. The docs neither require nor refuse it",
	"download whole header:Content-Range": "testbench sends Content-Range on a 200 with no Range; RFC 9110 §14.4 defines " +
		"it for 206 and 416, and the builtin server sends it only there",
}

// notCompared are the behaviours the oracle deliberately never exercises,
// with the reason. They are decided by the docs alone.
var notCompared = map[string]string{
	"error responses": "only the status of an error is compared, not its headers or body: an error's wording is not " +
		"a documented contract",
	"object versioning": "the testbench keeps old generations even on an unversioned bucket (database.py:467-517), " +
		"so a versioning comparison would compare against a known departure",
	"holds and retention":     "excluded by the council's plan (#497) and built to the docs alone (#500)",
	"non-empty bucket delete": "excluded by the council's plan (#497); the builtin server's refusal is built to the docs alone",
	"paging":                  "the testbench's objects.list returns no nextPageToken (rest_server.py:506-517)",
}

// TestOracleAllowlistIsExplicit checks the comparison itself, with no
// server: an unlisted difference fails, a listed one does not, an entry
// covers its path's subtree only, "*" covers any step, an entry that
// explained nothing is stale, and every entry has a reason.
func TestOracleAllowlistIsExplicit(t *testing.T) {
	tb := observed{"status": "200", "body.name": `"a"`, "body.size": `"1"`, "body.acl[0].role": `"OWNER"`}
	cb := observed{"status": "200", "body.name": `"a"`, "body.size": `"2"`, "header:Range": "bytes=0-1"}
	diffs := diff("get", tb, cb)
	if len(diffs) != 3 {
		t.Fatalf("diff = %v, want body.size, body.acl[0].role and header:Range", diffs)
	}

	allow := map[string]string{"get body.size": "r", "* body.acl": "r", "get missing status": "r", "* body.ac": "r"}
	used := map[string]bool{}
	got := unexplained(diffs, allow, used)
	if len(got) != 1 || !strings.Contains(got[0], `unlisted difference "get header:Range"`) {
		t.Errorf("unexplained = %v, want only get header:Range", got)
	}
	if !used["get body.size"] || !used["* body.acl"] || used["* body.ac"] {
		t.Errorf("used = %v: want the exact entry and the subtree entry, not a name prefix", used)
	}
	if got := stale(allow, used); len(got) != 2 || !strings.Contains(got[0], `"* body.ac"`) ||
		!strings.Contains(got[1], `"get missing status"`) {
		t.Errorf("stale = %v, want * body.ac and get missing status", got)
	}
	if _, ok := explain("download whole header:Range", map[string]string{"download header:Range": "r"}); ok {
		t.Error("an entry for step \"download\" explained step \"download whole\"")
	}

	for k, v := range allowlist {
		if strings.TrimSpace(v) == "" {
			t.Errorf("allowlist entry %q has no reason", k)
		}
		if !strings.Contains(k, " ") {
			t.Errorf("allowlist entry %q is not \"<step> <path>\"", k)
		}
	}
	for k, v := range notCompared {
		if strings.TrimSpace(v) == "" {
			t.Errorf("notCompared %q has no reason", k)
		}
	}
}

// TestOracleNormalizes checks that each server's own values are rewritten
// out before comparing: host, generation, upload ID and volatile fields.
func TestOracleNormalizes(t *testing.T) {
	tg := &target{base: "http://127.0.0.1:9000"}
	got := tg.normalize("", map[string]any{
		"mediaLink":  "http://127.0.0.1:9000/download/storage/v1/b/b/o/x?generation=17&alt=media",
		"generation": "17",
		"metadata":   map[string]any{"k": "v", "x_emulator_upload": "resumable"},
	}).(map[string]any)
	if got["mediaLink"] != "<host>/download/storage/v1/b/b/o/x?generation=<gen>&alt=media" {
		t.Errorf("mediaLink = %v", got["mediaLink"])
	}
	if got["generation"] != "<set>" {
		t.Errorf("generation = %v", got["generation"])
	}
	if md := got["metadata"].(map[string]any); len(md) != 1 || md["k"] != "v" {
		t.Errorf("metadata = %v", md)
	}
	if s := tg.normalizeString("Location", "http://127.0.0.1:9000/upload/storage/v1/b/b/o?uploadType=resumable&upload_id=abc"); s != "<host>/upload/storage/v1/b/b/o?uploadType=resumable&upload_id=<id>" {
		t.Errorf("Location = %s", s)
	}
}
