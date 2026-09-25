package components

import (
	"strings"
	"testing"
)

// Every image CloudBurrow deploys is pinned by digest (AGENTS.md: never pin a
// mutable tag). A tag alone can be moved to different bytes; #487 closed the
// last gap, fake-gcs-server.
func TestComponentImagesArePinnedByDigest(t *testing.T) {
	for name, image := range map[string]string{
		"PubSubImage": PubSubImage, "StorageImage": StorageImage, "SpannerImage": SpannerImage,
		"CloudSQLImage": CloudSQLImage, "BigQueryImage": BigQueryImage, "MemorystoreImage": MemorystoreImage,
		"CloudSQLMySQLImage": CloudSQLMySQLImage,
	} {
		_, digest, ok := strings.Cut(image, "@sha256:")
		if !ok || len(digest) != 64 {
			t.Errorf("%s = %q is not pinned by a sha256 digest", name, image)
		}
	}
}
