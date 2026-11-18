package firestoreadmin

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

// channelEmulator is a WebChannel Write session's server side, as the
// emulator answers it: a handshake naming SID1, a backchannel that sends
// a stream token and stays open, and forward POSTs whose requests it keeps.
type channelEmulator struct {
	mu   sync.Mutex
	reqs []string // the reqI___data__ it was sent
}

func (e *channelEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Get("TYPE") == "xmlhttp":
		_, _ = io.WriteString(w, channelChunk(`[[1,[{"streamId":"1","streamToken":"MA=="}]]]`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case r.Method == http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		n, _ := strconv.Atoi(form.Get("count"))
		e.mu.Lock()
		for i := 0; i < n; i++ {
			e.reqs = append(e.reqs, form.Get("req"+strconv.Itoa(i)+"___data__"))
		}
		e.mu.Unlock()
		if q.Get("SID") == "" {
			_, _ = io.WriteString(w, channelChunk(`[[0,["c","SID1","",8,12,30000]]]`))
			return
		}
		_, _ = io.WriteString(w, channelChunk("[1,1,0]"))
	}
}

func (e *channelEmulator) sent() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.reqs...)
}

// TestEditionGuardWriteChannel (#1744): on a Standard database the guard
// passes a WebChannel Write session on, and a forward POST whose request
// carries an Enterprise-only value goes on with the requests before it
// alone, its refusal sent down the backchannel with the next array id, as
// Google's JSON error; an Enterprise database's channel is not claimed.
func TestEditionGuardWriteChannel(t *testing.T) {
	emu := &channelEmulator{}
	es := httptest.NewServer(emu)
	t.Cleanup(es.Close)
	a := New(strings.TrimPrefix(es.URL, "http://"))
	a.databases["projects/p/databases/(default)"] = &adminpb.Database{Name: "projects/p/databases/(default)", DatabaseEdition: adminpb.Database_STANDARD}
	a.databases["projects/p/databases/ent"] = &adminpb.Database{Name: "projects/p/databases/ent", DatabaseEdition: adminpb.Database_ENTERPRISE}
	g := a.EditionGuard()
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.Claims(r) {
			http.Error(w, "not claimed", http.StatusTeapot)
			return
		}
		g.ServeHTTP(w, r)
	}))
	t.Cleanup(fs.Close)

	channel := func(db string, params ...string) string {
		v := url.Values{"VER": {"8"}, "database": {"projects/p/databases/" + db}}
		for i := 0; i+1 < len(params); i += 2 {
			v.Set(params[i], params[i+1])
		}
		return fs.URL + writeChannelPath + "?" + v.Encode()
	}
	post := func(u string, reqs ...string) string {
		t.Helper()
		v := url.Values{"count": {strconv.Itoa(len(reqs))}, "ofs": {"0"}}
		for i, r := range reqs {
			v.Set("req"+strconv.Itoa(i)+"___data__", r)
		}
		res, err := http.Post(u, "application/x-www-form-urlencoded", strings.NewReader(v.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %d %s", u, res.StatusCode, b)
		}
		return string(b)
	}
	// chunks reads a backchannel's chunks as they come.
	backchannel := func(aid string) (<-chan string, func()) {
		t.Helper()
		res, err := http.Get(channel("(default)", "SID", "SID1", "RID", "rpc", "TYPE", "xmlhttp", "AID", aid, "CI", "0"))
		if err != nil {
			t.Fatal(err)
		}
		out := make(chan string, 8)
		go func() {
			defer close(out)
			br := bufio.NewReader(res.Body)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				n, _ := strconv.Atoi(strings.TrimSpace(line))
				b := make([]byte, n)
				if _, err := io.ReadFull(br, b); err != nil {
					return
				}
				out <- string(b)
			}
		}()
		return out, func() { res.Body.Close() }
	}
	next := func(c <-chan string) string {
		t.Helper()
		select {
		case s, ok := <-c:
			if !ok {
				return "<ended>"
			}
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("no chunk came down the backchannel")
			return ""
		}
	}

	const (
		plain  = `{"streamToken":"MA==","writes":[{"update":{"name":"projects/p/databases/(default)/documents/c/a","fields":{"v":{"integerValue":"1"}}}}]}`
		int32  = `{"streamToken":"MA==","writes":[{"update":{"name":"projects/p/databases/(default)/documents/c/b","fields":{"v":{"mapValue":{"fields":{"__int__":{"integerValue":"7"}}}}}}}]}`
		nested = `{"streamToken":"MA==","writes":[{"update":{"name":"projects/p/databases/(default)/documents/c/d","fields":{"v":{"arrayValue":{"values":[{"arrayValue":{}}]}}}}}]}`
		intErr = `[[2,[{"error":{"code":400,"message":"Value type INT is not allowed","status":"INVALID_ARGUMENT"}}]]]`
	)

	if got := post(channel("(default)", "RID", "1", "CVER", "22"), `{"database":"projects/p/databases/(default)"}`); !strings.Contains(got, `"SID1"`) {
		t.Fatalf("handshake answered %q", got)
	}
	down, closeDown := backchannel("0")
	if got := next(down); !strings.Contains(got, "streamToken") {
		t.Fatalf("backchannel's first chunk %q", got)
	}

	// A plain write goes on.
	post(channel("(default)", "SID", "SID1", "RID", "2", "AID", "1"), plain)
	// One with an int32 does not; its refusal ends the backchannel.
	if got := post(channel("(default)", "SID", "SID1", "RID", "3", "AID", "1"), int32); !strings.Contains(got, "[1,1,0]") {
		t.Errorf("refused POST answered %q, want [1,1,0]", got)
	}
	if got := next(down); got != intErr {
		t.Errorf("refusal %q, want %q", got, intErr)
	}
	if got := next(down); got != "<ended>" {
		t.Errorf("after the refusal: %q, want the backchannel ended", got)
	}
	closeDown()
	if got := emu.sent(); len(got) != 2 || got[1] != plain {
		t.Fatalf("the emulator was sent %q, want the handshake and the plain write", got)
	}

	// Batched: the request before the refused one goes on alone; the
	// refusal waits for the next backchannel.
	post(channel("(default)", "SID", "SID1", "RID", "4", "AID", "2"), plain, nested, plain)
	if got := emu.sent(); len(got) != 3 || got[2] != plain {
		t.Fatalf("the emulator was sent %q, want the batch's first request alone", got)
	}
	down, closeDown = backchannel("2")
	if got := next(down); !strings.Contains(got, `"message":"Nested arrays are not allowed","status":"INVALID_ARGUMENT"`) {
		t.Errorf("pending refusal %q", got)
	}
	closeDown()

	// An Enterprise database's channel is the emulator's.
	res, err := http.Post(channel("ent", "SID", "SID2", "RID", "2"), "text/plain", strings.NewReader("count=1&ofs=0&req0___data__="+url.QueryEscape(int32)))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTeapot {
		t.Errorf("an Enterprise database's channel was claimed: %d", res.StatusCode)
	}
}
