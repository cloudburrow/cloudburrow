// Package storageimagetest gives tests stand-ins for the storage-server
// binaries the CLI embeds (internal/storageimage), so a test behaves the
// same whether or not `make storage-binaries` has run.
package storageimagetest

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"testing"
	"testing/fstest"

	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// ELF is the smallest file storageimage accepts as a Linux executable for
// arch: an ELF header and nothing else.
func ELF(arch string) []byte {
	m := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	h := elf.Header64{
		Type: uint16(elf.ET_EXEC), Machine: uint16(m), Version: uint32(elf.EV_CURRENT),
		Ehsize: 64, Phentsize: 56, Shentsize: 64,
	}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, h)
	return b.Bytes()
}

// FS is the embedded directory as a build with files would have it: the
// README, and each named file with its content.
func FS(files map[string][]byte) fstest.MapFS {
	fsys := fstest.MapFS{"bin/README.md": {Data: []byte("build outputs\n")}}
	for name, data := range files {
		fsys["bin/"+name] = &fstest.MapFile{Data: data}
	}
	return fsys
}

// Present embeds a stand-in for every architecture until the test ends.
func Present(t testing.TB) {
	t.Helper()
	files := map[string][]byte{}
	for _, a := range storageimage.Arches {
		files["cloudburrow-storage-linux-"+a] = ELF(a)
	}
	Use(t, FS(files))
}

// Missing embeds what a plain `go build` does, the README alone, until the
// test ends.
func Missing(t testing.TB) {
	t.Helper()
	Use(t, FS(nil))
}

// Use makes storageimage read fsys until the test ends.
func Use(t testing.TB, fsys fstest.MapFS) {
	t.Helper()
	t.Cleanup(storageimage.UseBinaries(fsys))
}
