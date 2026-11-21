# bigquery.spark.procedure, the library BigQuery's runtime gives a stored
# procedure for Apache Spark written in Python
# (https://docs.cloud.google.com/bigquery/docs/spark-procedures):
# SparkProcParamContext.getOrCreate(spark) holds the procedure's arguments,
# read from the BIGQUERY_PROC_PARAM.<name> environment variables, whose
# values are JSON; an OUT or INOUT argument is set by assigning the
# attribute of its name. CloudBurrow's runner reads the values set from
# CB_SPARK_OUT when the application ends.
import atexit
import json
import os

_PREFIX = "BIGQUERY_PROC_PARAM."


class SparkProcParamContext(object):
    _instance = None

    def __init__(self):
        values = {}
        for key, value in os.environ.items():
            if key.startswith(_PREFIX):
                values[key[len(_PREFIX):]] = json.loads(value)
        object.__setattr__(self, "_values", values)
        object.__setattr__(self, "_assigned", {})

    @classmethod
    def getOrCreate(cls, spark=None):
        if cls._instance is None:
            cls._instance = cls()
            atexit.register(cls._instance._write)
        return cls._instance

    def __getattr__(self, name):
        assigned = object.__getattribute__(self, "_assigned")
        if name in assigned:
            return assigned[name]
        values = object.__getattribute__(self, "_values")
        if name in values:
            return values[name]
        raise AttributeError("The procedure has no argument named " + name)

    def __setattr__(self, name, value):
        object.__getattribute__(self, "_assigned")[name] = value

    def _write(self):
        path = os.environ.get("CB_SPARK_OUT")
        if not path:
            return
        out = {}
        for name, value in object.__getattribute__(self, "_assigned").items():
            out[name] = json.dumps(value, default=str)
        with open(path, "w") as f:
            json.dump(out, f)
