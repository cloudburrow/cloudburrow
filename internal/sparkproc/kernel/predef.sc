// A Jupyter session's Scala kernel (internal/sparkproc, #2162): `spark`
// is a Spark Connect session of the session's server, from Spark's Scala
// Connect client (on the kernel's --extra-class-path with the runtime's
// common utilities and their dependencies, json4s, Jackson and logging
// among them; Spark's server, Scala, Hadoop and the jars the client
// shades stay off it). Lazy, so a kernel starts even before its first use.
import org.apache.spark.sql.SparkSession
lazy val spark = SparkSession.builder().remote(sys.env.getOrElse("CB_CONNECT", "sc://localhost:15002")).getOrCreate()
