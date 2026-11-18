package resourcemanager

// A PKCS#12 key file (#1750): what CreateServiceAccountKey returns for
// TYPE_PKCS12_FILE, "PKCS12 format. The password for the PKCS12 file is
// notasecret." (the projects.serviceAccounts.keys REST reference, read
// 2026-11-17), as RFC 7292 lays it out: a PFX whose authenticated safe
// holds the private key in a pkcs8ShroudedKeyBag (PBE-SHA1-3DES), named
// "privatekey", and its self-signed certificate in a certBag, with a
// SHA-1 HMAC over both.

import (
	"crypto/cipher"
	"crypto/des"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/asn1"
	"math/big"
	"unicode/utf16"
)

const pkcs12Password = "notasecret"

var (
	oidData           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidShroudedKeyBag = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 2}
	oidCertBag        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 10, 1, 3}
	oidX509Cert       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 22, 1}
	oidFriendlyName   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 20}
	oidLocalKeyID     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 21}
	oidPBESHA13DES    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 12, 1, 3}
	oidSHA1           = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
)

const pkcs12Iterations = 2048

type p12AlgID struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type p12PBEParams struct {
	Salt       []byte
	Iterations int
}

type p12EncryptedKey struct {
	Algorithm p12AlgID
	Data      []byte
}

type p12Attribute struct {
	ID     asn1.ObjectIdentifier
	Values asn1.RawValue
}

type p12SafeBag struct {
	ID         asn1.ObjectIdentifier
	Value      asn1.RawValue
	Attributes []p12Attribute `asn1:"set"`
}

type p12CertBag struct {
	ID   asn1.ObjectIdentifier
	Data asn1.RawValue
}

type p12ContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue
}

type p12DigestInfo struct {
	Algorithm p12AlgID
	Digest    []byte
}

type p12MacData struct {
	Mac        p12DigestInfo
	Salt       []byte
	Iterations int
}

type p12PFX struct {
	Version  int
	AuthSafe p12ContentInfo
	MacData  p12MacData
}

// bmpString is a password or name as RFC 7292 encodes it: UTF-16BE.
func bmpString(s string) []byte {
	var out []byte
	for _, r := range utf16.Encode([]rune(s)) {
		out = append(out, byte(r>>8), byte(r))
	}
	return out
}

// p12KDF is RFC 7292 appendix B.2's key derivation with SHA-1.
func p12KDF(password, salt []byte, id byte, iterations, size int) []byte {
	const u, v = 20, 64
	pass := append(append([]byte{}, password...), 0, 0)
	d := make([]byte, v)
	for i := range d {
		d[i] = id
	}
	fill := func(b []byte) []byte {
		if len(b) == 0 {
			return nil
		}
		n := v * ((len(b) + v - 1) / v)
		out := make([]byte, n)
		for i := range out {
			out[i] = b[i%len(b)]
		}
		return out
	}
	in := append(fill(salt), fill(pass)...)
	var out []byte
	for len(out) < size {
		h := sha1.New()
		h.Write(d)
		h.Write(in)
		a := h.Sum(nil)
		for j := 1; j < iterations; j++ {
			s := sha1.Sum(a)
			a = s[:]
		}
		out = append(out, a...)
		b := new(big.Int).SetBytes(fill(a)[:v])
		one := big.NewInt(1)
		mod := new(big.Int).Lsh(one, v*8)
		for k := 0; k < len(in); k += v {
			x := new(big.Int).SetBytes(in[k : k+v])
			x.Add(x, b).Add(x, one).Mod(x, mod)
			xb := x.Bytes()
			blk := in[k : k+v]
			for i := range blk {
				blk[i] = 0
			}
			copy(blk[v-len(xb):], xb)
		}
	}
	return out[:size]
}

func explicit0(der []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der}
}

func octets(b []byte) []byte {
	der, _ := asn1.Marshal(b)
	return der
}

func p12Set(v asn1.RawValue) asn1.RawValue {
	der, _ := asn1.Marshal(v)
	return asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSet, IsCompound: true, Bytes: der}
}

// encodePKCS12 is a PFX of a PKCS#8 private key and its certificate.
func encodePKCS12(pkcs8, certDER []byte) ([]byte, error) {
	pass := bmpString(pkcs12Password)
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key := p12KDF(pass, salt, 1, pkcs12Iterations, 24)
	iv := p12KDF(pass, salt, 2, pkcs12Iterations, 8)
	block, err := des.NewTripleDESCipher(key)
	if err != nil {
		return nil, err
	}
	pad := 8 - len(pkcs8)%8
	plain := append(append([]byte{}, pkcs8...), make([]byte, pad)...)
	for i := len(pkcs8); i < len(plain); i++ {
		plain[i] = byte(pad)
	}
	enc := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, plain)
	params, err := asn1.Marshal(p12PBEParams{Salt: salt, Iterations: pkcs12Iterations})
	if err != nil {
		return nil, err
	}
	shrouded, err := asn1.Marshal(p12EncryptedKey{Algorithm: p12AlgID{Algorithm: oidPBESHA13DES, Parameters: asn1.RawValue{FullBytes: params}}, Data: enc})
	if err != nil {
		return nil, err
	}
	localID := sha1.Sum(certDER)
	attrs := []p12Attribute{
		{ID: oidFriendlyName, Values: p12Set(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagBMPString, Bytes: bmpString("privatekey")})},
		{ID: oidLocalKeyID, Values: p12Set(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagOctetString, Bytes: localID[:]})},
	}
	certBag, err := asn1.Marshal(p12CertBag{ID: oidX509Cert, Data: explicit0(octets(certDER))})
	if err != nil {
		return nil, err
	}
	keyContents, err := asn1.Marshal([]p12SafeBag{{ID: oidShroudedKeyBag, Value: explicit0(shrouded), Attributes: attrs}})
	if err != nil {
		return nil, err
	}
	certContents, err := asn1.Marshal([]p12SafeBag{{ID: oidCertBag, Value: explicit0(certBag), Attributes: attrs}})
	if err != nil {
		return nil, err
	}
	authSafe, err := asn1.Marshal([]p12ContentInfo{
		{ContentType: oidData, Content: explicit0(octets(certContents))},
		{ContentType: oidData, Content: explicit0(octets(keyContents))},
	})
	if err != nil {
		return nil, err
	}
	macSalt := make([]byte, 8)
	if _, err := rand.Read(macSalt); err != nil {
		return nil, err
	}
	mac := hmac.New(sha1.New, p12KDF(pass, macSalt, 3, pkcs12Iterations, 20))
	mac.Write(authSafe)
	return asn1.Marshal(p12PFX{Version: 3,
		AuthSafe: p12ContentInfo{ContentType: oidData, Content: explicit0(octets(authSafe))},
		MacData: p12MacData{Mac: p12DigestInfo{Algorithm: p12AlgID{Algorithm: oidSHA1, Parameters: asn1.NullRawValue}, Digest: mac.Sum(nil)},
			Salt: macSalt, Iterations: pkcs12Iterations}})
}
