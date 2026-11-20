//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	connection "cloud.google.com/go/bigquery/connection/apiv1"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/storage"
)

// objRefPNG is a small PNG image.
func objRefPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: 120, B: uint8(y * 16), A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// objRefFixture is a bucket holding an image, a CLOUD_RESOURCE
// connection, and the grants an ObjectRef of it needs.
type objRefFixture struct {
	bh      *storage.BucketHandle
	conn    string // project.us.id
	short   string // us.id
	sa      string
	project string
	png     []byte
}

// grantObjectRefs gives the caller bigquery.objectRefs.read/write
// (BigQuery ObjectRef Admin) and bigquery.connections.use on cn, keeping
// its other bindings.
func grantObjectRefs(t *testing.T, h *Harness, cc *connection.Client, project string, cn *connectionpb.Connection) {
	t.Helper()
	ctx := h.Context()
	pol, err := cc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: cn.Name})
	if err != nil {
		t.Fatalf("GetIamPolicy on the connection: %v", err)
	}
	for _, role := range []string{"roles/bigquery.objectRefAdmin", "roles/bigquery.connectionUser"} {
		pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{callerMember(project)}})
	}
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: pol}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
}

// grantBucket gives sa role on bh.
func grantBucket(t *testing.T, h *Harness, bh *storage.BucketHandle, sa, role string) {
	t.Helper()
	pol, err := bh.IAM().Policy(h.Context())
	if err != nil {
		t.Fatalf("bucket IAM Policy: %v", err)
	}
	pol.Add("serviceAccount:"+sa, iam.RoleName(role))
	if err := bh.IAM().SetPolicy(h.Context(), pol); err != nil {
		t.Fatalf("bucket IAM SetPolicy: %v", err)
	}
}

func objRefBucket(t *testing.T, h *Harness, suffix string) (*storage.BucketHandle, []byte) {
	t.Helper()
	ctx := h.Context()
	sc := storageClient(t, h)
	bh := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-" + suffix)
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	img := objRefPNG(t)
	w := bh.Object("img/pixel.png").NewWriter(ctx)
	w.ContentType = "image/png"
	if _, err := w.Write(img); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write the image: %v", err)
	}
	return bh, img
}

// TestBigQueryObjectRef (#1980): ObjectRefs and the OBJ functions over the
// instance's Cloud Storage, through the official cloud.google.com/go
// bigquery, storage and bigquery/connection/apiv1 clients:
// OBJ.MAKE_REF (uri and authorizer, region.connection written in its
// project form, the JSON form, and a new authorizer for a ref);
// OBJ.FETCH_METADATA of one ref and of an ARRAY, failing in details
// (errors, source OBJ.FETCH_METADATA) until the caller holds
// bigquery.objectRefs.read on the connection and its service account
// storage.objects.get on the bucket, then filling version (the
// generation) and details.gcs_metadata; direct access without an
// authorizer; OBJ.GET_ACCESS_URL 'r' with INTERVAL 45 MINUTE, whose
// read_url plain HTTP downloads the object through (X-Goog-Expires 2700,
// expiry_time 45 minutes on), and which Cloud Storage refuses when
// tampered with; 'rw', whose write_url a plain HTTP PUT writes a new
// object through; the documented errors without delegated access and the
// duration's bounds; OBJ.GET_READ_URL; an object table's ref column into
// OBJ.GET_ACCESS_URL; a mode and a duration that are columns (#1983);
// and OBJ.LIST, the listing as a table, with and without an authorizer,
// its refs into OBJ.GET_ACCESS_URL, and the documented bad URIs refused.
//
// covers: bigquery.jobs.query
func TestBigQueryObjectRef(t *testing.T) {
	h := New(t)
	gcsEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_oref"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	sa := cn.GetCloudResource().GetServiceAccountId()
	conn := project + ".us." + id
	bh, img := objRefBucket(t, h, "oref")
	uri := "gs://" + bh.BucketName() + "/img/pixel.png"
	attrs, err := bh.Object("img/pixel.png").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	one := func(sql string) bigquery.Value {
		t.Helper()
		rows := mlQuery(t, h, c, sql)
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows", sql, len(rows))
		}
		for _, v := range rows[0] {
			return v
		}
		return nil
	}
	str := func(sql string) string {
		t.Helper()
		v := one(sql)
		s, _ := v.(string)
		return s
	}

	// OBJ.MAKE_REF: the STRUCT, its authorizer in project form.
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT r.uri, r.version, r.authorizer, r.details IS NULL AS no_details FROM (SELECT OBJ.MAKE_REF('%s', 'us.%s') AS r)", uri, id))
	if len(rows) != 1 || rows[0]["uri"] != uri || rows[0]["version"] != nil || rows[0]["authorizer"] != conn || rows[0]["no_details"] != true {
		t.Fatalf("OBJ.MAKE_REF: %v", rows)
	}
	if got := str(fmt.Sprintf(`SELECT OBJ.MAKE_REF(JSON '{"uri": "%s", "authorizer": "us.%s"}').authorizer`, uri, id)); got != conn {
		t.Fatalf("OBJ.MAKE_REF(json).authorizer = %q", got)
	}
	if got := str(fmt.Sprintf("SELECT OBJ.MAKE_REF(OBJ.MAKE_REF('%s'), '%s').authorizer", uri, conn)); got != conn {
		t.Fatalf("OBJ.MAKE_REF(ref, authorizer).authorizer = %q", got)
	}
	if err := mlRun(h, c, "SELECT OBJ.MAKE_REF('not-a-uri')"); err == nil || !strings.Contains(err.Error(), "gs://") {
		t.Fatalf("OBJ.MAKE_REF of a bad uri: %v", err)
	}

	fetch := fmt.Sprintf("SELECT TO_JSON_STRING(OBJ.FETCH_METADATA(OBJ.MAKE_REF('%s', '%s')))", uri, conn)
	// Without bigquery.objectRefs.read on the connection, details holds the error.
	if got := str(fetch); !strings.Contains(got, "cannot be used") || !strings.Contains(got, "OBJ.FETCH_METADATA") || !strings.Contains(got, `"errors"`) {
		t.Fatalf("FETCH_METADATA without objectRefs.read: %s", got)
	}
	grantObjectRefs(t, h, cc, project, cn)
	// The service account cannot read the bucket yet.
	if got := str(fetch); !strings.Contains(got, "storage.objects.get") || !strings.Contains(got, sa) {
		t.Fatalf("FETCH_METADATA before the bucket grant: %s", got)
	}
	grantBucket(t, h, bh, sa, "roles/storage.objectUser")
	var ref struct {
		URI        string `json:"uri"`
		Version    string `json:"version"`
		Authorizer string `json:"authorizer"`
		Details    struct {
			GCS struct {
				ContentType string `json:"content_type"`
				Size        int64  `json:"size"`
				MD5         string `json:"md5_hash"`
				Updated     int64  `json:"updated"`
			} `json:"gcs_metadata"`
		} `json:"details"`
	}
	if err := json.Unmarshal([]byte(str(fetch)), &ref); err != nil {
		t.Fatal(err)
	}
	if ref.URI != uri || ref.Version != fmt.Sprint(attrs.Generation) || ref.Authorizer != conn || ref.Details.GCS.ContentType != "image/png" ||
		ref.Details.GCS.Size != int64(len(img)) || len(ref.Details.GCS.MD5) != 32 || ref.Details.GCS.Updated == 0 {
		t.Fatalf("FETCH_METADATA: %+v (generation %d)", ref, attrs.Generation)
	}
	// The ARRAY form, and direct access (no authorizer).
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT ARRAY_LENGTH(m) AS n, JSON_VALUE(m[OFFSET(1)].details.gcs_metadata.content_type) AS ct, m[OFFSET(1)].authorizer AS a "+
		"FROM (SELECT OBJ.FETCH_METADATA([OBJ.MAKE_REF('%s', '%s'), OBJ.MAKE_REF('%s')]) AS m)", uri, conn, uri))
	if len(rows) != 1 || rows[0]["n"] != int64(2) || rows[0]["ct"] != "image/png" || rows[0]["a"] != nil {
		t.Fatalf("FETCH_METADATA of an ARRAY: %v", rows)
	}
	if got := str(fmt.Sprintf("SELECT TO_JSON_STRING(OBJ.FETCH_METADATA(OBJ.MAKE_REF('gs://%s/img/missing.png', '%s')))", bh.BucketName(), conn)); !strings.Contains(got, "404") {
		t.Fatalf("FETCH_METADATA of a missing object: %s", got)
	}

	// OBJ.GET_ACCESS_URL 'r': a read URL plain HTTP downloads through.
	gcs, err := url.Parse(gcsEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	// expiry_time is by the instance's BigQuery clock, which other tests in
	// the shard move with clock:advance.
	before := advanceBigQueryClock(t, h, 0)
	var rt struct {
		ObjRef struct {
			URI        string `json:"uri"`
			Authorizer string `json:"authorizer"`
		} `json:"obj_ref"`
		AccessURLs struct {
			ReadURL    string `json:"read_url"`
			WriteURL   string `json:"write_url"`
			ExpiryTime string `json:"expiry_time"`
		} `json:"access_urls"`
	}
	raw := str(fmt.Sprintf("SELECT TO_JSON_STRING(OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s', '%s'), 'r', INTERVAL 45 MINUTE))", uri, conn))
	if err := json.Unmarshal([]byte(raw), &rt); err != nil {
		t.Fatalf("GET_ACCESS_URL: %v: %s", err, raw)
	}
	if rt.ObjRef.URI != uri || rt.ObjRef.Authorizer != conn || rt.AccessURLs.ReadURL == "" || rt.AccessURLs.WriteURL != "" {
		t.Fatalf("GET_ACCESS_URL 'r': %s", raw)
	}
	exp, err := time.Parse("2006-01-02T15:04:05Z", rt.AccessURLs.ExpiryTime)
	if err != nil || exp.Before(before.Add(44*time.Minute)) || exp.After(advanceBigQueryClock(t, h, 0).Add(46*time.Minute)) {
		t.Fatalf("expiry_time %q: %v", rt.AccessURLs.ExpiryTime, err)
	}
	ru, err := url.Parse(rt.AccessURLs.ReadURL)
	if err != nil || ru.Host != gcs.Host || ru.Query().Get("X-Goog-Expires") != "2700" ||
		!strings.HasPrefix(ru.Query().Get("X-Goog-Credential"), sa+"/") {
		t.Fatalf("read_url %s (Cloud Storage at %s): %v", rt.AccessURLs.ReadURL, gcs.Host, err)
	}
	get := func(u string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if code, b := get(rt.AccessURLs.ReadURL); code != http.StatusOK || !bytes.Equal(b, img) {
		t.Fatalf("GET read_url: %d, %d bytes", code, len(b))
	}
	if code, _ := get(strings.Replace(rt.AccessURLs.ReadURL, "X-Goog-Expires=2700", "X-Goog-Expires=2701", 1)); code != http.StatusForbidden {
		t.Fatalf("GET of a tampered read_url: %d, want 403", code)
	}

	// 'rw' over an object not yet written: plain HTTP PUT writes it.
	newURI := "gs://" + bh.BucketName() + "/img/uploaded.txt"
	raw = str(fmt.Sprintf("SELECT TO_JSON_STRING(OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s', '%s'), 'rw'))", newURI, conn))
	if err := json.Unmarshal([]byte(raw), &rt); err != nil || rt.AccessURLs.WriteURL == "" {
		t.Fatalf("GET_ACCESS_URL 'rw': %v: %s", err, raw)
	}
	if wu, _ := url.Parse(rt.AccessURLs.WriteURL); wu.Query().Get("X-Goog-Expires") != "21600" {
		t.Fatalf("write_url's expiry is not the 6-hour default: %s", rt.AccessURLs.WriteURL)
	}
	req, _ := http.NewRequest(http.MethodPut, rt.AccessURLs.WriteURL, strings.NewReader("written through a signed URL"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT write_url: %d", resp.StatusCode)
	}
	r, err := sc.Bucket(bh.BucketName()).Object("img/uploaded.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("read the object written through write_url: %v", err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "written through a signed URL" {
		t.Fatalf("object written through write_url: %q", b)
	}
	// Without delegated access: the documented error shape.
	if got := str(fmt.Sprintf("SELECT TO_JSON_STRING(OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s'), 'r'))", uri)); !strings.Contains(got, `"errors"`) ||
		!strings.Contains(got, "OBJ.GET_ACCESS_URL") || !strings.Contains(got, `"objectref"`) {
		t.Fatalf("GET_ACCESS_URL without an authorizer: %s", got)
	}
	for _, d := range []string{"INTERVAL 10 MINUTE", "INTERVAL 7 HOUR"} {
		if err := mlRun(h, c, fmt.Sprintf("SELECT OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s', '%s'), 'r', %s)", uri, conn, d)); err == nil ||
			!strings.Contains(err.Error(), "30 minutes and 6 hours") {
			t.Fatalf("GET_ACCESS_URL with %s: %v", d, err)
		}
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s', '%s'), 'w')", uri, conn)); err == nil {
		t.Fatal("GET_ACCESS_URL mode 'w' was not refused")
	}

	// OBJ.GET_READ_URL.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT u.url, u.status FROM (SELECT OBJ.GET_READ_URL(OBJ.MAKE_REF('%s', '%s')) AS u)", uri, conn))
	if len(rows) != 1 || rows[0]["status"] != nil {
		t.Fatalf("GET_READ_URL: %v", rows)
	}
	if code, b := get(rows[0]["url"].(string)); code != http.StatusOK || !bytes.Equal(b, img) || !strings.Contains(rows[0]["url"].(string), "X-Goog-Expires=2700") {
		t.Fatalf("GET of GET_READ_URL's url: %d, %d bytes", code, len(b))
	}

	// An object table's ref column.
	ds := "compat_oref_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	if err := mlRun(h, c, fmt.Sprintf("CREATE EXTERNAL TABLE `%s.objs` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/img/*.png'])",
		ds, conn, bh.BucketName())); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE: %v", err)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT uri, JSON_VALUE(OBJ.GET_ACCESS_URL(ref, 'r'), '$.access_urls.read_url') AS u FROM `%s.objs`", ds))
	if len(rows) != 1 || rows[0]["uri"] != uri {
		t.Fatalf("GET_ACCESS_URL of an object table's ref: %v", rows)
	}
	if code, b := get(rows[0]["u"].(string)); code != http.StatusOK || !bytes.Equal(b, img) {
		t.Fatalf("GET of the object table ref's read_url: %d", code)
	}

	// OBJ.GET_ACCESS_URL with a mode and a duration that are columns
	// (#1983): each row's own.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT m, JSON_VALUE(u, '$.access_urls.read_url') AS r, JSON_VALUE(u, '$.access_urls.write_url') AS w FROM ("+
		"SELECT m, OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s', '%s'), m, INTERVAL d MINUTE) AS u FROM UNNEST([STRUCT('r' AS m, 45 AS d), ('rw', 90)])) ORDER BY m", uri, conn))
	if len(rows) != 2 || rows[0]["w"] != nil || rows[1]["w"] == nil || !strings.Contains(fmt.Sprint(rows[0]["r"]), "X-Goog-Expires=2700") ||
		!strings.Contains(fmt.Sprint(rows[1]["w"]), "X-Goog-Expires=5400") {
		t.Fatalf("GET_ACCESS_URL with column arguments: %v", rows)
	}
	if code, b := get(rows[0]["r"].(string)); code != http.StatusOK || !bytes.Equal(b, img) {
		t.Fatalf("GET of a per-row read_url: %d", code)
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s', '%s'), m) FROM UNNEST(['r', 'x']) AS m", uri, conn)); err == nil ||
		!strings.Contains(err.Error(), "'r' or 'rw'") {
		t.Fatalf("GET_ACCESS_URL with a column mode 'x': %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("SELECT OBJ.GET_ACCESS_URL(OBJ.MAKE_REF('%s', '%s'), 'r', INTERVAL n MINUTE) FROM UNNEST([10]) AS n", uri, conn)); err == nil ||
		!strings.Contains(err.Error(), "30 minutes and 6 hours") {
		t.Fatalf("GET_ACCESS_URL with a column duration of 10 minutes: %v", err)
	}

	// OBJ.LIST (#1983): the listing as a table, direct access and delegated.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT uri, content_type, size, md5_hash, updated, metadata, generation, ref.uri AS ref_uri, ref.authorizer AS a "+
		"FROM OBJ.LIST('gs://%s/img/*.png')", bh.BucketName()))
	if len(rows) != 1 || rows[0]["uri"] != uri || rows[0]["content_type"] != "image/png" || rows[0]["size"] != int64(len(img)) ||
		rows[0]["generation"] != attrs.Generation || rows[0]["ref_uri"] != uri || rows[0]["a"] != nil || rows[0]["md5_hash"] == nil || rows[0]["updated"] == nil {
		t.Fatalf("OBJ.LIST of *.png: %v", rows)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT uri, ref.authorizer AS a, JSON_VALUE(OBJ.GET_ACCESS_URL(ref, 'r'), '$.access_urls.read_url') AS u "+
		"FROM OBJ.LIST('gs://%s/img/*', 'us.%s') ORDER BY uri", bh.BucketName(), id))
	if len(rows) != 2 || rows[0]["uri"] != uri || rows[1]["uri"] != newURI || rows[0]["a"] != conn {
		t.Fatalf("OBJ.LIST with an authorizer: %v", rows)
	}
	if code, b := get(rows[0]["u"].(string)); code != http.StatusOK || !bytes.Equal(b, img) {
		t.Fatalf("GET of an OBJ.LIST ref's read_url: %d", code)
	}
	if got := one(fmt.Sprintf("SELECT COUNT(*) FROM OBJ.LIST('gs://%s/none/*')", bh.BucketName())); got != int64(0) {
		t.Fatalf("OBJ.LIST of nothing: %v", got)
	}
	for _, bad := range []string{"'gs://%s/img/'", "'gs://%s//img/pixel.png'", "'gs://%s/*/*.png'", "CONCAT('gs://', '%s', '/img/*')"} {
		if err := mlRun(h, c, "SELECT * FROM OBJ.LIST("+fmt.Sprintf(bad, bh.BucketName())+")"); err == nil || !strings.Contains(err.Error(), "OBJ.LIST") {
			t.Fatalf("OBJ.LIST(%s): %v", bad, err)
		}
	}
}

// TestBigQueryObjectRefAIFunctions (#1980): ObjectRef inputs to the AI
// functions on the instance's local Vertex AI, through the official
// cloud.google.com/go bigquery, storage and connection clients:
// AI.GENERATE_EMBEDDING over OBJ.MAKE_REF of a PNG (the documented
// query form) and over an object table (TABLE), each row embedded by the
// local embedder when its model embeds images, or else answered with a
// status naming why (the community EmbeddingGemma embeds text only);
// with the local generator, AI.GENERATE of a prompt STRUCT holding the
// image ref. The connection's service account needs storage access to
// the object first: before the bucket grant the row's status names
// storage.objects.get.
//
// covers: bigquery.jobs.query
func TestBigQueryObjectRefAIFunctions(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvEmbeddings)
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	c, _ := bigqueryClient(t, h)
	cc, project := remoteConnectionClient(t, h)
	conn, ds := remoteModelSetup(t, h, c, "orefai")
	ctx := h.Context()
	cn, err := cc.GetConnection(ctx, &connectionpb.GetConnectionRequest{Name: "projects/" + project + "/locations/us/connections/" + strings.Split(conn, ".")[2]})
	if err != nil {
		t.Fatalf("GetConnection: %v", err)
	}
	grantObjectRefs(t, h, cc, project, cn)
	sa := cn.GetCloudResource().GetServiceAccountId()
	bh, _ := objRefBucket(t, h, "orefai")
	uri := "gs://" + bh.BucketName() + "/img/pixel.png"
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.emb` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = 'embeddinggemma-300m-onnx-community')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}
	embed := fmt.Sprintf("SELECT ARRAY_LENGTH(embedding) AS n, status FROM AI.GENERATE_EMBEDDING(MODEL `%s.emb`, (SELECT OBJ.MAKE_REF('%s', '%s') AS content))", ds, uri, conn)
	rows := mlQuery(t, h, c, embed)
	if len(rows) != 1 || !strings.Contains(fmt.Sprint(rows[0]["status"]), "storage.objects.get") {
		t.Fatalf("AI.GENERATE_EMBEDDING before the bucket grant: %v", rows)
	}
	grantBucket(t, h, bh, sa, "roles/storage.objectViewer")
	check := func(what string, rows []map[string]bigquery.Value) {
		t.Helper()
		if len(rows) != 1 {
			t.Fatalf("%s: %v", what, rows)
		}
		status, _ := rows[0]["status"].(string)
		if status == "" {
			if n, _ := rows[0]["n"].(int64); n == 0 {
				t.Fatalf("%s: no embedding and no status: %v", what, rows)
			}
			return
		}
		// The local embedder's model reads no images: its refusal is the
		// row's status, naming the part it cannot embed.
		if rows[0]["n"] != nil || !strings.Contains(strings.ToLower(status), "image") && !strings.Contains(strings.ToLower(status), "text") {
			t.Fatalf("%s: %v", what, rows)
		}
		t.Logf("%s: refused by the local embedder: %s", what, status)
	}
	check("AI.GENERATE_EMBEDDING of an ObjectRef", mlQuery(t, h, c, embed))
	if err := mlRun(h, c, fmt.Sprintf("CREATE EXTERNAL TABLE `%s.objs` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/img/*'])",
		ds, conn, bh.BucketName())); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE: %v", err)
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT uri, ARRAY_LENGTH(embedding) AS n, status FROM AI.GENERATE_EMBEDDING(MODEL `%s.emb`, TABLE `%s.objs`)", ds, ds))
	check("AI.GENERATE_EMBEDDING of an object table", rows)
	if rows[0]["uri"] != uri {
		t.Fatalf("the object table's columns are not in the output: %v", rows)
	}

	t.Run("AI.GENERATE", func(t *testing.T) {
		h := h.For(t)
		h.Endpoint(EnvLocalAI)
		rows := mlQuery(t, h, c, fmt.Sprintf("SELECT g.result, g.status FROM (SELECT AI.GENERATE(('Name the main colour of this image in one word: ', OBJ.MAKE_REF('%s', '%s')), "+
			"connection_id => '%s', endpoint => '%s') AS g)", uri, conn, conn, localModel))
		if len(rows) != 1 {
			t.Fatalf("AI.GENERATE of an image ref: %v", rows)
		}
		if s, _ := rows[0]["status"].(string); s != "" {
			if strings.Contains(s, "no local generation runtime is configured") {
				t.Skipf("the instance serves no local generation model (up --local-ai-model): %s", s)
			}
			if !strings.Contains(strings.ToLower(s), "image") {
				t.Fatalf("AI.GENERATE of an image ref: %v", rows)
			}
			t.Logf("refused by the local generator: %s", s)
			return
		}
		if r, _ := rows[0]["result"].(string); strings.TrimSpace(r) == "" {
			t.Fatalf("AI.GENERATE of an image ref: empty result: %v", rows)
		}
		t.Logf("AI.GENERATE of an image ref (recorded, not asserted): %q", rows[0]["result"])
	})
}
