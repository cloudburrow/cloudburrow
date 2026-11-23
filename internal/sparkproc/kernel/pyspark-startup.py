# A Jupyter session's PySpark kernel (internal/sparkproc, #2162): `spark`
# is a Spark Connect session of the session's server.
import os

from pyspark.sql import SparkSession

spark = SparkSession.builder.remote(os.environ.get("CB_CONNECT", "sc://localhost:15002")).getOrCreate()
