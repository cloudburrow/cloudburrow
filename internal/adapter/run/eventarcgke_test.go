package run

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	datastorepb "cloud.google.com/go/datastore/apiv1/datastorepb"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A GKE destination's fields are checked and defaulted, its cluster must
// be one this instance serves, and a delivery reaches the service's port
// 80 through the cluster's API server proxy with the client certificate.
func TestEventarcGKEDestination(t *testing.T) {
	var path, ceType, body string
	var sawCert bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, ceType, body = r.URL.Path, r.Header.Get("Ce-Type"), string(b)
		sawCert = len(r.TLS.PeerCertificates) == 1
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes-admin"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	b64 := func(typ string, der []byte) string {
		return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
	}
	access := GKEAccess{Endpoint: strings.TrimPrefix(srv.URL, "https://"), CACert: b64("CERTIFICATE", srv.Certificate().Raw),
		ClientCert: b64("CERTIFICATE", der), ClientKey: b64("EC PRIVATE KEY", keyDER)}
	cluster := "projects/gkep/locations/us-central1/clusters/events"
	f := (&FunctionsServer{}).WithEvents(Events{Transport: &fakeTransport{}, GKE: func(name string) (GKEAccess, bool) {
		return access, name == cluster
	}})
	typ := []*eventarcpb.EventFilter{{Attribute: "type", Value: pubsubPublished}}
	for name, c := range map[string]struct {
		gke  *eventarcpb.GKE
		code codes.Code
	}{
		"no cluster":          {&eventarcpb.GKE{Service: "s"}, codes.InvalidArgument},
		"no service":          {&eventarcpb.GKE{Cluster: "events"}, codes.InvalidArgument},
		"an unknown cluster":  {&eventarcpb.GKE{Cluster: "other", Service: "s"}, codes.InvalidArgument},
		"a cluster elsewhere": {&eventarcpb.GKE{Cluster: "events", Location: "europe-west1", Service: "s"}, codes.InvalidArgument},
		"the cluster":         {&eventarcpb.GKE{Cluster: "events", Service: "s"}, codes.OK},
	} {
		tr := &eventarcpb.Trigger{EventFilters: typ, Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_Gke{Gke: c.gke}}}
		eaDestinationDefaults(tr, "us-central1")
		_, err := eaSpec(tr, "us-central1")
		if err == nil {
			err = f.checkGKEDestination("gkep", tr)
		}
		got := codes.OK
		if err != nil {
			got = apierror.From(err).Code
		}
		if got != c.code {
			t.Errorf("%s: %v; want %v", name, err, c.code)
		}
		if c.code == codes.OK && (tr.GetDestination().GetGke().GetNamespace() != "default" || tr.GetDestination().GetGke().GetLocation() != "us-central1") {
			t.Errorf("defaults = %v", tr.GetDestination().GetGke())
		}
	}
	tr := &eventarcpb.Trigger{Name: "projects/gkep/locations/us-central1/triggers/t", Destination: &eventarcpb.Destination{
		Descriptor_: &eventarcpb.Destination_Gke{Gke: &eventarcpb.GKE{Cluster: "events", Location: "us-central1", Namespace: "apps", Service: "probe", Path: "route/sub"}}}}
	spec := &functionspb.EventTrigger{EventType: pubsubPublished, PubsubTopic: "projects/gkep/topics/t"}
	code, err := f.deliverEventarc(context.Background(), tr, spec, []byte(`{"message":{"data":"aGk=","messageId":"1"},"subscription":"s"}`))
	if err != nil || code != http.StatusNoContent || path != "/api/v1/namespaces/apps/services/probe:80/proxy/route/sub" ||
		ceType != pubsubPublished || !sawCert || !strings.Contains(body, `"aGk="`) {
		t.Errorf("delivery = %d, %v; path %q type %q cert %v body %s", code, err, path, ceType, sawCert, body)
	}
}

// A Datastore mode entity change reaches the triggers it matches as
// EntityEventData in protobuf, with the entity's extensions.
func TestDatastoreEventsReachMatchingTriggers(t *testing.T) {
	pub := &fakePublisher{}
	f := (&FunctionsServer{}).WithEvents(Events{Transport: pub})
	add := func(id, typ string, filters ...*functionspb.EventFilter) {
		name := "projects/dsp/locations/us-central1/triggers/" + id
		eaState.mu.Lock()
		eaState.triggers[name] = &eaTrigger{t: &eventarcpb.Trigger{Name: name}, live: &liveTrigger{
			spec: &functionspb.EventTrigger{EventType: typ, EventFilters: filters}, topic: "projects/dsp/topics/" + id}}
		eaState.mu.Unlock()
		t.Cleanup(func() { eaState.mu.Lock(); delete(eaState.triggers, name); eaState.mu.Unlock() })
	}
	db := &functionspb.EventFilter{Attribute: "database", Value: "(default)"}
	add("upd", datastorePrefix+"updated", db, &functionspb.EventFilter{Attribute: "entity", Value: "users/{u}", Operator: pathPatternOp})
	add("all", datastorePrefix+"written.withAuthContext", db)
	add("ns", datastorePrefix+"written", db, &functionspb.EventFilter{Attribute: "namespace", Value: "other"})
	add("posts", datastorePrefix+"updated", db, &functionspb.EventFilter{Attribute: "entity", Value: "posts/{p}", Operator: pathPatternOp})
	if w := DatastoreWatches(); len(w) != 1 || w[0] != (DatastoreWatch{"dsp", "(default)"}) {
		t.Errorf("DatastoreWatches = %v", w)
	}
	key := &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: "dsp"}, Path: []*datastorepb.Key_PathElement{
		{Kind: "users", IdType: &datastorepb.Key_PathElement_Name{Name: "alice"}}}}
	val := func(n int64) *datastorepb.Value {
		return &datastorepb.Value{ValueType: &datastorepb.Value_IntegerValue{IntegerValue: n}}
	}
	old := &datastorepb.EntityResult{Version: 1, Entity: &datastorepb.Entity{Key: key, Properties: map[string]*datastorepb.Value{"a": val(1), "b": val(1)}}}
	cur := &datastorepb.EntityResult{Version: 2, Entity: &datastorepb.Entity{Key: key, Properties: map[string]*datastorepb.Value{"a": val(2), "b": val(1)}}}
	n := f.Eventarc().EmitDatastore(context.Background(), DatastoreChange{Project: "dsp", Database: "(default)", Old: old, New: cur})
	if n != 2 || strings.Join(pub.topics, ",") != "projects/dsp/topics/upd,projects/dsp/topics/all" {
		t.Fatalf("EmitDatastore = %d to %v", n, pub.topics)
	}
	for k, want := range map[string]string{"ce-source": "//firestore.googleapis.com/projects/dsp/databases/(default)",
		"ce-entity": "users/alice", "ce-namespace": "(default)", "ce-database": "(default)", "ce-project": "dsp",
		"ce-authtype": "unauthenticated", "content-type": "application/protobuf"} {
		if pub.attrs[k] != want {
			t.Errorf("%s = %q; want %q", k, pub.attrs[k], want)
		}
	}
	var gotNew, gotOld datastorepb.EntityResult
	var mask []string
	for b := pub.data; len(b) > 0; {
		num, _, n := protowire.ConsumeTag(b)
		b = b[n:]
		v, n := protowire.ConsumeBytes(b)
		b = b[n:]
		switch num {
		case 1:
			_ = proto.Unmarshal(v, &gotNew)
		case 2:
			_ = proto.Unmarshal(v, &gotOld)
		case 3:
			for len(v) > 0 {
				_, _, n := protowire.ConsumeTag(v)
				v = v[n:]
				s, n := protowire.ConsumeString(v)
				v = v[n:]
				mask = append(mask, s)
			}
		}
	}
	if !proto.Equal(&gotNew, cur) || !proto.Equal(&gotOld, old) || strings.Join(mask, ",") != "a" {
		t.Errorf("EntityEventData = value %v, old %v, mask %v", &gotNew, &gotOld, mask)
	}
	if got := eaContentType(datastorePrefix + "created"); got != "application/protobuf" {
		t.Errorf("content type %q", got)
	}
}
