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
	// ServiceAccount is the identity the procedure runs as (the
	// connection's), written in its log entries' labels.
	ServiceAccount string `json:"serviceAccount,omitempty"`
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
}

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
