package compute

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSealOpen(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, n := range []int{0, 1, sealChunk - 1, sealChunk, sealChunk + 1, 3*sealChunk + 17} {
		pt := make([]byte, n)
		_, _ = rand.Read(pt)
		var sealed bytes.Buffer
		if err := Seal(&sealed, bytes.NewReader(pt), key); err != nil {
			t.Fatal(err)
		}
		if n > 64 && bytes.Contains(sealed.Bytes(), pt[:64]) {
			t.Fatalf("%d bytes: the plaintext is in the sealed file", n)
		}
		var out bytes.Buffer
		if err := Open(&out, bytes.NewReader(sealed.Bytes()), key); err != nil || !bytes.Equal(out.Bytes(), pt) {
			t.Fatalf("%d bytes: Open = %v, equal %v", n, err, bytes.Equal(out.Bytes(), pt))
		}
		wrong := bytes.Repeat([]byte{8}, 32)
		if err := Open(&bytes.Buffer{}, bytes.NewReader(sealed.Bytes()), wrong); !errors.Is(err, ErrSealed) {
			t.Errorf("%d bytes, wrong key: %v", n, err)
		}
		if sealed.Len() > 30 {
			trunc := sealed.Bytes()[:sealed.Len()-5]
			if err := Open(&bytes.Buffer{}, bytes.NewReader(trunc), key); !errors.Is(err, ErrSealed) {
				t.Errorf("%d bytes, truncated: %v", n, err)
			}
		}
	}
	// A whole last chunk dropped is caught too.
	pt := make([]byte, 2*sealChunk+10)
	var sealed bytes.Buffer
	_ = Seal(&sealed, bytes.NewReader(pt), key)
	cut := len(sealMagic) + 8 + 2*(4+sealChunk+16)
	if err := Open(&bytes.Buffer{}, bytes.NewReader(sealed.Bytes()[:cut]), key); !errors.Is(err, ErrSealed) {
		t.Errorf("last chunk dropped: %v", err)
	}
}

// sealVMs keeps each disk's contents as the real backend does: in the
// clear while open, sealed with its data key at rest.
type sealVMs struct {
	fakeVMs
	mu     sync.Mutex
	plain  map[string]string
	sealed map[string][]byte
	starts map[string]VMSpec
}

func newSealVMs() *sealVMs {
	return &sealVMs{plain: map[string]string{}, sealed: map[string][]byte{}, starts: map[string]VMSpec{}}
}

func (f *sealVMs) copyLocked(src, dst string) {
	delete(f.plain, dst)
	delete(f.sealed, dst)
	if b, ok := f.sealed[src]; ok {
		f.sealed[dst] = b
	} else {
		f.plain[dst] = f.plain[src]
	}
}

func (f *sealVMs) rekeyLocked(k string, from, to []byte) error {
	if b, ok := f.sealed[k]; ok {
		if from == nil {
			return errors.New("sealed without a key")
		}
		var out bytes.Buffer
		if err := Open(&out, bytes.NewReader(b), from); err != nil {
			return err
		}
		delete(f.sealed, k)
		f.plain[k] = out.String()
	}
	if to != nil {
		return f.sealLocked(k, to)
	}
	return nil
}

func (f *sealVMs) sealLocked(k string, key []byte) error {
	if _, ok := f.sealed[k]; ok {
		return nil
	}
	var out bytes.Buffer
	if err := Seal(&out, strings.NewReader(f.plain[k]), key); err != nil {
		return err
	}
	f.sealed[k] = out.Bytes()
	delete(f.plain, k)
	return nil
}

func (f *sealVMs) CreateDisk(_ context.Context, d DiskSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.SourceSnapshot != "" {
		f.copyLocked(d.SourceSnapshot, d.Key)
		return f.rekeyLocked(d.Key, d.SourceDataKey, d.DataKey)
	}
	f.plain[d.Key] = ""
	return f.rekeyLocked(d.Key, nil, d.DataKey)
}

func (f *sealVMs) CopyDisk(_ context.Context, src, dst DiskSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyLocked(src.Key, dst.Key)
	return f.rekeyLocked(dst.Key, src.DataKey, dst.DataKey)
}

func (f *sealVMs) Snapshot(_ context.Context, d DiskSpec, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyLocked(d.Key, key)
	if d.DataKey != nil {
		return f.sealLocked(key, d.DataKey)
	}
	return nil
}

func (f *sealVMs) SealDisk(_ context.Context, d DiskSpec) error {
	if d.DataKey == nil || d.Open {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sealLocked(d.Key, d.DataKey)
}

func (f *sealVMs) UnsealDisk(_ context.Context, d DiskSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rekeyLocked(d.Key, d.DataKey, nil)
}

func (f *sealVMs) Start(_ context.Context, vm VMSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range append([]AttachedDisk{{Disk: vm.BootDisk}}, vm.Disks...) {
		if _, ok := f.sealed[d.Disk.Key]; ok {
			return errors.New("a sealed disk was mounted")
		}
	}
	f.starts[vm.Name] = vm
	return nil
}

func (f *sealVMs) Stop(_ context.Context, vm VMSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.starts, vm.Name)
	return nil
}

func (f *sealVMs) diskKey(t *testing.T, s *Server, path string) string {
	t.Helper()
	d := mustCall(t, s, "GET", path, nil)
	return "disk-" + str(d, "id")
}

func (f *sealVMs) state(k string) (plain string, sealed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, sealed = f.sealed[k]
	return f.plain[k], sealed
}

func (f *sealVMs) put(k, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain[k] = v
}

// TestDiskEncryptionAtRest (#2206): a CMEK disk is sealed whenever no
// running instance has it, opened for one, unusable with its key version
// disabled, and usable again once the version is enabled.
func TestDiskEncryptionAtRest(t *testing.T) {
	vms := newSealVMs()
	s, err := New(Options{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	const k1 = "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k1"
	f := &fakeKMS{primary: map[string]int{k1: 1}, disabled: map[string]bool{}}
	s.UseKMS(f)
	z := "zones/us-central1-a/"
	mustCall(t, s, "POST", z+"disks", obj{"name": "data", "sizeGb": "10", "diskEncryptionKey": obj{"kmsKeyName": k1}})
	key := vms.diskKey(t, s, z+"disks/data")
	if _, sealed := vms.state(key); !sealed {
		t.Fatal("a new CMEK disk is not sealed")
	}
	newVM(t, s, "us-central1-a", "vm", "n2-standard-2")
	mustCall(t, s, "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/data", "deviceName": "data"})
	if _, sealed := vms.state(key); sealed {
		t.Fatal("the attached disk of a running instance is still sealed")
	}
	vms.put(key, "secret")
	mustCall(t, s, "POST", z+"instances/vm/stop", nil)
	if p, sealed := vms.state(key); !sealed || p != "" {
		t.Fatalf("after stop: sealed %v, plaintext %q", sealed, p)
	}
	// A snapshot of it is sealed, with the same key; a disk from it opens.
	mustCall(t, s, "POST", z+"disks/data/createSnapshot", obj{"name": "snap"})
	mustCall(t, s, "POST", z+"disks", obj{"name": "restored", "sourceSnapshot": "global/snapshots/snap"})
	if p, sealed := vms.state(vms.diskKey(t, s, z+"disks/restored")); sealed || p != "secret" {
		t.Errorf("a disk from the snapshot (no key): sealed %v, %q", sealed, p)
	}
	// Disabled: no start; enabled again: the data is back.
	f.disabled[k1+"/cryptoKeyVersions/1"] = true
	wantStatus(t, s, 400, "DISABLED", "POST", z+"instances/vm/start", nil)
	delete(f.disabled, k1+"/cryptoKeyVersions/1")
	mustCall(t, s, "POST", z+"instances/vm/start", nil)
	if p, sealed := vms.state(key); sealed || p != "secret" {
		t.Errorf("after the key is enabled again: sealed %v, %q", sealed, p)
	}
	// Detaching from the running instance seals it.
	out, err := s.Call("POST", "p", z+"instances/vm/detachDisk", map[string][]string{"deviceName": {"data"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.waitOp(context.Background(), str(out, "selfLink"))
	if _, sealed := vms.state(key); !sealed {
		t.Error("a detached CMEK disk is not sealed")
	}
}

// TestDiskCSEK (#2206): rawKey and rsaEncryptedKey disks: only sha256 is
// answered, every use names the key, a wrong one is refused, suspend and
// updateKmsKey are refused, and the disk is sealed at rest.
func TestDiskCSEK(t *testing.T) {
	vms := newSealVMs()
	s, err := New(Options{VMs: vms})
	if err != nil {
		t.Fatal(err)
	}
	z := "zones/us-central1-a/"
	raw := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	ck := obj{"rawKey": raw}
	wantStatus(t, s, 400, "256-bit", "POST", z+"disks", obj{"name": "x", "sizeGb": "10", "diskEncryptionKey": obj{"rawKey": "c2hvcnQ="}})
	mustCall(t, s, "POST", z+"disks", obj{"name": "c", "sizeGb": "10", "diskEncryptionKey": ck})
	d := mustCall(t, s, "GET", z+"disks/c", nil)
	ek, _ := d["diskEncryptionKey"].(map[string]any)
	if str(ek, "sha256") != csekSHA256(bytes.Repeat([]byte{1}, 32)) || str(ek, "rawKey") != "" {
		t.Errorf("diskEncryptionKey = %v", ek)
	}
	key := vms.diskKey(t, s, z+"disks/c")
	if _, sealed := vms.state(key); !sealed {
		t.Fatal("a CSEK disk is not sealed")
	}
	newVM(t, s, "us-central1-a", "vm", "n2-standard-2")
	wantStatus(t, s, 400, "the key was not provided", "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/c", "deviceName": "c"})
	wantStatus(t, s, 400, "is incorrect", "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/c", "deviceName": "c", "diskEncryptionKey": obj{"rawKey": other}})
	mustCall(t, s, "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/c", "deviceName": "c", "diskEncryptionKey": ck})
	vms.put(key, "csek data")
	wantStatus(t, s, 400, "CSEK-protected", "POST", z+"instances/vm/suspend", nil)
	wantStatus(t, s, 400, "Customer-Supplied Encryption Key", "POST", z+"disks/c/updateKmsKey", obj{})
	mustCall(t, s, "POST", z+"instances/vm/stop", nil)
	if _, sealed := vms.state(key); !sealed {
		t.Error("not sealed after stop")
	}
	wantStatus(t, s, 400, "the key was not provided", "POST", z+"instances/vm/start", nil)
	src := "projects/p/zones/us-central1-a/disks/c"
	wantStatus(t, s, 400, "is incorrect", "POST", z+"instances/vm/startWithEncryptionKey",
		obj{"disks": []any{obj{"source": src, "diskEncryptionKey": obj{"rawKey": other}}}})
	mustCall(t, s, "POST", z+"instances/vm/startWithEncryptionKey", obj{"disks": []any{obj{"source": src, "diskEncryptionKey": ck}}})
	if p, _ := vms.state(key); p != "csek data" {
		t.Errorf("after startWithEncryptionKey: %q", p)
	}
	// Snapshots: the source key is needed; with snapshotEncryptionKey the
	// snapshot needs that key too.
	wantStatus(t, s, 400, "the key was not provided", "POST", z+"disks/c/createSnapshot", obj{"name": "s1"})
	mustCall(t, s, "POST", z+"disks/c/createSnapshot", obj{"name": "s1", "sourceDiskEncryptionKey": ck, "snapshotEncryptionKey": obj{"rawKey": other}})
	mustCall(t, s, "POST", z+"disks/c/createSnapshot", obj{"name": "s2", "sourceDiskEncryptionKey": ck})
	wantStatus(t, s, 400, "the key was not provided", "POST", z+"disks", obj{"name": "r1", "sourceSnapshot": "global/snapshots/s1"})
	mustCall(t, s, "POST", z+"disks", obj{"name": "r1", "sourceSnapshot": "global/snapshots/s1", "sourceSnapshotEncryptionKey": obj{"rawKey": other}})
	mustCall(t, s, "POST", z+"disks", obj{"name": "r2", "sourceSnapshot": "global/snapshots/s2"})
	for _, n := range []string{"r1", "r2"} {
		if p, sealed := vms.state(vms.diskKey(t, s, z+"disks/"+n)); sealed || p != "csek data" {
			t.Errorf("%s: sealed %v, %q", n, sealed, p)
		}
	}

	// rsaEncryptedKey, wrapped with the served certificate.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", CSEKCertPath, nil))
	blk, _ := pem.Decode(rec.Body.Bytes())
	if blk == nil {
		t.Fatalf("certificate: %s", rec.Body.String())
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, cert.PublicKey.(*rsa.PublicKey), bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	mustCall(t, s, "POST", z+"disks", obj{"name": "rsa", "sizeGb": "10", "diskEncryptionKey": obj{"rsaEncryptedKey": base64.StdEncoding.EncodeToString(wrapped)}})
	d = mustCall(t, s, "GET", z+"disks/rsa", nil)
	if ek, _ := d["diskEncryptionKey"].(map[string]any); str(ek, "sha256") != csekSHA256(bytes.Repeat([]byte{1}, 32)) {
		t.Errorf("rsa disk key = %v", ek)
	}
	// The same key, given raw, opens it.
	mustCall(t, s, "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/rsa", "deviceName": "rsa", "diskEncryptionKey": ck})
}
