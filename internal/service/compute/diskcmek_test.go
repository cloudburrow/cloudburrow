package compute

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// fakeKMS is Cloud KMS with numbered versions: a ciphertext is its
// version's number, then the plaintext.
type fakeKMS struct {
	primary  map[string]int
	disabled map[string]bool // key/cryptoKeyVersions/N
}

func (f *fakeKMS) Encrypt(_ context.Context, key string, pt []byte) ([]byte, string, error) {
	v, ok := f.primary[key]
	if !ok {
		return nil, "", fmt.Errorf("Resource '%s' was not found", key)
	}
	name := key + "/cryptoKeyVersions/" + strconv.Itoa(v)
	if f.disabled[name] {
		return nil, "", fmt.Errorf("%s is not enabled, current state is: DISABLED.", name)
	}
	return append([]byte{byte(v)}, pt...), name, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, key string, ct []byte) ([]byte, error) {
	if len(ct) == 0 {
		return nil, errors.New("Decryption failed")
	}
	name := key + "/cryptoKeyVersions/" + strconv.Itoa(int(ct[0]))
	if f.disabled[name] {
		return nil, fmt.Errorf("%s is not enabled, current state is: DISABLED.", name)
	}
	return ct[1:], nil
}

// TestDiskCMEK (#2206): diskEncryptionKey.kmsKeyName on zonal and
// regional disks, the key's location, updateKmsKey's rotation and change,
// and the refusals of a disabled version.
func TestDiskCMEK(t *testing.T) {
	s, err := New(Options{VMs: newDataVMs()})
	if err != nil {
		t.Fatal(err)
	}
	const k1 = "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k1"
	const k2 = "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k2"
	const glob = "projects/p/locations/global/keyRings/r/cryptoKeys/g"
	const us = "projects/p/locations/us/keyRings/r/cryptoKeys/us"
	const eu = "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/eu"
	z := "zones/us-central1-a/"
	key := func(k string) obj { return obj{"diskEncryptionKey": obj{"kmsKeyName": k}} }
	with := func(b, k obj) obj {
		for n, v := range k {
			b[n] = v
		}
		return b
	}
	// Without Cloud KMS a key is refused.
	wantStatus(t, s, 400, "Cloud KMS error when using key", "POST", z+"disks", with(obj{"name": "a", "sizeGb": "10"}, key(k1)))
	f := &fakeKMS{primary: map[string]int{k1: 1, k2: 1, glob: 1, us: 1, eu: 1}, disabled: map[string]bool{}}
	s.UseKMS(f)
	wantStatus(t, s, 400, "same region as the disk", "POST", z+"disks", with(obj{"name": "a", "sizeGb": "10"}, key(eu)))
	wantStatus(t, s, 400, "keyRings/{key_ring}", "POST", z+"disks", with(obj{"name": "a", "sizeGb": "10"}, key(k1+"/cryptoKeyVersions/1")))
	for i, k := range []string{k1, glob, us} {
		mustCall(t, s, "POST", z+"disks", with(obj{"name": "d" + strconv.Itoa(i), "sizeGb": "10"}, key(k)))
	}
	d := mustCall(t, s, "GET", z+"disks/d0", nil)
	if got := str(d["diskEncryptionKey"].(map[string]any), "kmsKeyName"); got != k1+"/cryptoKeyVersions/1" {
		t.Errorf("kmsKeyName = %q", got)
	}
	if _, ok := d[wrappedKeyKey]; ok {
		t.Error("the wrapped key is returned")
	}
	r := "regions/us-central1/"
	zs := []any{"zones/us-central1-a", "zones/us-central1-b"}
	mustCall(t, s, "POST", r+"disks", with(obj{"name": "rd", "sizeGb": "10", "replicaZones": zs}, key(k1)))

	// Rotation to the primary version, then a change of key.
	f.primary[k1] = 2
	mustCall(t, s, "POST", z+"disks/d0/updateKmsKey", obj{})
	mustCall(t, s, "POST", r+"disks/rd/updateKmsKey", nil)
	for _, p := range []string{z + "disks/d0", r + "disks/rd"} {
		d := mustCall(t, s, "GET", p, nil)
		if got := str(d["diskEncryptionKey"].(map[string]any), "kmsKeyName"); got != k1+"/cryptoKeyVersions/2" {
			t.Errorf("%s after rotation: %q", p, got)
		}
	}
	mustCall(t, s, "POST", z+"disks/d0/updateKmsKey", obj{"kmsKeyName": k2})
	d = mustCall(t, s, "GET", z+"disks/d0", nil)
	if got := str(d["diskEncryptionKey"].(map[string]any), "kmsKeyName"); got != k2+"/cryptoKeyVersions/1" {
		t.Errorf("after the change: %q", got)
	}
	wantStatus(t, s, 400, "same region as the disk", "POST", z+"disks/d0/updateKmsKey", obj{"kmsKeyName": eu})
	// A disk without a CMEK gets none.
	mustCall(t, s, "POST", z+"disks", obj{"name": "plain", "sizeGb": "10"})
	wantStatus(t, s, 400, "can't encrypt existing resources", "POST", z+"disks/plain/updateKmsKey", obj{"kmsKeyName": k1})

	// A disabled primary version refuses the rotation; the disabled
	// version that wrapped the data key refuses it, attach, snapshot and
	// clone.
	f.primary[k1] = 3
	f.disabled[k1+"/cryptoKeyVersions/3"] = true
	wantStatus(t, s, 400, "DISABLED", "POST", r+"disks/rd/updateKmsKey", obj{})
	f.disabled[k2+"/cryptoKeyVersions/1"] = true
	wantStatus(t, s, 400, "DISABLED", "POST", z+"disks/d0/updateKmsKey", obj{})
	wantStatus(t, s, 400, "DISABLED", "POST", z+"disks/d0/createSnapshot", obj{"name": "s"})
	wantStatus(t, s, 400, "DISABLED", "POST", z+"disks", obj{"name": "clone", "sourceDisk": z + "disks/d0"})
	newVM(t, s, "us-central1-a", "vm", "n2-standard-2")
	wantStatus(t, s, 400, "DISABLED", "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/d0"})
	delete(f.disabled, k2+"/cryptoKeyVersions/1")
	mustCall(t, s, "POST", z+"instances/vm/attachDisk", obj{"source": z + "disks/d0"})
	vm := mustCall(t, s, "GET", z+"instances/vm", nil)
	found := false
	for _, e := range vm["disks"].([]any) {
		if ek, ok := e.(map[string]any)["diskEncryptionKey"].(map[string]any); ok && strings.HasPrefix(str(ek, "kmsKeyName"), k2) {
			found = true
		}
	}
	if !found {
		t.Error("the attached disk does not name its key")
	}
	// The instance does not start while a disk's key is revoked.
	mustCall(t, s, "POST", z+"instances/vm/stop", nil)
	f.disabled[k2+"/cryptoKeyVersions/1"] = true
	wantStatus(t, s, 400, "DISABLED", "POST", z+"instances/vm/start", nil)

	// Hyperdisk clones keep their key while the family exists.
	hd := z + "diskTypes/hyperdisk-balanced"
	mustCall(t, s, "POST", z+"disks", with(obj{"name": "h1", "sizeGb": "10", "type": hd}, key(glob)))
	mustCall(t, s, "POST", z+"disks", with(obj{"name": "h2", "sourceDisk": z + "disks/h1", "type": hd}, key(glob)))
	wantStatus(t, s, 400, "created by cloning", "POST", z+"disks/h2/updateKmsKey", obj{"kmsKeyName": us})
	mustCall(t, s, "POST", z+"disks/h2/updateKmsKey", obj{})
}
