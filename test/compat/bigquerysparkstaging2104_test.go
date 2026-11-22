//go:build compat

package compat

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/storage"
	bq "google.golang.org/api/bigquery/v2"
	bqconn "google.golang.org/api/bigqueryconnection/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.projects.getServiceAccount
//
// TestBigQuerySparkProcedureStagingAndCMEK (#2104): a stored procedure
// for Apache Spark's staging bucket and customer-managed key, as the page
// documents them (https://docs.cloud.google.com/bigquery/docs/spark-procedures,
// re-read 2026-11-21), through the official cloud.google.com/go bigquery,
// bigquery/connection/apiv1, storage and kms/apiv1 clients. SET
// @@spark_proc_properties.staging_bucket names "the Cloud Storage bucket
// that is used as the default Spark application file system": the CALL
// writes the job's code and the driver's output to it, in the instance's
// Cloud Storage, once the job's identity (the connection's service account
// of a DEFINER procedure) holds storage.objects.* there (refused before);
// a bucket that does not exist fails the CALL. A DEFINER procedure's CMEK
// is its connection's kmsKeyName, a key of the instance's Cloud KMS in the
// connection's single region: the staged objects carry it as their
// kmsKeyName, and with its primary version disabled the CALL is refused.
// An INVOKER procedure's CMEK is @@spark_proc_properties.kms_key_name: a
// multi-region key and a key that does not exist are refused.
func TestBigQuerySparkProcedureStagingAndCMEK(t *testing.T) {
	h, c, project := sparkEnv(t)
	if h.Endpoint(EnvKMS) == "" {
		t.Skip("no Cloud KMS endpoint: the procedure's key is the instance's Cloud KMS key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	const region = "us-central1"
	under := strings.ReplaceAll(h.Project(), "-", "_")
	sc := storageClient(t, h)
	k := kmsClients(t, h)["grpc"]

	// The key, granted to BigQuery's encryption service account (the page's
	// "BigQuery encryption service account") and Cloud Storage's agent.
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/" + region,
		KeyRingId: "sparkcmek-" + strings.ReplaceAll(under, "_", "-")})
	if err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "spark",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	bqAgent, err := svc.Projects.GetServiceAccount(project).Context(ctx).Do()
	if err != nil {
		t.Fatalf("projects.getServiceAccount: %v", err)
	}
	gcsAgent, err := sc.ServiceAccount(ctx, project)
	if err != nil {
		t.Fatalf("storage ServiceAccount: %v", err)
	}
	kp, err := k.ResourceIAM(key.GetName()).Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{bqAgent.Email, gcsAgent} {
		kp.Add("serviceAccount:"+a, iam.RoleName("roles/cloudkms.cryptoKeyEncrypterDecrypter"))
	}
	if err := k.ResourceIAM(key.GetName()).SetPolicy(ctx, kp); err != nil {
		t.Fatalf("grant the agents on the key: %v", err)
	}

	// A Spark connection in the region, its kmsKeyName the key.
	cc, _ := remoteConnectionClient(t, h)
	id := under + "_sparkcmek"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/" + region,
		ConnectionId: id, Connection: &connectionpb.Connection{
			Properties: &connectionpb.Connection_Spark{Spark: &connectionpb.SparkProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection (spark): %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteConnection(context.Background(), &connectionpb.DeleteConnectionRequest{Name: cn.Name})
	})
	// connectionpb has no kmsKeyName; the discovery client's Connection
	// does ("bq mk --connection ... --kms_key_name", the page's).
	u, err := url.Parse(h.Endpoint(EnvBigQuery))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := bqconn.NewService(ctx, option.WithEndpoint(u.Scheme+"://"+u.Host+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Projects.Locations.Connections.Patch(cn.Name, &bqconn.Connection{KmsKeyName: key.GetName()}).
		UpdateMask("kmsKeyName").Context(ctx).Do(); err != nil {
		t.Fatalf("connections.patch kmsKeyName: %v", err)
	}
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionAdmin", Members: []string{callerMember(project)}}}}}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
	conn := project + "." + region + "." + id
	ds := "compat_sparkcmek_" + under
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: region}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(context.Background()) })
	code := `
from pyspark.sql import SparkSession
from bigquery.spark.procedure import SparkProcParamContext
spark = SparkSession.builder.getOrCreate()
ctx = SparkProcParamContext.getOrCreate(spark)
print("staged-hello from the driver")
ctx.n = spark.range(5).count()
`
	for name, security := range map[string]string{"definer": "", "invoker": "EXTERNAL SECURITY INVOKER "} {
		if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE PROCEDURE `%s.%s`(OUT n INT64) %sWITH CONNECTION `%s` OPTIONS(engine=\"SPARK\") "+
			"LANGUAGE PYTHON AS r\"\"\"%s\"\"\"", ds, name, security, conn, code)); err != nil {
			t.Fatalf("CREATE PROCEDURE %s: %v", name, err)
		}
	}

	// The staging bucket, in the region.
	bucket := strings.ReplaceAll(h.Project(), "_", "-") + "-sparkstage"
	bh := sc.Bucket(bucket)
	if err := bh.Create(ctx, project, &storage.BucketAttrs{Location: strings.ToUpper(region)}); err != nil {
		t.Fatalf("create the staging bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })

	call := func(prefix, name string) (int64, string, error) {
		q := c.Query(fmt.Sprintf("%sDECLARE n INT64; CALL `%s.%s`(n); SELECT n", prefix, ds, name))
		q.Location = region
		job, err := q.Run(ctx)
		if err != nil {
			return 0, "", err
		}
		it, err := job.Read(ctx)
		if err != nil {
			return 0, job.ID(), err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return 0, job.ID(), err
		}
		n, _ := row[0].(int64)
		return n, job.ID(), nil
	}
	stage := fmt.Sprintf("SET @@spark_proc_properties.staging_bucket = '%s';\n", bucket)

	// The connection's service account may not write the bucket yet.
	if n, _, err := call(stage, "definer"); err == nil || !strings.Contains(err.Error(), "storage.objects.create") {
		t.Errorf("CALL with a staging bucket its identity cannot write: %d, %v; want storage.objects.create refused", n, err)
	}
	if _, _, err := call("SET @@spark_proc_properties.staging_bucket = 'no-such-bucket-"+strings.ReplaceAll(under, "_", "-")+"';\n",
		"definer"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("CALL with a staging bucket that does not exist: %v", err)
	}
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pol.Add("serviceAccount:"+cn.GetSpark().GetServiceAccountId(), iam.RoleName("roles/storage.objectAdmin"))
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("grant objectAdmin on the staging bucket: %v", err)
	}
	var n int64
	deadline := time.Now().Add(3 * time.Minute)
	for {
		n, _, err = call(stage, "definer")
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil || n != 5 {
		t.Fatalf("CALL with a staging bucket and the connection's key: %d, %v", n, err)
	}

	// The staged code and the driver's output, under the key.
	var mainPy, driver *storage.ObjectAttrs
	it := bh.Objects(ctx, &storage.Query{Prefix: "spark-procedures/"})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("list the staging bucket: %v", err)
		}
		switch {
		case strings.HasSuffix(a.Name, "/staging/main.py"):
			mainPy = a
		case strings.HasSuffix(a.Name, "/driveroutput.000000000"):
			driver = a
		}
	}
	if mainPy == nil || driver == nil {
		t.Fatalf("staged objects: main.py %v, driveroutput %v", mainPy, driver)
	}
	for _, a := range []*storage.ObjectAttrs{mainPy, driver} {
		if !strings.HasPrefix(a.KMSKeyName, key.GetName()+"/cryptoKeyVersions/") {
			t.Errorf("%s kmsKeyName %q; want a version of %s", a.Name, a.KMSKeyName, key.GetName())
		}
	}
	read := func(name string) string {
		r, err := bh.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}
	if got := read(mainPy.Name); !strings.Contains(got, "spark.range(5).count()") {
		t.Errorf("staged main.py %q", got)
	}
	if got := read(driver.Name); !strings.Contains(got, "staged-hello from the driver") {
		t.Errorf("driver output lacks the procedure's print: %q", got)
	}

	// INVOKER: the variable's key; multi-region and missing keys refused.
	if _, _, err := call("SET @@spark_proc_properties.kms_key_name = 'projects/"+project+"/locations/us/keyRings/r/cryptoKeys/k';\n",
		"invoker"); err == nil || !strings.Contains(err.Error(), "multi-region") {
		t.Errorf("INVOKER with a multi-region key: %v", err)
	}
	if _, _, err := call("SET @@spark_proc_properties.kms_key_name = '"+ring.GetName()+"/cryptoKeys/missing';\n",
		"invoker"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("INVOKER with a key that does not exist: %v", err)
	}
	if n, _, err := call("SET @@spark_proc_properties.kms_key_name = '"+key.GetName()+"';\n", "invoker"); err != nil || n != 5 {
		t.Errorf("INVOKER with the key: %d, %v", n, err)
	}

	// The key's primary version disabled: the CALL is refused.
	v1 := key.GetName() + "/cryptoKeyVersions/1"
	if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v1, State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatalf("disable the key's version: %v", err)
	}
	if _, _, err := call(stage, "definer"); err == nil || !strings.Contains(err.Error(), "cannot be used") {
		t.Errorf("CALL with the key's primary version disabled: %v", err)
	}
}
