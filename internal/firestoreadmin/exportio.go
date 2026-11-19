package firestoreadmin

// The pieces of a managed export the Datastore front's Datastore Admin API
// shares (#1884): the instance's Cloud Storage, over its JSON API, and
// the LevelDB log files an export's output files are.

import (
	"bytes"
	"context"
	"net/http"
)

// GCS is the instance's Cloud Storage at a base URL (http://storage:4443).
type GCS struct{ c *gcsClient }

// NewGCS is the Cloud Storage at base, called through client.
func NewGCS(base string, client *http.Client) *GCS {
	return &GCS{c: &gcsClient{base: base, http: client}}
}

// BucketExists refuses a missing bucket with Google's FAILED_PRECONDITION.
func (g *GCS) BucketExists(ctx context.Context, bucket string) error {
	return g.c.bucketExists(ctx, bucket)
}

// Put writes an object.
func (g *GCS) Put(ctx context.Context, bucket, name string, data []byte) error {
	return g.c.put(ctx, bucket, name, data)
}

// Get reads an object.
func (g *GCS) Get(ctx context.Context, bucket, name string) ([]byte, error) {
	return g.c.get(ctx, bucket, name)
}

// List is the names of the objects under prefix.
func (g *GCS) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	return g.c.list(ctx, bucket, prefix)
}

// SplitGS is a gs:// URI's bucket and object path, without a trailing /.
func SplitGS(uri string) (bucket, prefix string, err error) { return splitGS(uri) }

// EncodeLog is records as a LevelDB log file.
func EncodeLog(records [][]byte) []byte {
	var b bytes.Buffer
	w := &logWriter{w: &b}
	for _, r := range records {
		w.record(r)
	}
	return b.Bytes()
}

// DecodeLog is the records of a LevelDB log file.
func DecodeLog(b []byte) ([][]byte, error) { return readLog(b) }
