package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

func TestPartitionOffset(t *testing.T) {
	// MBR: a swap partition and the Linux one at sector 2048.
	mbr := make([]byte, rawHeadBytes)
	mbr[510], mbr[511] = 0x55, 0xaa
	mbr[446+4] = 0x82
	binary.LittleEndian.PutUint32(mbr[446+8:], 63)
	binary.LittleEndian.PutUint32(mbr[446+12:], 100)
	mbr[462+4] = 0x83
	binary.LittleEndian.PutUint32(mbr[462+8:], 2048)
	binary.LittleEndian.PutUint32(mbr[462+12:], 20000)
	if off, err := partitionOffset(mbr); err != nil || off != 2048*512 {
		t.Fatalf("MBR: %d %v", off, err)
	}
	// GPT: an EFI system partition, then the x86-64 root at LBA 262144.
	gpt := make([]byte, rawHeadBytes)
	gpt[510], gpt[511] = 0x55, 0xaa
	gpt[446+4] = 0xee
	copy(gpt[512:], "EFI PART")
	binary.LittleEndian.PutUint64(gpt[512+72:], 2)
	binary.LittleEndian.PutUint32(gpt[512+80:], 128)
	binary.LittleEndian.PutUint32(gpt[512+84:], 128)
	entry := func(i int, guid []byte, first, last uint64) {
		e := gpt[1024+128*i:]
		copy(e, guid)
		binary.LittleEndian.PutUint64(e[32:], first)
		binary.LittleEndian.PutUint64(e[40:], last)
	}
	esp := []byte{0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, 0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b}
	root := []byte{0xe3, 0xbc, 0x68, 0x4f, 0xcd, 0xe8, 0xb1, 0x4d, 0x96, 0xe7, 0xfb, 0xca, 0xf9, 0x84, 0xb7, 0x09}
	entry(0, esp, 2048, 262143)
	entry(1, root, 262144, 20971486)
	if off, err := partitionOffset(gpt); err != nil || off != 262144*512 {
		t.Fatalf("GPT: %d %v", off, err)
	}
	// A whole-disk ext4 file system.
	ext := make([]byte, rawHeadBytes)
	binary.LittleEndian.PutUint16(ext[1024+56:], 0xef53)
	if off, err := partitionOffset(ext); err != nil || off != 0 {
		t.Fatalf("ext4: %d %v", off, err)
	}
	if _, err := partitionOffset(make([]byte, rawHeadBytes)); err == nil {
		t.Fatal("a blank disk has no Linux partition")
	}
	entry(1, esp, 262144, 20971486)
	if _, err := partitionOffset(gpt); err == nil {
		t.Fatal("a GPT with no Linux partition")
	}
}

// importConsoleVMs is consoleFakeVMs that imports a disk.raw.
type importConsoleVMs struct{ consoleFakeVMs }

func (importConsoleVMs) ImportRaw(context.Context, string, int64, func(context.Context) (io.ReadCloser, error)) (string, error) {
	return "ID=debian\n", nil
}

func TestConsoleCreateImageFromCloudStorageFile(t *testing.T) {
	ctx := context.Background()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "disk.raw", Mode: 0o644, Size: 1024, Typeflag: tar.TypeReg})
	_, _ = tw.Write(make([]byte, 1024))
	_ = tw.Close()
	_ = zw.Close()
	api, err := compute.New(compute.Options{VMs: importConsoleVMs{}, ReadObject: func(_ context.Context, bucket, object string) (io.ReadCloser, int64, error) {
		if bucket != "b" || object != "disk.tar.gz" {
			return nil, 0, compute.ErrObjectNotFound
		}
		return io.NopCloser(bytes.NewReader(b.Bytes())), int64(b.Len()), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	images := computeImagesProvider{computeScreen{svc: &computeService{api: api}}}
	fields := images.CreateFormFor(ctx, "p")
	src := vpcField(fields, "source")
	for _, d := range src.DisabledOptions {
		if d == "Cloud Storage file" {
			t.Fatal("Cloud Storage file is still drawn unavailable")
		}
	}
	f := vpcField(fields, "rawSource")
	if f == nil || f.Browse != "storage" || f.VisibleWhen["source"] != "Cloud Storage file" || !f.Required {
		t.Fatalf("Cloud Storage file field = %+v", f)
	}
	if _, err := images.Create(ctx, "p", map[string]string{"name": "imported", "source": "Cloud Storage file", "rawSource": "gs://b/disk.tar.gz", "locationName": "us"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		im, err := images.call("GET", "p", "global/images/imported", nil, nil)
		if err == nil && vpcStr(im, "status") == "READY" {
			if vpcStr(vmMap(im, "rawDisk"), "source") != "gs://b/disk.tar.gz" || vpcStr(im, "sourceType") != "RAW" {
				t.Fatalf("image = %v", im)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the import did not finish: %v %v", im, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := images.Create(ctx, "p", map[string]string{"name": "empty", "source": "Cloud Storage file"}); err == nil {
		t.Fatal("an import with no file")
	}
}
