package bigqueryfront

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Avro object container files compressed with zstandard (#1547).
//
// BigQuery "supports the following compression codecs for Avro file
// contents: Snappy, DEFLATE, ZSTD" (Loading Avro data,
// https://cloud.google.com/bigquery/docs/loading-data-cloud-storage-avro,
// read 2026-10-17). The Avro decoder the front reads files with, goavro
// v2.15.0, reads null, deflate and snappy only, so a file whose avro.codec
// is "zstandard" (the Avro specification's name for it) is read through
// zstdOCF: its blocks are decompressed with the zstandard decoder of
// github.com/klauspost/compress, already in go.mod, and passed on as the
// same file with codec null, which goavro reads.

const avroMagic = "Obj\x01"

// zstdOCF returns rd's data as an object container file goavro reads: rd
// as it is unless its codec is zstandard (above), and then the same file
// uncompressed.
func zstdOCF(rd io.Reader) (io.Reader, error) {
	br := bufio.NewReader(rd)
	head, err := br.Peek(4)
	if err != nil || string(head) != avroMagic {
		return br, nil // goavro says what is wrong
	}
	if _, err := br.Discard(4); err != nil {
		return nil, err
	}
	meta, err := avroMetadata(br)
	if err != nil {
		return nil, err
	}
	var sync [16]byte
	if _, err := io.ReadFull(br, sync[:]); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	writeHeader := func() {
		out.WriteString(avroMagic)
		avroLong(&out, int64(len(meta)))
		for _, kv := range meta {
			avroLong(&out, int64(len(kv[0])))
			out.WriteString(kv[0])
			avroLong(&out, int64(len(kv[1])))
			out.WriteString(kv[1])
		}
		avroLong(&out, 0)
		out.Write(sync[:])
	}
	zst := false
	for i, kv := range meta {
		if kv[0] == "avro.codec" && kv[1] == "zstandard" {
			zst, meta[i][1] = true, "null"
		}
	}
	writeHeader()
	if !zst {
		return io.MultiReader(bytes.NewReader(out.Bytes()), br), nil
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	for {
		count, err := binary.ReadVarint(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		size, err := binary.ReadVarint(br)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("a block of the file is cut short")
		}
		block := make([]byte, size)
		if _, err := io.ReadFull(br, block); err != nil {
			return nil, fmt.Errorf("a block of the file is cut short: %w", err)
		}
		data, err := dec.DecodeAll(block, nil)
		if err != nil {
			return nil, fmt.Errorf("a zstandard block could not be read: %w", err)
		}
		var bsync [16]byte
		if _, err := io.ReadFull(br, bsync[:]); err != nil || bsync != sync {
			return nil, errors.New("a block of the file does not end with the file's sync marker")
		}
		avroLong(&out, count)
		avroLong(&out, int64(len(data)))
		out.Write(data)
		out.Write(sync[:])
	}
	return &out, nil
}

// avroMetadata reads an object container file's metadata map.
func avroMetadata(br *bufio.Reader) ([][2]string, error) {
	var out [][2]string
	str := func() (string, error) {
		n, err := binary.ReadVarint(br)
		if err != nil || n < 0 || n > 1<<26 {
			return "", errors.New("the file's header is not an Avro object container file's")
		}
		b := make([]byte, n)
		_, err = io.ReadFull(br, b)
		return string(b), err
	}
	for {
		n, err := binary.ReadVarint(br)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out, nil
		}
		if n < 0 {
			n = -n
			if _, err := binary.ReadVarint(br); err != nil { // the block's size
				return nil, err
			}
		}
		for ; n > 0; n-- {
			k, err := str()
			if err != nil {
				return nil, err
			}
			v, err := str()
			if err != nil {
				return nil, err
			}
			out = append(out, [2]string{k, v})
		}
	}
}

// avroLong writes n as Avro writes a long (zigzag varint).
func avroLong(w *bytes.Buffer, n int64) {
	var b [binary.MaxVarintLen64]byte
	w.Write(b[:binary.PutVarint(b[:], n)])
}
