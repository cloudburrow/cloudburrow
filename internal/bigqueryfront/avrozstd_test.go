package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/linkedin/goavro/v2"
)

// TestZstandardAvroFilesAreRead (#1547): an object container file whose
// codec is zstandard, which goavro does not read, is read as the same file
// uncompressed; a null-codec file is read as it is.
func TestZstandardAvroFilesAreRead(t *testing.T) {
	var plain bytes.Buffer
	w, err := goavro.NewOCFWriter(goavro.OCFConfig{W: &plain, Schema: `{"type":"record","name":"R","fields":[{"name":"n","type":"long"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]any{map[string]any{"n": int64(1)}, map[string]any{"n": int64(2)}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append([]any{map[string]any{"n": int64(3)}}); err != nil {
		t.Fatal(err)
	}
	// The same file, its blocks zstandard-compressed.
	br := bufio.NewReader(bytes.NewReader(plain.Bytes()))
	br.Discard(4)
	meta, err := avroMetadata(br)
	if err != nil {
		t.Fatal(err)
	}
	var sync [16]byte
	io.ReadFull(br, sync[:])
	var z bytes.Buffer
	z.WriteString(avroMagic)
	avroLong(&z, int64(len(meta)))
	for _, kv := range meta {
		if kv[0] == "avro.codec" {
			kv[1] = "zstandard"
		}
		avroLong(&z, int64(len(kv[0])))
		z.WriteString(kv[0])
		avroLong(&z, int64(len(kv[1])))
		z.WriteString(kv[1])
	}
	avroLong(&z, 0)
	z.Write(sync[:])
	enc, _ := zstd.NewWriter(nil)
	for {
		count, err := binary.ReadVarint(br)
		if err == io.EOF {
			break
		}
		size, _ := binary.ReadVarint(br)
		block := make([]byte, size)
		io.ReadFull(br, block)
		io.ReadFull(br, make([]byte, 16))
		c := enc.EncodeAll(block, nil)
		avroLong(&z, count)
		avroLong(&z, int64(len(c)))
		z.Write(c)
		z.Write(sync[:])
	}
	if _, err := goavro.NewOCFReader(bytes.NewReader(z.Bytes())); err == nil {
		t.Fatal("goavro read a zstandard file: the test file is not one")
	}
	for name, data := range map[string][]byte{"zstandard": z.Bytes(), "null": plain.Bytes()} {
		ocf, err := avroReader(bytes.NewReader(data), "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []int64
		for ocf.Scan() {
			v, err := ocf.Read()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			got = append(got, v.(map[string]any)["n"].(int64))
		}
		if len(got) != 3 || got[0] != 1 || got[2] != 3 {
			t.Errorf("%s: read %v", name, got)
		}
	}
}
