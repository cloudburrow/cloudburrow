// A Jupyter session's Scala kernel (internal/sparkproc, #2162): `spark`
// is a Spark Connect session of the session's server, from Spark's Scala
// Connect client.
interp.load.cp(os.Path("/opt/cloudburrow/spark-connect-client-jvm.jar"))

@

import org.apache.spark.sql.SparkSession
val spark = SparkSession.builder().remote(sys.env.getOrElse("CB_CONNECT", "sc://localhost:15002")).getOrCreate()
