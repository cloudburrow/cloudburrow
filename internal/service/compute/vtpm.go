package compute

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/go-tpm/legacy/tpm2"
	"github.com/google/go-tpm/tpmutil"
)

// Shielded VM's vTPM (#2179), as Google documents it
// (https://docs.cloud.google.com/compute/shielded-vm/docs/shielded-vm,
// .../integrity-monitoring, .../retrieving-endorsement-key, read
// 2026-11-22). Each instance with shieldedInstanceConfig.enableVtpm has a
// software TPM 2.0 of its own (swtpm, which the VM backend runs beside the
// guest, its state on a claim of its own, so it survives stop and start
// as the vTPM's does), reached through VMTPM. At its first boot this
// package provisions it as Compute Engine's vTPM is:
//
//   - the RSA 2048 and ECC P-256 endorsement keys (the TCG default
//     templates), persisted at 0x81010001 and 0x81010002, their
//     certificates at NV 0x01c00002 and 0x01c0000a;
//   - the attestation (signing) keys from the templates at NV 0x01c10001
//     (RSA) and 0x01c10003 (ECC), their certificates at 0x01c10000 and
//     0x01c10002, which go-tpm-tools' GceAttestationKeyRSA/ECC read;
//   - every certificate issued by the instance's vTPM CA, CloudBurrow's
//     stand-in for Google's EK/AK CA.
//
// At every boot the firmware's measurements are extended into PCRs 0, 2,
// 4 and 7 (firmware, option ROMs, boot loader and kernel, Secure Boot
// policy); integrity monitoring compares the early boot (PCRs 0-7 up to
// the separator) and late boot (PCRs 0-9) measurements with the
// instance's integrity policy baseline, learnt at its first boot and
// learnt again by setShieldedInstanceIntegrityPolicy, and reports both.

// VMTPM is a VM backend that gives instances a vTPM: OpenTPM is a channel
// to the running instance's vTPM (raw TPM 2.0 commands and responses);
// DeleteTPM forgets an instance's vTPM state.
type VMTPM interface {
	OpenTPM(ctx context.Context, vm VMSpec) (io.ReadWriteCloser, error)
	DeleteTPM(ctx context.Context, vm VMSpec) error
}

// The vTPM's handles.
const (
	ekRSAHandle   = tpmutil.Handle(0x81010001)
	ekECCHandle   = tpmutil.Handle(0x81010002)
	ekCertRSA     = tpmutil.Handle(0x01c00002)
	ekCertECC     = tpmutil.Handle(0x01c0000a)
	akCertRSA     = tpmutil.Handle(0x01c10000)
	akTemplateRSA = tpmutil.Handle(0x01c10001)
	akCertECC     = tpmutil.Handle(0x01c10002)
	akTemplateECC = tpmutil.Handle(0x01c10003)
)

// vtpmOn reports whether an instance has a vTPM.
func vtpmOn(o obj) bool {
	sc := objOf(o["shieldedInstanceConfig"])
	return sc != nil && sc["enableVtpm"] == true
}

func ekAuthPolicy() []byte {
	buf, err := tpmutil.Pack(tpm2.CmdPolicySecret, tpm2.HandleEndorsement)
	if err != nil {
		panic(err)
	}
	d1 := sha256.Sum256(append(make([]byte, 32), buf...))
	d2 := sha256.Sum256(d1[:])
	return d2[:]
}

func symAES128() *tpm2.SymScheme {
	return &tpm2.SymScheme{Alg: tpm2.AlgAES, KeyBits: 128, Mode: tpm2.AlgCFB}
}

// The TCG's default EK templates and go-tpm-tools' AK templates
// (go-tpm-tools client/template.go, Apache-2.0, Copyright Google LLC).
func ekTemplateECC() tpm2.Public {
	return tpm2.Public{Type: tpm2.AlgECC, NameAlg: tpm2.AlgSHA256,
		Attributes: (tpm2.FlagStorageDefault | tpm2.FlagAdminWithPolicy) & ^tpm2.FlagUserWithAuth, AuthPolicy: ekAuthPolicy(),
		ECCParameters: &tpm2.ECCParams{Symmetric: symAES128(), CurveID: tpm2.CurveNISTP256,
			Point: tpm2.ECPoint{XRaw: make([]byte, 32), YRaw: make([]byte, 32)}}}
}

func ekTemplateRSA() tpm2.Public {
	return tpm2.Public{Type: tpm2.AlgRSA, NameAlg: tpm2.AlgSHA256,
		Attributes: (tpm2.FlagStorageDefault | tpm2.FlagAdminWithPolicy) & ^tpm2.FlagUserWithAuth, AuthPolicy: ekAuthPolicy(),
		RSAParameters: &tpm2.RSAParams{Symmetric: symAES128(), KeyBits: 2048, ModulusRaw: make([]byte, 256)}}
}

func akTemplateRSAPublic() tpm2.Public {
	return tpm2.Public{Type: tpm2.AlgRSA, NameAlg: tpm2.AlgSHA256, Attributes: tpm2.FlagSignerDefault,
		RSAParameters: &tpm2.RSAParams{Sign: &tpm2.SigScheme{Alg: tpm2.AlgRSASSA, Hash: tpm2.AlgSHA256}, KeyBits: 2048}}
}

func akTemplateECCPublic() tpm2.Public {
	return tpm2.Public{Type: tpm2.AlgECC, NameAlg: tpm2.AlgSHA256, Attributes: tpm2.FlagSignerDefault,
		ECCParameters: &tpm2.ECCParams{Sign: &tpm2.SigScheme{Alg: tpm2.AlgECDSA, Hash: tpm2.AlgSHA256}, CurveID: tpm2.CurveNISTP256,
			Point: tpm2.ECPoint{XRaw: make([]byte, 32), YRaw: make([]byte, 32)}}}
}

// vtpmCA is the instance-wide CA that issues the vTPMs' EK and AK
// certificates, kept beside the state file.
type vtpmCA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

func (s *Server) vtpmCALocked() (*vtpmCA, error) {
	if s.ca != nil {
		return s.ca, nil
	}
	path := ""
	if s.opts.State != "" {
		path = s.opts.State + ".vtpm-ca.pem"
		if b, err := os.ReadFile(path); err == nil {
			var key *ecdsa.PrivateKey
			var cert *x509.Certificate
			for rest := b; ; {
				var blk *pem.Block
				blk, rest = pem.Decode(rest)
				if blk == nil {
					break
				}
				switch blk.Type {
				case "EC PRIVATE KEY":
					key, _ = x509.ParseECPrivateKey(blk.Bytes)
				case "CERTIFICATE":
					cert, _ = x509.ParseCertificate(blk.Bytes)
				}
			}
			if key != nil && cert != nil {
				s.ca = &vtpmCA{key: key, cert: cert}
				return s.ca, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := s.opts.Now()
	tmpl := &x509.Certificate{SerialNumber: certSerial(),
		Subject:   pkix.Name{CommonName: "CloudBurrow vTPM CA", Organization: []string{"CloudBurrow"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(mdsRootLife), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	if path != "" {
		if err := os.WriteFile(path, []byte(pemKey(key)+pemCert(der)), 0o600); err != nil {
			return nil, err
		}
	}
	s.ca = &vtpmCA{key: key, cert: cert}
	return s.ca, nil
}

// oidTPMManufacturer etc. are the TCG's subject alternative name
// attributes of an EK certificate.
var (
	oidTPMManufacturer = asn1.ObjectIdentifier{2, 23, 133, 2, 1}
	oidTPMModel        = asn1.ObjectIdentifier{2, 23, 133, 2, 2}
	oidTPMVersion      = asn1.ObjectIdentifier{2, 23, 133, 2, 3}
)

func (ca *vtpmCA) issue(pub crypto.PublicKey, cn string, ek bool, now time.Time) ([]byte, error) {
	tmpl := &x509.Certificate{SerialNumber: certSerial(), Subject: pkix.Name{CommonName: cn, Organization: []string{"CloudBurrow"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 365 * 24 * time.Hour), BasicConstraintsValid: true}
	if ek {
		tmpl.KeyUsage = x509.KeyUsageKeyEncipherment
		if _, ok := pub.(*ecdsa.PublicKey); ok {
			tmpl.KeyUsage = x509.KeyUsageKeyAgreement
		}
		tmpl.Subject = pkix.Name{}
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, tpmSAN())
	} else {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
}

// tpmSAN is the EK certificate's directoryName subject alternative name:
// the vTPM's manufacturer ("id:474F4F47", GOOG), model and version.
func tpmSAN() pkix.Extension {
	type atv struct {
		Type  asn1.ObjectIdentifier
		Value string `asn1:"utf8"`
	}
	rdn := []atv{{oidTPMManufacturer, "id:474F4F47"}, {oidTPMModel, "vTPM"}, {oidTPMVersion, "id:00010000"}}
	set := [][]atv{rdn}
	name, _ := asn1.Marshal(set)
	dir, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 4, IsCompound: true, Bytes: name})
	seq, _ := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: dir})
	return pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Critical: true, Value: seq}
}

func pemPublic(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// nvStore defines index (platform-created, readable with owner auth, as
// a vTPM's certificates are) and writes data to it, replacing what it had.
func nvStore(rw io.ReadWriter, index tpmutil.Handle, data []byte) error {
	_ = tpm2.NVUndefineSpace(rw, "", tpm2.HandlePlatform, index)
	attr := tpm2.AttrPPWrite | tpm2.AttrPPRead | tpm2.AttrOwnerRead | tpm2.AttrAuthRead | tpm2.AttrNoDA | tpm2.AttrPlatformCreate
	if err := tpm2.NVDefineSpace(rw, tpm2.HandlePlatform, index, "", "", nil, attr, uint16(len(data))); err != nil {
		return fmt.Errorf("define NV %#x: %w", uint32(index), err)
	}
	for off := 0; off < len(data); off += 512 {
		end := min(off+512, len(data))
		if err := tpm2.NVWrite(rw, tpm2.HandlePlatform, index, "", data[off:end], uint16(off)); err != nil {
			return fmt.Errorf("write NV %#x: %w", uint32(index), err)
		}
	}
	return nil
}

// createPersisted makes the primary key template in the endorsement
// hierarchy and persists it at handle; it returns its public key.
func createPersisted(rw io.ReadWriter, template tpm2.Public, handle tpmutil.Handle) (crypto.PublicKey, error) {
	if pub, _, _, err := tpm2.ReadPublic(rw, handle); err == nil {
		if pub.MatchesTemplate(template) {
			return pub.Key()
		}
		_ = tpm2.EvictControl(rw, "", tpm2.HandleOwner, handle, handle)
	}
	h, pub, err := tpm2.CreatePrimary(rw, tpm2.HandleEndorsement, tpm2.PCRSelection{}, "", "", template)
	if err != nil {
		return nil, err
	}
	defer tpm2.FlushContext(rw, h)
	if err := tpm2.EvictControl(rw, "", tpm2.HandleOwner, h, handle); err != nil {
		return nil, err
	}
	return pub, nil
}

// primary makes a primary key of template in the endorsement hierarchy
// and returns its public key.
func primary(rw io.ReadWriter, template tpm2.Public) (crypto.PublicKey, error) {
	h, pub, err := tpm2.CreatePrimary(rw, tpm2.HandleEndorsement, tpm2.PCRSelection{}, "", "", template)
	if err != nil {
		return nil, err
	}
	_ = tpm2.FlushContext(rw, h)
	return pub, nil
}

// provisionVTPM gives a new vTPM its keys and certificates, and returns
// what the instance keeps of it (_vtpm).
func (s *Server) provisionVTPM(rw io.ReadWriter, name string) (obj, error) {
	s.mu.Lock()
	ca, err := s.vtpmCALocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	now := s.opts.Now()
	out := obj{}
	type key struct {
		field    string
		template tpm2.Public
		persist  tpmutil.Handle
		tmplNV   tpmutil.Handle
		certNV   tpmutil.Handle
		ek       bool
	}
	for _, k := range []key{
		{"ekRsa", ekTemplateRSA(), ekRSAHandle, 0, ekCertRSA, true},
		{"ekEcc", ekTemplateECC(), ekECCHandle, 0, ekCertECC, true},
		{"akRsa", akTemplateRSAPublic(), 0, akTemplateRSA, akCertRSA, false},
		{"akEcc", akTemplateECCPublic(), 0, akTemplateECC, akCertECC, false},
	} {
		var pub crypto.PublicKey
		var err error
		if k.persist != 0 {
			pub, err = createPersisted(rw, k.template, k.persist)
		} else {
			enc, eerr := k.template.Encode()
			if eerr != nil {
				return nil, eerr
			}
			if err := nvStore(rw, k.tmplNV, enc); err != nil {
				return nil, err
			}
			pub, err = primary(rw, k.template)
		}
		if err != nil {
			return nil, fmt.Errorf("vTPM %s: %w", k.field, err)
		}
		cn := name + " vTPM attestation key"
		if k.ek {
			cn = name + " vTPM endorsement key"
		}
		der, err := ca.issue(pub, cn, k.ek, now)
		if err != nil {
			return nil, err
		}
		if err := nvStore(rw, k.certNV, der); err != nil {
			return nil, err
		}
		p, err := pemPublic(pub)
		if err != nil {
			return nil, err
		}
		out[k.field+"Pub"], out[k.field+"Cert"] = p, pemCert(der)
	}
	return out, nil
}

// bootMeasurements are the firmware's measurements of a boot: PCR to the
// digests extended into it, in order.
func bootMeasurements(o obj, image string) map[int][][]byte {
	d := func(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }
	sb := "disabled"
	if objOf(o["shieldedInstanceConfig"])["enableSecureBoot"] == true {
		sb = "enabled"
	}
	sep := make([]byte, 32)
	return map[int][][]byte{
		0: {d("CloudBurrow vTPM firmware: Compute Engine virtual firmware"), sep},
		2: {sep},
		4: {d("boot loader: " + image), d("kernel: " + image), sep},
		7: {d("SecureBoot: " + sb), d("PK KEK db dbx: Google"), sep},
	}
}

// measureBoot extends the boot's measurements and reads PCRs 0-9.
func measureBoot(rw io.ReadWriter, m map[int][][]byte) (map[int]string, error) {
	for pcr := 0; pcr <= 9; pcr++ {
		for _, dg := range m[pcr] {
			if err := tpm2.PCRExtend(rw, tpmutil.Handle(pcr), tpm2.AlgSHA256, dg, ""); err != nil {
				return nil, fmt.Errorf("extend PCR %d: %w", pcr, err)
			}
		}
	}
	out := map[int]string{}
	for pcr := 0; pcr <= 9; pcr++ {
		v, err := tpm2.ReadPCR(rw, pcr, tpm2.AlgSHA256)
		if err != nil {
			return nil, err
		}
		out[pcr] = hex.EncodeToString(v)
	}
	return out, nil
}

func pcrObj(p map[int]string, from, to int) obj {
	out := obj{}
	for i := from; i <= to; i++ {
		out[fmt.Sprintf("%d", i)] = p[i]
	}
	return out
}

// startVM starts vm on the backend and, for a Shielded VM, provisions and
// measures its vTPM meanwhile, as the vTPM is there from power on: its
// container starts before the guest does, so the credentials endpoint
// (which waits for the vTPM) answers Google's guest agent early in boot.
func (s *Server) startVM(ctx context.Context, rel string, vm VMSpec) error {
	if _, ok := s.opts.VMs.(VMTPM); !ok || !vm.VTPM {
		return s.opts.VMs.Start(ctx, vm)
	}
	vctx, cancel := context.WithCancel(ctx)
	defer cancel()
	booted := make(chan error, 1)
	go func() {
		// The pod, and so its vTPM container, appears during the start:
		// retry until it answers.
		deadline := time.Now().Add(5 * time.Minute)
		for {
			err := s.bootVTPM(vctx, rel, vm)
			if err == nil || vctx.Err() != nil || time.Now().After(deadline) {
				booted <- err
				return
			}
			select {
			case <-vctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()
	if err := s.opts.VMs.Start(ctx, vm); err != nil {
		cancel()
		<-booted
		return err
	}
	return <-booted
}

// bootVTPM is a start's vTPM work, after the backend started the
// instance: the first boot's provisioning and every boot's measurements.
func (s *Server) bootVTPM(ctx context.Context, rel string, vm VMSpec) error {
	t, ok := s.opts.VMs.(VMTPM)
	if !ok || !vm.VTPM {
		return nil
	}
	rw, err := t.OpenTPM(ctx, vm)
	if err != nil {
		return fmt.Errorf("open the vTPM of %s: %w", vm.Name, err)
	}
	defer rw.Close()
	s.mu.Lock()
	inst, ok := s.res[rel]
	var have obj
	var o obj
	if ok {
		have = objOf(inst["_vtpm"])
		o = copyObj(inst)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	rec := have
	if pub, _, _, err := tpm2.ReadPublic(rw, ekECCHandle); err != nil || have == nil || !samePub(pub, str(have, "ekEccPub")) {
		rec, err = s.provisionVTPM(rw, vm.Name)
		if err != nil {
			return err
		}
	}
	pcrs, err := measureBoot(rw, bootMeasurements(o, vm.Image.Name))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok = s.res[rel]
	if !ok {
		return nil
	}
	inst["_vtpm"] = rec
	s.integrityReportLocked(inst, pcrs)
	ig := objOf(inst["_integrity"])
	if err := s.saveLocked(); err != nil {
		return err
	}
	if ig != nil && s.opts.IntegrityEvents != nil {
		// Integrity monitoring's events, in Cloud Logging's
		// compute.googleapis.com/shielded_vm_integrity log.
		n, _ := ig["bootCounter"].(float64)
		for k, base := range map[string]string{"earlyBootReportEvent": "baselineEarly", "lateBootReportEvent": "baselineLate"} {
			ev := copyObj(objOf(ig[k]))
			ev["policyMeasurements"] = ig[base]
			payload := obj{"@type": "type.googleapis.com/cloud_integrity.IntegrityEvent", "bootCounter": fmt.Sprint(int(n)), k: ev}
			go s.opts.IntegrityEvents(context.Background(), vm.Project, vm.Zone, str(inst, "id"), payload)
		}
	}
	return nil
}

func samePub(p tpm2.Public, want string) bool {
	k, err := p.Key()
	if err != nil {
		return false
	}
	got, err := pemPublic(k)
	return err == nil && got == want
}

// integrityReportLocked compares a boot's measurements with the
// instance's integrity policy baseline (learning it when there is none,
// or when setShieldedInstanceIntegrityPolicy asked to learn it again) and
// keeps the early and late boot reports.
func (s *Server) integrityReportLocked(inst obj, pcrs map[int]string) {
	if objOf(inst["shieldedInstanceConfig"])["enableIntegrityMonitoring"] != true {
		delete(inst, "_integrity")
		return
	}
	ig := objOf(inst["_integrity"])
	if ig == nil {
		ig = obj{}
	}
	early, late := pcrObj(pcrs, 0, 7), pcrObj(pcrs, 0, 9)
	if objOf(ig["baselineEarly"]) == nil || ig["learn"] == true {
		ig["baselineEarly"], ig["baselineLate"] = early, late
		delete(ig, "learn")
	}
	same := func(a, b obj) bool {
		if len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if b[k] != v {
				return false
			}
		}
		return true
	}
	ig["earlyBootReportEvent"] = obj{"policyEvaluationPassed": same(early, objOf(ig["baselineEarly"])), "actualMeasurements": early}
	ig["lateBootReportEvent"] = obj{"policyEvaluationPassed": same(late, objOf(ig["baselineLate"])), "actualMeasurements": late}
	ig["time"] = s.stamp()
	n, _ := ig["bootCounter"].(float64)
	ig["bootCounter"] = n + 1
	inst["_integrity"] = ig
}

// getShieldedInstanceIdentity is instances.getShieldedInstanceIdentity.
func (s *Server) getShieldedInstanceIdentity(rel string, o obj) (obj, error) {
	if !vtpmOn(o) {
		return nil, errf(400, "badRequest", "Invalid resource usage: 'Instance '%s' does not have vTPM enabled.'.", str(o, "name"))
	}
	v := objOf(o["_vtpm"])
	if v == nil {
		return nil, errf(400, "resourceNotReady", "The resource '%s' is not ready: its vTPM has not been provisioned (the instance has not booted).", rel)
	}
	entry := func(k string) obj { return obj{"ekPub": str(v, k+"Pub"), "ekCert": str(v, k+"Cert")} }
	return obj{"kind": "compute#shieldedInstanceIdentity", "encryptionKey": entry("ekRsa"), "signingKey": entry("akRsa"),
		"eccP256EncryptionKey": entry("ekEcc"), "eccP256SigningKey": entry("akEcc")}, nil
}

// checkShieldedConfig is an instance's shieldedInstanceConfig as Google
// defaults it for a Shielded VM image (Secure Boot off, vTPM and
// integrity monitoring on), merged with in; integrity monitoring needs
// the vTPM.
func checkShieldedConfig(field string, in, cur obj) (obj, error) {
	out := obj{"enableSecureBoot": false, "enableVtpm": true, "enableIntegrityMonitoring": true}
	for k, v := range cur {
		out[k] = v
	}
	for _, k := range []string{"enableSecureBoot", "enableVtpm", "enableIntegrityMonitoring"} {
		if v, ok := in[k]; ok {
			b, isBool := v.(bool)
			if !isBool {
				return nil, invalid("Invalid value for field '%s.%s': '%v'. Expected a boolean.", field, k, v)
			}
			out[k] = b
		}
	}
	if out["enableIntegrityMonitoring"] == true && out["enableVtpm"] != true {
		return nil, invalid("Invalid value for field '%s.enableIntegrityMonitoring': 'true'. Integrity monitoring requires the vTPM to be enabled.", field)
	}
	return out, nil
}

// setIntegrityPolicy is instances.setShieldedInstanceIntegrityPolicy:
// updateAutoLearnPolicy takes the most recent boot's measurements as the
// integrity policy baseline.
func (s *Server) setIntegrityPolicy(c call, scope, rel string, o obj) (obj, error) {
	if objOf(o["shieldedInstanceConfig"])["enableIntegrityMonitoring"] != true {
		return nil, invalid("Invalid resource usage: 'Integrity monitoring is not enabled on instance '%s'.'.", str(o, "name"))
	}
	if v, ok := c.body["updateAutoLearnPolicy"]; ok {
		if _, isBool := v.(bool); !isBool {
			return nil, invalid("Invalid value for field 'resource.updateAutoLearnPolicy': '%v'. Expected a boolean.", v)
		}
	}
	if c.body["updateAutoLearnPolicy"] == true {
		if ig := objOf(o["_integrity"]); ig != nil {
			ig["baselineEarly"] = objOf(ig["earlyBootReportEvent"])["actualMeasurements"]
			ig["baselineLate"] = objOf(ig["lateBootReportEvent"])["actualMeasurements"]
		} else {
			o["_integrity"] = obj{"learn": true}
		}
	}
	o["shieldedInstanceIntegrityPolicy"] = obj{"updateAutoLearnPolicy": c.body["updateAutoLearnPolicy"] == true}
	return s.done(c.project, scope, "setShieldedInstanceIntegrityPolicy", rel, o["id"])
}

// updateShieldedConfig is instances.updateShieldedInstanceConfig, which
// Google allows only on a stopped instance.
func (s *Server) updateShieldedConfig(c call, scope, rel string, o obj, status string) (obj, error) {
	if status != stTerminated {
		return nil, errf(400, "resourceNotReady",
			"The resource '%s' is not ready: the instance must be stopped (TERMINATED) to update its Shielded VM config, it is %s.", rel, status)
	}
	next, err := checkShieldedConfig("resource", c.body, objOf(o["shieldedInstanceConfig"]))
	if err != nil {
		return nil, err
	}
	o["shieldedInstanceConfig"] = next
	return s.done(c.project, scope, "updateShieldedInstanceConfig", rel, o["id"])
}
