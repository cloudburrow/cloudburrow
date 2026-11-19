// Package isoconformance fetches ISO/IEC 14496-26 (MPEG-4 audio
// conformance) streams and reference decodes for tests, at test time.
//
// ISO's files are under ISO's copyright and are never committed to the
// repository: a test that needs one asks Fetch (or File, which skips the
// test), which returns the copy cached under the user's cache directory
// (os.UserCacheDir()/cloudburrow-test/iso-14496-26-ed2, the directory the
// other pinned test downloads use, outside the repository). A missing copy is
// downloaded only when CLOUDBURROW_TEST_DOWNLOADS is set: ISO publishes the
// package only as zips of 1.2 and 9.8 GB, so each member is read out of its
// zip by HTTP range requests (the zip's central directory, then the member's
// own bytes), never the whole archive. Every file is checked against its
// pinned SHA-256 and ISO's MD5 (the package's md5 list) before it is moved
// into the cache, and again each time it is read. The pins are recorded in
// dependencies.json (components.testOracles.iso14496_26Conformance), which
// TestDependenciesRecordThePins holds to this file.
//
// Only tests import this package; nothing in the binary does.
package isoconformance

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// BaseURL is ISO's Publicly Available Standards directory of ISO/IEC
// 14496-26:2024 (2nd edition).
const BaseURL = "https://standards.iso.org/iso-iec/14496/-26/ed-2/en/"

// EnvDownloads, set to anything, lets tests download test data that is not
// cached yet. Unset, such a test skips.
const EnvDownloads = "CLOUDBURROW_TEST_DOWNLOADS"

// Pin is one file of the conformance package.
type Pin struct {
	// Zip is the archive under BaseURL, and ZipBytes its size, which a
	// changed archive would not keep.
	Zip      string
	ZipBytes int64
	// Member is the file's name inside the zip.
	Member string
	Bytes  int64
	SHA256 string
	// MD5 is ISO's own, from mpeg4audio-conformance2ndEd_20230125.md5.
	MD5 string
}

// Name is the file's base name, the key of Pins.
func (p Pin) Name() string { return filepath.Base(p.Member) }

const (
	compressedMp4      = "compressedMp4.zip"
	compressedMp4Bytes = 1207540409
	referencesWav      = "referencesWav.zip"
	referencesWavBytes = 9849662301
	// pkg is the directory the zips' member names start with.
	pkg = "../mpeg4audio-conformance2ndEd_20230125/"
)

// Pins are the files tests use, by base name: the HE-AAC v2 streams with
// parametric stereo beyond baseline and their unrestricted-PS ("_ur")
// reference decodes (24-bit, 32 kHz, stereo).
var Pins = map[string]Pin{}

func init() {
	for _, p := range []Pin{
		{compressedMp4, compressedMp4Bytes, pkg + "compressedMp4/al_sbr_ps_03.mp4", 28612,
			"717153ee39f505e897e3dc22357b39dbf0470c8033c7e0c76d5b3b68d6d8e238", "17f7d06f1c0dee51e7ca4a01d06b453b"},
		{compressedMp4, compressedMp4Bytes, pkg + "compressedMp4/al_sbr_ps_04.mp4", 28612,
			"89669b9f796678db2a9bf13cc33a0617e6864bf1b0152fa717ba3616d869a749", "596100860fe4ed5e53fcfcd5a1648f73"},
		{compressedMp4, compressedMp4Bytes, pkg + "compressedMp4/al_sbr_ps_06.mp4", 56598,
			"42d273c6260d2541d07e297257aa3238ebbcc1cb9eac5cf71bbfba69a6238132", "726dd94f7c29da1dc11314c837c37400"},
		{referencesWav, referencesWavBytes, pkg + "referencesWav/add-opt/al_sbr_ps_03_ur.wav", 1302716,
			"3b905534a0e90922dcde26048a0023f405bd1f496d6e35586fddda5502dbaafa", "0f4d0ddcd52d8ca7ea4de16a3c6aefd1"},
		{referencesWav, referencesWavBytes, pkg + "referencesWav/add-opt/al_sbr_ps_04_ur.wav", 1302716,
			"d00d6d52a362a31b362010e30f422898eb48ecd8829cfdbbbe0fabd927c41d35", "8d885cbc26f0a2764f83d17522de6562"},
		{referencesWav, referencesWavBytes, pkg + "referencesWav/add-opt/al_sbr_ps_06_ur.wav", 2605244,
			"e2ef76b498971db8871190e2102eadc747870ca00c83d0e9add7fb9be7d4e817", "a170700536bd15130a58d2cbe22ee99d"},
	} {
		Pins[p.Name()] = p
	}
}

// ErrNotCached means the file is not cached and downloads are not allowed.
var ErrNotCached = errors.New("not cached")

// CacheDir is where the files are cached.
func CacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cloudburrow-test", "iso-14496-26-ed2"), nil
}

// File returns the named file's bytes, fetching it first when it is not
// cached and EnvDownloads is set. It skips the test when the file is not
// cached and downloads are not allowed, and fails it when a download or a
// checksum fails.
func File(t testing.TB, name string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	b, err := Fetch(ctx, name)
	if errors.Is(err, ErrNotCached) {
		t.Skipf("%v; set %s=1 to download it from %s (about 8 MB in all, by HTTP range requests)", err, EnvDownloads, BaseURL)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var mu sync.Mutex

// Fetch returns the named file's bytes from the cache, downloading it
// first when it is missing and EnvDownloads is set.
func Fetch(ctx context.Context, name string) ([]byte, error) {
	p, ok := Pins[name]
	if !ok {
		return nil, fmt.Errorf("isoconformance: no pinned file %q", name)
	}
	dir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, name)
	mu.Lock()
	defer mu.Unlock()
	if b, err := os.ReadFile(dst); err == nil {
		if err := p.check(b); err != nil {
			return nil, fmt.Errorf("cached %s: %w (delete it to fetch it again)", dst, err)
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if os.Getenv(EnvDownloads) == "" {
		return nil, fmt.Errorf("ISO/IEC 14496-26 %s: %w in %s", name, ErrNotCached, dir)
	}
	b, err := download(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("downloading %s from %s%s: %w", p.Member, BaseURL, p.Zip, err)
	}
	if err := p.check(b); err != nil {
		return nil, fmt.Errorf("downloaded %s: %w", p.Member, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	return b, os.Rename(tmp.Name(), dst)
}

// check compares b with the pin's size, SHA-256 and MD5.
func (p Pin) check(b []byte) error {
	s, m := sha256.Sum256(b), md5.Sum(b)
	switch {
	case p.Bytes != 0 && int64(len(b)) != p.Bytes:
		return fmt.Errorf("%d bytes, want %d", len(b), p.Bytes)
	case hex.EncodeToString(m[:]) != p.MD5:
		return fmt.Errorf("MD5 %x, want ISO's %s", m, p.MD5)
	case hex.EncodeToString(s[:]) != p.SHA256:
		return fmt.Errorf("SHA-256 %x, want %s", s, p.SHA256)
	}
	return nil
}

// download reads p's member out of its zip on ISO's server by range
// requests.
func download(ctx context.Context, p Pin) ([]byte, error) {
	r := &httpReaderAt{ctx: ctx, url: BaseURL + p.Zip, blocks: map[int64][]byte{}}
	size, err := r.size()
	if err != nil {
		return nil, err
	}
	if size != p.ZipBytes {
		return nil, fmt.Errorf("the zip is %d bytes, want %d: ISO has changed it", size, p.ZipBytes)
	}
	// The member names start with "../", which archive/zip reports as
	// ErrInsecurePath; they are only compared here, never used as paths.
	zr, err := zip.NewReader(r, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name != p.Member {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, io.LimitReader(rc, 64<<20)); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("no member %s in the zip", p.Member)
}

// httpReaderAt reads a remote file by HTTP range requests, in blocks of
// blockSize, caching each block it reads.
type httpReaderAt struct {
	ctx    context.Context
	url    string
	blocks map[int64][]byte
	total  int64
}

const blockSize = 256 << 10

var client = &http.Client{Timeout: 2 * time.Minute}

func (r *httpReaderAt) size() (int64, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodHead, r.url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HEAD %s: %s", r.url, resp.Status)
	}
	if !strings.Contains(resp.Header.Get("Accept-Ranges"), "bytes") {
		return 0, fmt.Errorf("HEAD %s: no byte ranges", r.url)
	}
	r.total = resp.ContentLength
	return r.total, nil
}

func (r *httpReaderAt) block(i int64) ([]byte, error) {
	if b, ok := r.blocks[i]; ok {
		return b, nil
	}
	start, end := i*blockSize, min((i+1)*blockSize, r.total)-1
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("GET %s bytes %d-%d: %s", r.url, start, end, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, blockSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != end-start+1 {
		return nil, fmt.Errorf("GET %s bytes %d-%d: %d bytes", r.url, start, end, len(b))
	}
	r.blocks[i] = b
	return b, nil
}

func (r *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		o := off + int64(n)
		if o >= r.total {
			return n, io.EOF
		}
		b, err := r.block(o / blockSize)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], b[o%blockSize:])
	}
	return n, nil
}
