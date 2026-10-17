package run

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
)

// An .rpm's header (#1531, #1539), read in Go: the 96-byte lead, the
// signature header (padded to 8 bytes), then the header whose tags
// describe the package. Each header is a magic, an index of
// (tag, type, offset, count) entries and a data store.

// RPM header data types.
const (
	rpmInt8        = 2
	rpmInt16       = 3
	rpmInt32       = 4
	rpmInt64       = 5
	rpmString      = 6
	rpmBin         = 7
	rpmStringArray = 8
	rpmI18NString  = 9
)

// RPM header tags the import and the repository metadata read.
const (
	tagName           = 1000
	tagVersion        = 1001
	tagRelease        = 1002
	tagEpoch          = 1003
	tagSummary        = 1004
	tagDescription    = 1005
	tagBuildTime      = 1006
	tagBuildHost      = 1007
	tagSize           = 1009
	tagVendor         = 1011
	tagLicense        = 1014
	tagPackager       = 1015
	tagGroup          = 1016
	tagURL            = 1020
	tagArch           = 1022
	tagOldFilenames   = 1027
	tagFileModes      = 1030
	tagFileFlags      = 1037
	tagSourceRPM      = 1044
	tagArchiveSize    = 1046
	tagProvideName    = 1047
	tagRequireFlags   = 1048
	tagRequireName    = 1049
	tagRequireVersion = 1050
	tagConflictFlags  = 1053
	tagConflictName   = 1054
	tagConflictVer    = 1055
	tagChangelogTime  = 1080
	tagChangelogName  = 1081
	tagChangelogText  = 1082
	tagObsoleteName   = 1090
	tagProvideFlags   = 1112
	tagProvideVersion = 1113
	tagObsoleteFlags  = 1114
	tagObsoleteVer    = 1115
	tagDirIndexes     = 1116
	tagBaseNames      = 1117
	tagDirNames       = 1118
	tagLongSize       = 5009

	sigTagPayloadSize = 1007 // the signature header's uncompressed payload size
)

// rpmEntry is one tag's raw value.
type rpmEntry struct {
	typ   uint32
	count int
	data  []byte // from the entry's offset to the end of the store
}

// rpmHeaderTags is one header's tags.
type rpmHeaderTags map[int32]rpmEntry

func (h rpmHeaderTags) strs(tag int32) []string {
	e, ok := h[tag]
	if !ok {
		return nil
	}
	switch e.typ {
	case rpmString, rpmI18NString, rpmStringArray:
	default:
		return nil
	}
	n := e.count
	if e.typ != rpmStringArray {
		n = 1 // an I18NSTRING's count is its translations; the first is C
	}
	out := make([]string, 0, n)
	d := e.data
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(d, 0)
		if j < 0 {
			break
		}
		out = append(out, string(d[:j]))
		d = d[j+1:]
	}
	return out
}

func (h rpmHeaderTags) str(tag int32) string {
	if s := h.strs(tag); len(s) > 0 {
		return s[0]
	}
	return ""
}

func (h rpmHeaderTags) ints(tag int32) []int64 {
	e, ok := h[tag]
	if !ok {
		return nil
	}
	size := map[uint32]int{rpmInt8: 1, rpmInt16: 2, rpmInt32: 4, rpmInt64: 8}[e.typ]
	if size == 0 || e.count*size > len(e.data) {
		return nil
	}
	out := make([]int64, e.count)
	for i := range out {
		b := e.data[i*size:]
		switch size {
		case 1:
			out[i] = int64(b[0])
		case 2:
			out[i] = int64(binary.BigEndian.Uint16(b))
		case 4:
			out[i] = int64(binary.BigEndian.Uint32(b))
		case 8:
			out[i] = int64(binary.BigEndian.Uint64(b))
		}
	}
	return out
}

func (h rpmHeaderTags) int(tag int32) (int64, bool) {
	if v := h.ints(tag); len(v) > 0 {
		return v[0], true
	}
	return 0, false
}

// readRPMHeader reads the header at off, returning its tags and where it
// ends.
func readRPMHeader(b []byte, off int) (rpmHeaderTags, int, error) {
	if off < 0 || off+16 > len(b) || !bytes.Equal(b[off:off+3], []byte{0x8e, 0xad, 0xe8}) {
		return nil, 0, errors.New("not an RPM package: bad header")
	}
	n := int(binary.BigEndian.Uint32(b[off+8:]))
	size := int(binary.BigEndian.Uint32(b[off+12:]))
	idx := off + 16
	if n < 0 || size < 0 || n > 1<<16 || size > len(b) {
		return nil, 0, errors.New("not an RPM package: truncated header")
	}
	store := idx + 16*n
	if store+size > len(b) {
		return nil, 0, errors.New("not an RPM package: truncated header")
	}
	data := b[store : store+size]
	out := rpmHeaderTags{}
	for i := 0; i < n; i++ {
		e := b[idx+16*i:]
		tag := int32(binary.BigEndian.Uint32(e))
		typ := binary.BigEndian.Uint32(e[4:])
		o := int(int32(binary.BigEndian.Uint32(e[8:])))
		count := int(binary.BigEndian.Uint32(e[12:]))
		if o < 0 || o >= size || count < 0 {
			continue
		}
		out[tag] = rpmEntry{typ: typ, count: count, data: data[o:]}
	}
	return out, store + size, nil
}

// rpmHeaders reads an .rpm's signature and package headers, and the byte
// range of the package header (createrepo's header-range).
func rpmHeaders(b []byte) (sig, h rpmHeaderTags, start, end int, err error) {
	if len(b) < 96 || !bytes.Equal(b[:4], []byte{0xed, 0xab, 0xee, 0xdb}) {
		return nil, nil, 0, 0, errors.New("not an RPM package: no lead")
	}
	sig, start, err = readRPMHeader(b, 96)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if pad := start % 8; pad != 0 {
		start += 8 - pad
	}
	h, end, err = readRPMHeader(b, start)
	return sig, h, start, end, err
}

// rpmDep is one provides, requires, conflicts or obsoletes entry.
type rpmDep struct {
	Name    string `json:"name"`
	Flags   string `json:"flags,omitempty"` // EQ, LT, GT, LE or GE
	Epoch   string `json:"epoch,omitempty"`
	Ver     string `json:"ver,omitempty"`
	Rel     string `json:"rel,omitempty"`
	Pre     bool   `json:"pre,omitempty"`
	rawFlag int64  `json:"-"`
}

// rpmFile is one file the package installs.
type rpmFile struct {
	Path  string `json:"path"`
	Dir   bool   `json:"dir,omitempty"`
	Ghost bool   `json:"ghost,omitempty"`
}

// rpmChange is one changelog entry.
type rpmChange struct {
	Author string `json:"author"`
	Date   int64  `json:"date"`
	Text   string `json:"text"`
}

// rpmInfo is what the repository metadata says of a package, read from
// its header.
type rpmInfo struct {
	Name        string      `json:"name"`
	Epoch       string      `json:"epoch"`
	Version     string      `json:"version"`
	Release     string      `json:"release"`
	Arch        string      `json:"arch"`
	Summary     string      `json:"summary,omitempty"`
	Description string      `json:"description,omitempty"`
	Packager    string      `json:"packager,omitempty"`
	URL         string      `json:"url,omitempty"`
	License     string      `json:"license,omitempty"`
	Vendor      string      `json:"vendor,omitempty"`
	Group       string      `json:"group,omitempty"`
	BuildHost   string      `json:"buildHost,omitempty"`
	SourceRPM   string      `json:"sourceRpm,omitempty"`
	BuildTime   int64       `json:"buildTime,omitempty"`
	Installed   int64       `json:"installed,omitempty"`
	Archive     int64       `json:"archive,omitempty"`
	HeaderStart int         `json:"headerStart"`
	HeaderEnd   int         `json:"headerEnd"`
	Provides    []rpmDep    `json:"provides,omitempty"`
	Requires    []rpmDep    `json:"requires,omitempty"`
	Conflicts   []rpmDep    `json:"conflicts,omitempty"`
	Obsoletes   []rpmDep    `json:"obsoletes,omitempty"`
	Files       []rpmFile   `json:"files,omitempty"`
	Changelog   []rpmChange `json:"changelog,omitempty"`
}

// RPMSENSE flags.
const (
	senseLess       = 1 << 1
	senseGreater    = 1 << 2
	senseEqual      = 1 << 3
	sensePrereq     = 1 << 6
	senseScriptPre  = 1 << 9
	senseScriptPost = 1 << 10
	senseRPMLib     = 1 << 24
)

func rpmFlagName(f int64) string {
	switch f & (senseLess | senseGreater | senseEqual) {
	case senseEqual:
		return "EQ"
	case senseLess:
		return "LT"
	case senseGreater:
		return "GT"
	case senseLess | senseEqual:
		return "LE"
	case senseGreater | senseEqual:
		return "GE"
	}
	return ""
}

// splitEVR splits [epoch:]version[-release].
func splitEVR(evr string) (epoch, ver, rel string) {
	if i := strings.Index(evr, ":"); i >= 0 {
		if _, err := strconv.Atoi(evr[:i]); err == nil {
			epoch, evr = evr[:i], evr[i+1:]
		}
	}
	ver = evr
	if i := strings.LastIndex(evr, "-"); i >= 0 {
		ver, rel = evr[:i], evr[i+1:]
	}
	if epoch == "" && ver != "" {
		epoch = "0"
	}
	return epoch, ver, rel
}

func rpmDeps(h rpmHeaderTags, nameTag, flagsTag, verTag int32) []rpmDep {
	names := h.strs(nameTag)
	flags := h.ints(flagsTag)
	vers := h.strs(verTag)
	var out []rpmDep
	for i, n := range names {
		d := rpmDep{Name: n}
		if i < len(flags) {
			d.rawFlag = flags[i]
			d.Flags = rpmFlagName(flags[i])
			d.Pre = flags[i]&(sensePrereq|senseScriptPre|senseScriptPost) != 0
		}
		if i < len(vers) && vers[i] != "" && d.Flags != "" {
			d.Epoch, d.Ver, d.Rel = splitEVR(vers[i])
		}
		out = append(out, d)
	}
	return out
}

// parseRPM reads an .rpm's header into what the repository metadata
// lists.
func parseRPM(b []byte) (*rpmInfo, error) {
	sig, h, start, end, err := rpmHeaders(b)
	if err != nil {
		return nil, err
	}
	p := &rpmInfo{
		Name: h.str(tagName), Version: h.str(tagVersion), Release: h.str(tagRelease), Arch: h.str(tagArch),
		Summary: h.str(tagSummary), Description: h.str(tagDescription), Packager: h.str(tagPackager),
		URL: h.str(tagURL), License: h.str(tagLicense), Vendor: h.str(tagVendor), Group: h.str(tagGroup),
		BuildHost: h.str(tagBuildHost), SourceRPM: h.str(tagSourceRPM), HeaderStart: start, HeaderEnd: end,
		Epoch: "0",
	}
	if p.Name == "" || p.Version == "" || p.Arch == "" {
		return nil, errors.New("the RPM header has no name, version or architecture")
	}
	if p.SourceRPM == "" {
		p.Arch = "src" // a source package's arch in the metadata, as createrepo's
	}
	if e, ok := h.int(tagEpoch); ok {
		p.Epoch = strconv.FormatInt(e, 10)
	}
	p.BuildTime, _ = h.int(tagBuildTime)
	if v, ok := h.int(tagLongSize); ok {
		p.Installed = v
	} else {
		p.Installed, _ = h.int(tagSize)
	}
	if v, ok := h.int(tagArchiveSize); ok {
		p.Archive = v
	} else {
		p.Archive, _ = sig.int(sigTagPayloadSize)
	}

	p.Provides = rpmDeps(h, tagProvideName, tagProvideFlags, tagProvideVersion)
	p.Conflicts = rpmDeps(h, tagConflictName, tagConflictFlags, tagConflictVer)
	p.Obsoletes = rpmDeps(h, tagObsoleteName, tagObsoleteFlags, tagObsoleteVer)

	// Files: BASENAMES under DIRNAMES by DIRINDEXES, or the older
	// OLDFILENAMES.
	paths := h.strs(tagOldFilenames)
	if base := h.strs(tagBaseNames); len(base) > 0 {
		dirs, idx := h.strs(tagDirNames), h.ints(tagDirIndexes)
		paths = make([]string, len(base))
		for i, bn := range base {
			if i < len(idx) && int(idx[i]) < len(dirs) {
				paths[i] = dirs[idx[i]] + bn
			} else {
				paths[i] = bn
			}
		}
	}
	modes, fflags := h.ints(tagFileModes), h.ints(tagFileFlags)
	own := map[string]bool{}
	for i, path := range paths {
		f := rpmFile{Path: path}
		if i < len(modes) && modes[i]&0o170000 == 0o040000 {
			f.Dir = true
		}
		if i < len(fflags) && fflags[i]&(1<<6) != 0 { // RPMFILE_GHOST
			f.Ghost = true
		}
		own[path] = true
		p.Files = append(p.Files, f)
	}

	// Requires, as createrepo lists them: no rpmlib() dependencies, none on
	// a file or name the package provides itself, and each only once.
	provided := map[string]bool{}
	for _, d := range p.Provides {
		provided[d.Name+"\x00"+d.Flags+"\x00"+d.Epoch+"\x00"+d.Ver+"\x00"+d.Rel] = true
	}
	seen := map[string]bool{}
	for _, d := range rpmDeps(h, tagRequireName, tagRequireFlags, tagRequireVersion) {
		key := d.Name + "\x00" + d.Flags + "\x00" + d.Epoch + "\x00" + d.Ver + "\x00" + d.Rel
		if strings.HasPrefix(d.Name, "rpmlib(") || d.rawFlag&senseRPMLib != 0 || own[d.Name] || provided[key] || seen[key] {
			continue
		}
		seen[key] = true
		p.Requires = append(p.Requires, d)
	}

	times, names, texts := h.ints(tagChangelogTime), h.strs(tagChangelogName), h.strs(tagChangelogText)
	for i := range names {
		c := rpmChange{Author: names[i]}
		if i < len(times) {
			c.Date = times[i]
		}
		if i < len(texts) {
			c.Text = texts[i]
		}
		p.Changelog = append(p.Changelog, c)
	}
	return p, nil
}
