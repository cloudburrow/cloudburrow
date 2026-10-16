package components

import (
	"strings"
	"testing"
)

// Every image CloudBurrow pulls is pinned by digest (AGENTS.md: never pin a
// mutable tag). A tag alone can be moved to different bytes; #487 closed the
// last gap. The storage server's image is not pulled: it is built locally
// from the CLI's embedded binary on a base pinned by digest
// (internal/storageimage, #514), and neither is the BigQuery emulator's
// (internal/bigqueryimage, #1061).
func TestComponentImagesArePinnedByDigest(t *testing.T) {
	for name, image := range map[string]string{
		"PubSubImage": PubSubImage, "SpannerImage": SpannerImage,
		"CloudSQLImage": CloudSQLImage, "MemorystoreImage": MemorystoreImage,
		"CloudSQLMySQLImage":      CloudSQLMySQLImage,
		"MemorystoreRedis62Image": MemorystoreRedis62Image, "MemorystoreRedis70Image": MemorystoreRedis70Image,
		"CloudSQLPostgres13Image": CloudSQLPostgres13Image, "CloudSQLPostgres14Image": CloudSQLPostgres14Image,
		"CloudSQLPostgres15Image": CloudSQLPostgres15Image, "CloudSQLPostgres16Image": CloudSQLPostgres16Image,
		"CloudSQLPostgres18Image": CloudSQLPostgres18Image, "CloudSQLMySQL80Image": CloudSQLMySQL80Image,
	} {
		_, digest, ok := strings.Cut(image, "@sha256:")
		if !ok || len(digest) != 64 {
			t.Errorf("%s = %q is not pinned by a sha256 digest", name, image)
		}
	}
}
