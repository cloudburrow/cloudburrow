package kms

import (
	"encoding/base64"
	"hash/crc32"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// gcloud kms encrypt, with integrity verification on, sends
// additionalAuthenticatedDataCrc32c = crc32c("") = 0 even without AAD, and
// fails unless the response says it was verified (#427). A supplied 0 is
// present (#411), over JSON as over gRPC.
func TestRESTEncryptVerifiesAZeroAADChecksum(t *testing.T) {
	h := httptest.NewServer(NewRESTHandler(NewServer(store.NewMemory())))
	defer h.Close()
	base := h.URL + "/v1/" + loc
	restDo(t, "POST", base+"/keyRings?keyRingId=r", "")
	restDo(t, "POST", base+"/keyRings/r/cryptoKeys?cryptoKeyId=k", `{"purpose":"ENCRYPT_DECRYPT"}`)
	pt := []byte("gcloud-shaped")
	crc := strconv.FormatUint(uint64(crc32.Checksum(pt, crc32.MakeTable(crc32.Castagnoli))), 10)
	body := `{"plaintext":"` + base64.StdEncoding.EncodeToString(pt) + `","plaintextCrc32c":"` + crc + `","additionalAuthenticatedDataCrc32c":"0"}`
	code, m, raw := restDo(t, "POST", base+"/keyRings/r/cryptoKeys/k:encrypt?alt=json", body)
	if code != 200 || m["verifiedPlaintextCrc32c"] != true || m["verifiedAdditionalAuthenticatedDataCrc32c"] != true || m["ciphertextCrc32c"] == nil {
		t.Fatalf("gcloud-shaped encrypt = %d %s", code, raw)
	}
}
