# The PySpark kernel's session (internal/sparkproc, #2162): `spark` and
# `sc` on the cluster's standalone master, as a Dataproc cluster's PySpark
# kernel has them, with Google's BigQuery connector for Spark, and its
# event log in the cluster's History Server.
import os
import socket

from pyspark.sql import SparkSession

spark = (
    SparkSession.builder.appName("PySparkShell")
    .master(os.environ["CB_MASTER_URL"])
    .config("spark.driver.host", socket.gethostname())
    .config("spark.jars", "/cb-connector/spark-bigquery.jar")
    .config("spark.eventLog.enabled", "true")
    .config("spark.eventLog.dir", "file:/cb-history")
    .config("spark.driver.extraJavaOptions", "-Djavax.net.ssl.trustStore=/tmp/cb-trust.jks -Djavax.net.ssl.trustStorePassword=changeit")
    .config("spark.executor.extraJavaOptions", "-Djavax.net.ssl.trustStore=/tmp/cb-trust.jks -Djavax.net.ssl.trustStorePassword=changeit")
    .config("spark.executorEnv.PYSPARK_PYTHON", "python3")
    .config("spark.datasource.bigquery.bigQueryHttpEndpoint", "http://host.docker.internal:8080")
    .config("spark.datasource.bigquery.bigQueryStorageGrpcEndpoint", "bigquerystorage.cloudburrow.internal:8443")
    .config("spark.datasource.bigquery.parentProject", os.environ.get("CB_PROJECT", ""))
    .config("spark.datasource.bigquery.project", os.environ.get("CB_PROJECT", ""))
    .config("spark.datasource.bigquery.gcpAccessToken", "cloudburrow-spark-procedure")
    .getOrCreate()
)
sc = spark.sparkContext
