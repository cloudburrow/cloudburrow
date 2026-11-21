package bigqueryfront

// A Spark procedure's staging bucket and customer-managed key (#2104).
//
// "Use a custom service account" on the page
// (https://docs.cloud.google.com/bigquery/docs/spark-procedures, re-read
// 2026-11-21) sets, before a CALL:
//
//	SET @@spark_proc_properties.staging_bucket='BUCKET_NAME';
//	SET @@spark_proc_properties.kms_key_name='projects/PROJECT_ID/locations/LOCATION/keyRings/KEY_RING_NAME/cryptoKeys/KMS_KEY_NAME';
//
// BUCKET_NAME is "The Cloud Storage bucket that is used as the default
// Spark application file system", on which the job's identity needs
// "storage.objects.* permissions or the roles/storage.objectAdmin IAM role
// on the staging bucket that you specify". The CALL writes the job's
// staged files there, in the instance's Cloud Storage: the inline code
// (main.py), the main file, and the py, jar, plain and archive files, under
// spark-procedures/SPARK_JOB_ID/staging/, and the driver's output, once
// the job ends, as spark-procedures/SPARK_JOB_ID/driveroutput.000000000.
// The layout is CloudBurrow's own: the page names the bucket, not what it
// holds (UNVERIFIED). The job's identity needs storage.objects.create on
// the bucket, checked first; a bucket that does not exist is 404. Without a
// staging bucket nothing is written: "a default Cloud Storage bucket is
// created in your project", whose name the page does not give, so none is
// made here.
//
// CMEK: "If your stored procedure is using the INVOKER security mode, your
// CMEK should be specified through the SQL system variable when calling
// the procedure. Otherwise, your CMEK can be specified through the
// connection associated with the stored procedure" (the connection's
// kmsKeyName). "Customer-managed encryption keys (CMEK) are only available
// when customers create single-region Spark procedures. Global region CMEK
// keys and multi-region CMEK keys, for example, EU or US, are not
// supported." So a key is refused 400 when the connection is in a
// multi-region or the key is global or in a multi-region, and when it is
// not in the connection's region. The key is the instance's Cloud KMS
// key: the Spark runner (on the host) checks it exists, is
// ENCRYPT_DECRYPT with an ENABLED primary version, and, with IAM enforced,
// that BigQuery's encryption service account (projects.getServiceAccount's
// bq-PROJECT_NUMBER@bigquery-encryption.iam.gserviceaccount.com, the
// account the page has the key granted to) may encrypt with it; the staged
// objects are written with it as their kmsKeyName, so Cloud Storage seals
// them under it (and checks it again, for its own service agent).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// sparkMultiRegions are the locations whose keys and procedures have no
// CMEK.
var sparkMultiRegions = map[string]bool{"global": true, "us": true, "eu": true}

// sparkStage is where a CALL stages its files, and with which key.
type sparkStage struct {
	bucket, key string
}

// prefix is the staged objects' prefix for a job.
func (st sparkStage) prefix(jobID string) string { return "spark-procedures/" + jobID + "/" }

// sparkKey is the CMEK a CALL uses: an INVOKER procedure's from
// @@spark_proc_properties.kms_key_name, a DEFINER one's from its
// connection.
func sparkKey(invoker bool, props map[string]string, cn *bqConnection) string {
	if invoker {
		return strings.TrimSpace(props["kms_key_name"])
	}
	var k string
	if cn != nil {
		_ = json.Unmarshal(cn.Fields["kmsKeyName"], &k)
	}
	return strings.TrimSpace(k)
}

// checkSparkKey is why key cannot protect a procedure of a connection in
// location, or "".
func checkSparkKey(key, location string) string {
	loc, ok := cmek.Location(key)
	if !ok {
		return "Invalid kms_key_name '" + key + "': it must be projects/PROJECT_ID/locations/LOCATION/keyRings/KEY_RING_NAME/cryptoKeys/KMS_KEY_NAME"
	}
	location = strings.ToLower(location)
	switch {
	case sparkMultiRegions[location]:
		return "Customer-managed encryption keys (CMEK) are only available for single-region Spark procedures: the connection is in " +
			location
	case sparkMultiRegions[strings.ToLower(loc)]:
		return "Global region CMEK keys and multi-region CMEK keys, for example, EU or US, are not supported: " + key + " is in " + loc
	case !strings.EqualFold(loc, location):
		return "The CMEK " + key + " is in " + loc + ": a Spark procedure's key must be in its connection's region, " + location
	}
	return ""
}

// sparkStaging reads and checks the CALL's staging bucket and key; sa is
// the job's identity.
func (s *scriptRun) sparkStaging(def *sparkDef, cn *bqConnection, sa, stmt string) (sparkStage, *scriptFlow) {
	fail := func(code int, reason, m string) *scriptFlow { return failWith(code, reason, m, stmt) }
	st := sparkStage{key: sparkKey(def.Invoker, s.sparkProps, cn)}
	if st.key != "" {
		if msg := checkSparkKey(st.key, cn.Location); msg != "" {
			return st, fail(http.StatusBadRequest, "invalid", msg)
		}
	}
	b := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s.sparkProps["staging_bucket"]), "gs://"), "/")
	if b == "" {
		return st, nil
	}
	if strings.Contains(b, "/") {
		return st, fail(http.StatusBadRequest, "invalid", "Invalid staging_bucket '"+s.sparkProps["staging_bucket"]+"': it names a Cloud Storage bucket")
	}
	if s.f.storage == nil || s.f.storageHost == "" {
		s.notImpl = "Not implemented here: a stored procedure for Apache Spark stages its files in the instance's Cloud Storage, " +
			"which the BigQuery front was not given (enable Cloud Storage). Nothing more was run."
		return st, &scriptFlow{}
	}
	status, err := s.f.storage.do(s.r.Context(), "/storage/v1/b/"+url.PathEscape(b), nil)
	switch {
	case err != nil:
		return st, fail(http.StatusInternalServerError, "internalError", "cloudburrow: Cloud Storage: "+err.Error())
	case status == http.StatusNotFound:
		return st, fail(http.StatusNotFound, "notFound", "Not found: the staging bucket "+b+" does not exist")
	case status != http.StatusOK:
		return st, fail(status, "invalid", fmt.Sprintf("Cloud Storage answered %d reading the staging bucket %s", status, b))
	}
	if code, msg := s.f.checkStoragePerm(s.r, sa, b, permObjectsCreate); code != 0 {
		return st, fail(code, "accessDenied", msg)
	}
	st.bucket = b
	return st, nil
}

// stagedObject is one file of a job as it is written to the staging
// bucket.
type stagedObject struct {
	name, contentType string
	data              []byte
}

// stagedFiles are job's files as they are written to the staging bucket,
// by object name: the inline code as main.py, then the main file and the
// py, jar, plain and archive files by their base names.
func stagedFiles(st sparkStage, job sparkproc.Job) []stagedObject {
	dir := st.prefix(job.SparkJobID) + "staging/"
	var out []stagedObject
	if job.Code != "" {
		out = append(out, stagedObject{dir + "main.py", "text/x-python", []byte(job.Code)})
	}
	var files []sparkproc.File
	if job.Main != nil {
		files = append(files, *job.Main)
	}
	for _, l := range [][]sparkproc.File{job.PyFiles, job.Jars, job.Files, job.Archives} {
		files = append(files, l...)
	}
	for _, f := range files {
		out = append(out, stagedObject{dir + f.Base(), "application/octet-stream", f.Data})
	}
	return out
}

// stageSparkFiles writes job's files to st's bucket.
func (s *scriptRun) stageSparkFiles(ctx context.Context, st sparkStage, job sparkproc.Job) error {
	if st.bucket == "" {
		return nil
	}
	for _, o := range stagedFiles(st, job) {
		if err := s.f.uploadKeyed(ctx, "gs://"+st.bucket+"/"+o.name, o.contentType, o.data, st.key); err != nil {
			return err
		}
	}
	return nil
}

// stageDriverOutput writes the driver's output to st's bucket.
func (s *scriptRun) stageDriverOutput(ctx context.Context, st sparkStage, job sparkproc.Job, out string) error {
	if st.bucket == "" {
		return nil
	}
	return s.f.uploadKeyed(ctx, "gs://"+st.bucket+"/"+st.prefix(job.SparkJobID)+"driveroutput.000000000", "text/plain", []byte(out), st.key)
}

// uploadKeyed writes an object to the instance's Cloud Storage, with key
// as its kmsKeyName when set.
func (f front) uploadKeyed(ctx context.Context, uri, contentType string, data []byte, key string) error {
	bucket, object, ok := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	if !strings.HasPrefix(uri, "gs://") || !ok || object == "" {
		return fmt.Errorf("not a gs:// URI of an object")
	}
	u := "http://" + f.storageHost + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?uploadType=media&name=" + url.QueryEscape(object)
	if key != "" {
		u += "&kmsKeyName=" + url.QueryEscape(key)
	}
	status, b, err := serviceCall(ctx, "Cloud Storage upload of "+uri, http.MethodPost, u, contentType, data)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		if len(msg) > 512 {
			msg = msg[:512]
		}
		return fmt.Errorf("Cloud Storage answered %d writing %s: %s", status, uri, msg)
	}
	return nil
}
