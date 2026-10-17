//go:build compat

package compat

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"cloud.google.com/go/bigquery"
	"github.com/klauspost/compress/zstd"
)

// zstdAvro is data, an Avro object container file with codec null, with
// its blocks compressed by zstandard and its codec "zstandard", which the
// Avro writer in go.mod (goavro) does not write.
func zstdAvro(t *testing.T, data string) string {
	t.Helper()
	br := bufio.NewReader(bytes.NewReader([]byte(data)))
	var out bytes.Buffer
	long := func(n int64) {
		var b [binary.MaxVarintLen64]byte
		out.Write(b[:binary.PutVarint(b[:], n)])
	}
	read := func() int64 {
		n, err := binary.ReadVarint(br)
		if err != nil {
			t.Fatalf("avro: %v", err)
		}
		return n
	}
	str := func() string {
		b := make([]byte, read())
		if _, err := io.ReadFull(br, b); err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	magic := make([]byte, 4)
	io.ReadFull(br, magic)
	out.Write(magic)
	for n := read(); n != 0; n = read() {
		long(n)
		for ; n > 0; n-- {
			k, v := str(), str()
			if k == "avro.codec" {
				v = "zstandard"
			}
			long(int64(len(k)))
			out.WriteString(k)
			long(int64(len(v)))
			out.WriteString(v)
		}
	}
	long(0)
	sync := make([]byte, 16)
	io.ReadFull(br, sync)
	out.Write(sync)
	enc, _ := zstd.NewWriter(nil)
	for {
		count, err := binary.ReadVarint(br)
		if err == io.EOF {
			break
		}
		block := make([]byte, read())
		io.ReadFull(br, block)
		io.ReadFull(br, make([]byte, 16))
		c := enc.EncodeAll(block, nil)
		long(count)
		long(int64(len(c)))
		out.Write(c)
		out.Write(sync)
	}
	return out.String()
}

// TestBigQueryAvroZstandardLoad (#1547): an Avro file compressed with
// zstandard loads from Cloud Storage, as BigQuery "supports the following
// compression codecs for Avro file contents: Snappy, DEFLATE, ZSTD"
// (Loading Avro data); it was 501, the Avro decoder CloudBurrow uses
// reading null, deflate and snappy only.
//
// covers: bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryAvroZstandardLoad(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := loadBucket(t, h, "bq-avro-zstd", map[string]string{
		"z.avro": zstdAvro(t, avroFile(t, `{"type":"record","name":"R","fields":[{"name":"n","type":"long"},{"name":"s","type":"string"}]}`, "",
			map[string]any{"n": int64(1), "s": "a"}, map[string]any{"n": int64(2), "s": "b"})),
	})
	g := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/z.avro")
	g.SourceFormat = bigquery.Avro
	if err := runLoad(ctx, ds.Table("zstd").LoaderFrom(g)); err != nil {
		t.Fatalf("a zstandard Avro load: %v", err)
	}
	if got := fmt.Sprint(rows(t, ctx, c, "SELECT n, s FROM "+ds.DatasetID+".zstd ORDER BY n")); got != "[[1 a] [2 b]]" {
		t.Errorf("the zstandard file loaded %s", got)
	}
}
