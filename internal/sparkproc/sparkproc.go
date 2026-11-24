// Package sparkproc runs BigQuery's stored procedures for Apache Spark
// (#2104) on the host, in a local Apache Spark container.
//
// BigQuery runs a procedure created WITH CONNECTION … OPTIONS
// (engine="SPARK", …) LANGUAGE PYTHON|JAVA|SCALA on its serverless Spark
// (https://docs.cloud.google.com/bigquery/docs/spark-procedures). The
// BigQuery front is a CGO-less static binary in the cluster and runs no
// Spark, so the CLI serves this runner on --port-spark, published to the
// cluster through cloudburrow-host like the ONNX runner and the
// TensorFlow converter. The front sends a CALL's job: the procedure's code
// or its main file, its py_file_uris, jar_uris, file_uris and
// archive_uris (read from the instance's Cloud Storage by the front), its
// properties and its arguments. The runner writes them to a directory and
// runs `spark-submit --master local[*]` in the official apache/spark image
// (Apache-2.0, pinned by digest in dependencies.json) with Google's
// BigQuery connector for Spark (spark-3.5-bigquery, Apache-2.0, Maven
// Central, pinned by SHA-256) on the classpath, as Google's runtime has it.
//
// Egress: the Spark container is on a Docker network created --internal,
// which has no route out. Its one peer is a relay container (the same
// image, running relay.py) on that network and on the default bridge,
// whose aliases are the connector's endpoints: bigquery.cloudburrow.internal
// (port 80, the instance's BigQuery REST port) and
// bigquerystorage.cloudburrow.internal (port 443, TLS with a certificate
// of a CA made for the run, which the Spark JVM trusts, terminated and
// relayed to the instance's Storage Read and Write API port). The relay
// dials only those two host ports. The connector is given a fixed access
// token, so it asks no token server for one.
//
// The driver's and executors' output is written to the instance's Cloud
// Logging as the bigquery.googleapis.com/SparkJob resource, the entries'
// labels.component DRIVER or EXECUTOR, as the page documents.
package sparkproc

import (
	"encoding/json"
	"fmt"
)

// Job is one CALL of a stored procedure for Apache Spark.
type Job struct {
	// SparkJobID is the job's sparkStatistics.sparkJobId; Project,
	// Location and Procedure (project.dataset.routine) name it in logs.
	SparkJobID string `json:"sparkJobId"`
	Project    string `json:"project"`
	Location   string `json:"location"`
	Procedure  string `json:"procedure"`
	// Language is PYTHON, JAVA or SCALA.
	Language string `json:"language"`
	// Code is a PySpark procedure's inline body; Main its main_file_uri's
	// object, MainClass its main_class.
	Code      string `json:"code,omitempty"`
	Main      *File  `json:"main,omitempty"`
	MainClass string `json:"mainClass,omitempty"`
	PyFiles   []File `json:"pyFiles,omitempty"`
	Jars      []File `json:"jars,omitempty"`
	Files     []File `json:"files,omitempty"`
	Archives  []File `json:"archives,omitempty"`
	// Properties are the procedure's Spark properties.
	Properties map[string]string `json:"properties,omitempty"`
	// Params are the arguments' values, JSON-encoded, by name: the
	// BIGQUERY_PROC_PARAM.<name> environment variables. Out are the OUT
	// and INOUT arguments' names, which a PySpark procedure sets.
	Params map[string]string `json:"params,omitempty"`
	Out    []string          `json:"out,omitempty"`
	// Types are the arguments' GoogleSQL types, by name, which the
	// procedure's SparkProcParamContext converts their values to Python's
	// types by (BIGQUERY_PROC_PARAM's values are JSON).
	Types map[string]string `json:"types,omitempty"`
	// ServiceAccount is the identity the procedure runs as (the
	// connection's, or the invoker's), written in its log entries' labels.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// RuntimeVersion is the procedure's runtime_version (MAJOR.MINOR, ""
	// for the default), which picks the Spark image and connector.
	RuntimeVersion string `json:"runtimeVersion,omitempty"`
	// ContainerImage is the procedure's container_image, an Artifact
	// Registry path ({location}-docker.pkg.dev/P/R/IMAGE[:TAG|@DIGEST])
	// of the instance's own registry, or "".
	ContainerImage string `json:"containerImage,omitempty"`
	// AccessToken is the token the BigQuery connector sends: an
	// EXTERNAL SECURITY INVOKER procedure's caller's, so the instance
	// checks the job's reads and writes as the caller's ("" for the fixed
	// token of the instance's own account). RunAs, when set, is the
	// service account an invoker set with
	// @@spark_proc_properties.service_account: the runner mints a token
	// of the instance for it, and the job runs as that account.
	AccessToken string `json:"accessToken,omitempty"`
	RunAs       string `json:"runAs,omitempty"`
	// KMSKey is the job's customer-managed key (a CryptoKey of the
	// instance's Cloud KMS), or "", and KeyAgent BigQuery's encryption
	// service account, which must be able to encrypt with it. The runner
	// checks the key before it runs anything (Docker.KeyCheck).
	KMSKey   string `json:"kmsKey,omitempty"`
	KeyAgent string `json:"keyAgent,omitempty"`
	// KeyOnly asks for the key's check alone: nothing is run.
	KeyOnly bool `json:"keyOnly,omitempty"`

	// Kind is "" for a stored procedure, or the Dataproc workload a job
	// of Serverless for Apache Spark (#2156) runs: KindPySpark,
	// KindSpark, KindSparkR or KindSparkSQL for a batch, KindConnect for
	// a Spark Connect session (Docker.StartConnect).
	Kind string `json:"kind,omitempty"`
	// Args are a batch's application arguments, after its main file.
	Args []string `json:"args,omitempty"`
	// QueryVariables are a Spark SQL batch's query_variables, each set
	// first as "SET name=\"value\";", as the API reference says they are.
	QueryVariables map[string]string `json:"queryVariables,omitempty"`
	// AppID is the workload ID a batch or session runs as, recorded in
	// its event log as WorkloadProperty so its Spark UI is found by it
	// ("" for none).
	AppID string `json:"appId,omitempty"`
	// Log names the Cloud Logging resource and logs the job's output is
	// written to; nil is a procedure's bigquery.googleapis.com/SparkJob.
	Log *LogTarget `json:"log,omitempty"`

	// A Hive, Pig or Trino job's: ContinueOnFailure runs its remaining
	// queries after one fails; OutputFormat and ClientTags are a Trino
	// job's; SavepointURI a Flink job's savepoint to start from.
	ContinueOnFailure bool     `json:"continueOnFailure,omitempty"`
	OutputFormat      string   `json:"outputFormat,omitempty"`
	ClientTags        []string `json:"clientTags,omitempty"`
	SavepointURI      string   `json:"savepointUri,omitempty"`
	// Kernel is a Jupyter session's kernel (PYTHON or SCALA): its Spark
	// Connect server is joined by a Jupyter Server of the session's
	// kernels (Connect.KernelAddr). "" for a Spark Connect session.
	Kernel string `json:"kernel,omitempty"`
	// IPython runs the job's driver with IPython on its PYTHONPATH
	// (Docker.IPython): a notebook batch's, whose cells IPython runs.
	IPython bool `json:"ipython,omitempty"`
	// DriverNode is the driver node group instance a cluster job's
	// driver runs on ("": a container of its own).
	DriverNode string `json:"driverNode,omitempty"`
}

// The kinds of Dataproc workload (Job.Kind).
const (
	KindPySpark  = "pyspark"
	KindSpark    = "spark"
	KindSparkR   = "sparkr"
	KindSparkSQL = "sparksql"
	KindConnect  = "connect"
	// A Dataproc cluster's jobs of its other engines (#2162), run by
	// Cluster.Submit beside Spark's: Hadoop MapReduce, Hive, Pig, Trino
	// (and Presto's API on it) and Flink.
	KindHadoop = "hadoop"
	KindHive   = "hive"
	KindPig    = "pig"
	KindTrino  = "trino"
	KindFlink  = "flink"
)

// LogTarget is where a Dataproc workload's lines go: its monitored
// resource (cloud_dataproc_batch or cloud_dataproc_session, with their
// labels) and its logs, the driver's output in Output and Spark's own
// log4j lines in Spark.
type LogTarget struct {
	Type   string            `json:"type"`
	Labels map[string]string `json:"labels"`
	Output string            `json:"output"`
	Spark  string            `json:"spark"`
}

// File is one Cloud Storage object of a job, by its gs:// URI.
type File struct {
	URI  string `json:"uri"`
	Data []byte `json:"data"`
}

// Base is the object's name after its last slash, the name Spark gives
// it in a working directory.
func (f File) Base() string {
	for i := len(f.URI) - 1; i >= 0; i-- {
		if f.URI[i] == '/' {
			return f.URI[i+1:]
		}
	}
	return f.URI
}

// Result is how a job ended.
type Result struct {
	// Failed is set when the Spark application failed; Message is its
	// error, the last lines of the driver's output.
	Failed  bool   `json:"failed"`
	Message string `json:"message,omitempty"`
	// Out are the OUT and INOUT arguments' final values, JSON-encoded, by
	// name; one the procedure left unset is missing.
	Out map[string]string `json:"out,omitempty"`
	// DriverOutput is the driver's output, its last MaxDriverOutput
	// bytes, for the job's staging bucket.
	DriverOutput string `json:"driverOutput,omitempty"`
	// AppID is the application's ID on a cluster's master, read from the
	// driver's output (Cluster.Submit), or "".
	AppID string `json:"appId,omitempty"`
	// YarnApps are the YARN applications a Hadoop MapReduce or Pig job
	// ran on the cluster's YARN (Cluster.Submit).
	YarnApps []YarnApp `json:"yarnApps,omitempty"`
	// KeyRefused is why the job's KMSKey cannot be used, and KeyKind
	// which way (invalid, notFound, unusable, permissionDenied,
	// unavailable, noKMS); nothing was run.
	KeyRefused string `json:"keyRefused,omitempty"`
	KeyKind    string `json:"keyKind,omitempty"`
}

// MaxDriverOutput bounds Result.DriverOutput.
const MaxDriverOutput = 4 << 20

// Unavailable is a runner that could not run Spark at all (no docker, or
// the image or the connector could not be had).
type Unavailable struct{ Msg string }

func (u *Unavailable) Error() string { return u.Msg }

// MaxJobBytes bounds a job's request: the code and every file.
const MaxJobBytes = 512 << 20

func decodeUnavailable(b []byte) error {
	var u Unavailable
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) != nil || m.Message == "" {
		return &Unavailable{Msg: fmt.Sprintf("the Spark runner answered %q", string(b))}
	}
	u.Msg = m.Message
	return &u
}

// WorkloadProperty is the Spark property a batch's or session's driver
// carries its workload's application ID (Job.AppID) in.
const WorkloadProperty = "spark.cloudburrow.workload"
