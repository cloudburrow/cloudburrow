package components

import (
	"strings"
	"testing"
)

// Every image CloudBurrow pulls is pinned by digest (AGENTS.md: never pin a
// mutable tag). A tag alone can be moved to different bytes; #487 closed the
// last gap. The storage server's image is not pulled: it is built locally
// from the CLI's embedded binary on a base pinned by digest
// (internal/storageimage, #514).
func TestComponentImagesArePinnedByDigest(t *testing.T) {
	for name, image := range map[string]string{
		"PubSubImage": PubSubImage, "SpannerImage": SpannerImage,
		"CloudSQLImage": CloudSQLImage, "BigQueryImage": BigQueryImage, "MemorystoreImage": MemorystoreImage,
		"CloudSQLMySQLImage": CloudSQLMySQLImage,
	} {
		_, digest, ok := strings.Cut(image, "@sha256:")
		if !ok || len(digest) != 64 {
			t.Errorf("%s = %q is not pinned by a sha256 digest", name, image)
		}
	}
}
