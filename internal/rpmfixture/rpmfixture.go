// Package rpmfixture builds small .rpm packages for tests (#1539): a
// version 3 lead, a signature header with the header's SHA-1 and SHA-256
// and the payload's sizes, and a header with an immutable region, the
// file list (with SHA-256 file digests), dependencies and changelog, over
// a gzip-compressed cpio (newc) payload with its SHA-256 payload digest.
// rpm and dnf install what it builds, with gpgcheck=0: no rpmbuild
// needed.
package rpmfixture

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
)

// File is one file the package installs.
type File struct {
	Path string // absolute
	Body []byte
	Mode uint16 // permission bits; 0644 when zero
	Dir  bool
}

// Dep is one provides, requires, conflicts or obsoletes entry. Flags is
// "", EQ, LT, GT, LE or GE; EVR is [epoch:]version[-release].
type Dep struct {
	Name, Flags, EVR string
}

// Change is one changelog entry.
type Change struct {
	Time         int64
	Author, Text string
}

// Package is what Build writes.
type Package struct {
	Name, Version, Release, Arch string
	// Epoch is written when non-nil.
	Epoch                                               *int32
	Summary, Description, License, URL, Group, Packager string
	Vendor, BuildHost                                   string
	BuildTime                                           int64
	Files                                               []File
	Provides, Requires, Conflicts, Obsoletes            []Dep
	Changelog                                           []Change
	Source                                              bool // a source package: no SOURCERPM tag
}

const (
	typInt16       = 3
	typInt32       = 4
	typString      = 6
	typBin         = 7
	typStringArray = 8
	typI18N        = 9
)

type entry struct {
	tag, typ uint32
	count    int
	data     []byte
}

func int32s(v ...int64) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint32(b[4*i:], uint32(x))
	}
	return b
}

func int16s(v ...uint16) []byte {
	b := make([]byte, 2*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint16(b[2*i:], x)
	}
	return b
}

func strs(v ...string) []byte {
	var b bytes.Buffer
	for _, s := range v {
		b.WriteString(s)
		b.WriteByte(0)
	}
	return b.Bytes()
}

type header struct{ entries []entry }

func (h *header) add(tag, typ uint32, count int, data []byte) {
	h.entries = append(h.entries, entry{tag, typ, count, data})
}
func (h *header) str(tag uint32, s string)         { h.add(tag, typString, 1, strs(s)) }
func (h *header) i18n(tag uint32, s string)        { h.add(tag, typI18N, 1, strs(s)) }
func (h *header) strArray(tag uint32, v []string)  { h.add(tag, typStringArray, len(v), strs(v...)) }
func (h *header) int32Array(tag uint32, v []int64) { h.add(tag, typInt32, len(v), int32s(v...)) }

// bytes writes the header with its region tag: the region entry first,
// every entry's data in index order and aligned to its type, then the
// region trailer.
func (h *header) bytes(region uint32) []byte {
	sort.Slice(h.entries, func(i, j int) bool { return h.entries[i].tag < h.entries[j].tag })
	var index, store bytes.Buffer
	il := len(h.entries) + 1
	for _, e := range h.entries {
		align := map[uint32]int{typInt16: 2, typInt32: 4}[e.typ]
		for align > 0 && store.Len()%align != 0 {
			store.WriteByte(0)
		}
		_ = binary.Write(&index, binary.BigEndian, []uint32{e.tag, e.typ, uint32(store.Len()), uint32(e.count)})
		store.Write(e.data)
	}
	trailerAt := store.Len()
	_ = binary.Write(&store, binary.BigEndian, []uint32{region, typBin, uint32(int32(-16 * il)), 16})
	var out bytes.Buffer
	out.Write([]byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0})
	_ = binary.Write(&out, binary.BigEndian, []uint32{uint32(il), uint32(store.Len())})
	_ = binary.Write(&out, binary.BigEndian, []uint32{region, typBin, uint32(trailerAt), 16})
	out.Write(index.Bytes())
	out.Write(store.Bytes())
	return out.Bytes()
}

var flagBits = map[string]int64{"": 0, "EQ": 8, "LT": 2, "GT": 4, "LE": 10, "GE": 12}

func (h *header) deps(nameTag, flagsTag, verTag uint32, deps []Dep, extra int64) {
	if len(deps) == 0 {
		return
	}
	var names, vers []string
	var flags []int64
	for _, d := range deps {
		names = append(names, d.Name)
		vers = append(vers, d.EVR)
		flags = append(flags, flagBits[d.Flags]|extra)
	}
	h.strArray(nameTag, names)
	h.int32Array(flagsTag, flags)
	h.strArray(verTag, vers)
}

// cpio writes a newc archive of the files, named ./path as rpm's are.
func cpio(files []File) []byte {
	var b bytes.Buffer
	write := func(ino int, mode uint32, name string, body []byte) {
		fmt.Fprintf(&b, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			ino, mode, 0, 0, 1, 0, len(body), 0, 0, 0, 0, len(name)+1, 0)
		b.WriteString(name)
		b.WriteByte(0)
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
		b.Write(body)
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
	}
	for i, f := range files {
		write(i+1, uint32(fileMode(f)), "."+f.Path, f.Body)
	}
	write(0, 0, "TRAILER!!!", nil)
	return b.Bytes()
}

func fileMode(f File) uint16 {
	perm := f.Mode
	if perm == 0 {
		perm = 0o644
		if f.Dir {
			perm = 0o755
		}
	}
	if f.Dir {
		return 0o040000 | perm
	}
	return 0o100000 | perm
}

// Build writes the package.
func Build(p Package) []byte {
	files := append([]File(nil), p.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	payload := cpio(files)
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	pd := sha256.Sum256(gz.Bytes())

	h := &header{}
	h.strArray(100, []string{"C"}) // HEADERI18NTABLE
	h.str(1000, p.Name)
	h.str(1001, p.Version)
	h.str(1002, p.Release)
	if p.Epoch != nil {
		h.int32Array(1003, []int64{int64(*p.Epoch)})
	}
	h.i18n(1004, p.Summary)
	h.i18n(1005, p.Description)
	h.int32Array(1006, []int64{p.BuildTime})
	h.str(1007, or(p.BuildHost, "localhost"))
	var size int64
	for _, f := range files {
		size += int64(len(f.Body))
	}
	h.int32Array(1009, []int64{size})
	if p.Vendor != "" {
		h.str(1011, p.Vendor)
	}
	h.str(1014, or(p.License, "MIT"))
	if p.Packager != "" {
		h.str(1015, p.Packager)
	}
	h.i18n(1016, or(p.Group, "Unspecified"))
	if p.URL != "" {
		h.str(1020, p.URL)
	}
	h.str(1021, "linux")
	h.str(1022, p.Arch)
	if len(files) > 0 {
		var sizes, mtimes, fflags, devs, inodes, dirIdx []int64
		var modes, rdevs []uint16
		var digests, links, users, groups, langs, bases, dirs []string
		dirOf := map[string]int{}
		for i, f := range files {
			sizes = append(sizes, int64(len(f.Body)))
			modes = append(modes, fileMode(f))
			rdevs = append(rdevs, 0)
			mtimes = append(mtimes, p.BuildTime)
			d := ""
			if !f.Dir {
				s := sha256.Sum256(f.Body)
				d = hex.EncodeToString(s[:])
			}
			digests = append(digests, d)
			links = append(links, "")
			fflags = append(fflags, 0)
			users = append(users, "root")
			groups = append(groups, "root")
			devs = append(devs, 1)
			inodes = append(inodes, int64(i+1))
			langs = append(langs, "")
			dir, base := path.Split(f.Path)
			if _, ok := dirOf[dir]; !ok {
				dirOf[dir] = len(dirs)
				dirs = append(dirs, dir)
			}
			dirIdx = append(dirIdx, int64(dirOf[dir]))
			bases = append(bases, base)
		}
		h.int32Array(1028, sizes)
		h.add(1030, typInt16, len(modes), int16s(modes...))
		h.add(1033, typInt16, len(rdevs), int16s(rdevs...))
		h.int32Array(1034, mtimes)
		h.strArray(1035, digests)
		h.strArray(1036, links)
		h.int32Array(1037, fflags)
		h.strArray(1039, users)
		h.strArray(1040, groups)
		h.int32Array(1095, devs)
		h.int32Array(1096, inodes)
		h.strArray(1097, langs)
		h.int32Array(1116, dirIdx)
		h.strArray(1117, bases)
		h.strArray(1118, dirs)
		h.int32Array(5011, []int64{8}) // FILEDIGESTALGO: SHA-256
	}
	if !p.Source {
		h.str(1044, p.Name+"-"+p.Version+"-"+p.Release+".src.rpm")
	}
	h.deps(1047, 1112, 1113, p.Provides, 0)
	rpmlib := []Dep{
		{"rpmlib(CompressedFileNames)", "LE", "3.0.4-1"},
		{"rpmlib(FileDigests)", "LE", "4.6.0-1"},
		{"rpmlib(PayloadFilesHavePrefix)", "LE", "4.0-1"},
	}
	reqs := append(append([]Dep(nil), p.Requires...), rpmlib...)
	var rnames, rvers []string
	var rflags []int64
	for i, d := range reqs {
		rnames = append(rnames, d.Name)
		rvers = append(rvers, d.EVR)
		f := flagBits[d.Flags]
		if i >= len(p.Requires) {
			f |= 1 << 24 // RPMSENSE_RPMLIB
		}
		rflags = append(rflags, f)
	}
	h.int32Array(1048, rflags)
	h.strArray(1049, rnames)
	h.strArray(1050, rvers)
	h.deps(1054, 1053, 1055, p.Conflicts, 0)
	h.str(1064, "4.19.1") // RPMVERSION
	if len(p.Changelog) > 0 {
		var times []int64
		var names, texts []string
		for _, c := range p.Changelog {
			times = append(times, c.Time)
			names = append(names, c.Author)
			texts = append(texts, c.Text)
		}
		h.int32Array(1080, times)
		h.strArray(1081, names)
		h.strArray(1082, texts)
	}
	h.deps(1090, 1114, 1115, p.Obsoletes, 0)
	h.str(1124, "cpio")
	h.str(1125, "gzip")
	h.str(1126, "9")
	h.strArray(5092, []string{hex.EncodeToString(pd[:])}) // PAYLOADDIGEST
	h.int32Array(5093, []int64{8})                        // PAYLOADDIGESTALGO: SHA-256
	h.str(5062, "utf-8")                                  // ENCODING
	hb := h.bytes(63)                                     // HEADERIMMUTABLE

	s1 := sha1.Sum(hb)
	s256 := sha256.Sum256(hb)
	sig := &header{}
	sig.str(269, hex.EncodeToString(s1[:]))
	sig.str(273, hex.EncodeToString(s256[:]))
	sig.int32Array(1000, []int64{int64(len(hb) + gz.Len())}) // header and payload
	sig.int32Array(1007, []int64{int64(len(payload))})       // uncompressed payload
	sb := sig.bytes(62)                                      // HEADERSIGNATURES

	var out bytes.Buffer
	lead := make([]byte, 96)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb, 3, 0})
	if p.Source {
		binary.BigEndian.PutUint16(lead[6:], 1)
	}
	binary.BigEndian.PutUint16(lead[8:], 1)
	copy(lead[10:75], p.Name+"-"+p.Version+"-"+p.Release)
	binary.BigEndian.PutUint16(lead[76:], 1)
	binary.BigEndian.PutUint16(lead[78:], 5)
	out.Write(lead)
	out.Write(sb)
	for out.Len()%8 != 0 {
		out.WriteByte(0)
	}
	out.Write(hb)
	out.Write(gz.Bytes())
	return out.Bytes()
}

// FileName is the package's conventional file name.
func FileName(p Package) string {
	arch := p.Arch
	if p.Source {
		arch = "src"
	}
	return strings.Join([]string{p.Name, p.Version, p.Release}, "-") + "." + arch + ".rpm"
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
