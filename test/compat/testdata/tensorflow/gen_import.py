"""Writes the SavedModels TestBigQueryMLTensorFlowImportGenerated imports
(#1775), and TensorFlow's own serving_default output for its rows.

The test runs it, with no network, in CloudBurrow's pinned TensorFlow
converter image (deploy/tfconvert: TensorFlow 2.20.0 from PyPI,
Apache-2.0), the image the instance converts with:

  docker run --rm --network none --entrypoint python3 \
    -v <this folder>:/w:ro -v <out>:/out cloudburrow/tfconvert:<tag> -I /w/gen_import.py /out

<out>/cf:      control flow: x FLOAT32[None,3], n INT64[None]. A
               tf.while_loop of n steps whose body takes a tf.cond (halve x
               when its sum passes 10, else add 1.5), then a tf.cond on the
               result; outputs y FLOAT32[None,3], total FLOAT32[None],
               halvings INT64[None] and big BOOL[None].
<out>/example: a tf.train.Example signature: one string input of
               serialized Examples, parsed with tf.io.parse_example into
               features a FLOAT32[3] and k INT64[] (default 2); outputs
               score FLOAT32[None,1] and k2 INT64[None].
<out>/old:     a SavedModel whose GraphDef producer version is 19.
<out>/dev:     a SavedModel written by tensorflow_version 2.21.0-dev20251101.
<out>/ragged:  a tf.train.Example signature with a RaggedFeature.
<out>/sparse:  a tf.train.Example signature with a VarLenFeature.
<out>/predictions.json: {"cf": [...], "example": [...]}, each row's
               inputs and TensorFlow's outputs.
"""

import json
import os
import sys

import numpy as np
import tensorflow as tf
from tensorflow.core.protobuf import saved_model_pb2

os.umask(0)
out = sys.argv[1]
rng = np.random.default_rng(1775)


class ControlFlow(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None, 3], tf.float32, name="x"), tf.TensorSpec([None], tf.int64, name="n")])
    def serve(self, x, n):
        steps = tf.reduce_max(n)

        def cond(i, v, h):
            return i < steps

        def body(i, v, h):
            big = tf.reduce_sum(v) > 10.0
            v = tf.cond(big, lambda: v * 0.5, lambda: v + 1.5)
            return i + 1, v, h + tf.cast(big, tf.int64)

        _, y, h = tf.while_loop(cond, body, [tf.constant(0, tf.int64), x, tf.constant(0, tf.int64)])
        total = tf.cond(h > 1, lambda: tf.reduce_sum(y, axis=1) * 2.0, lambda: tf.reduce_sum(y, axis=1) - 3.0)
        return {"y": y, "total": total, "halvings": tf.reshape(h, [1]), "big": tf.reduce_max(y, axis=1) > 4.0}


class Example(tf.Module):
    def __init__(self):
        self.w = tf.Variable(rng.normal(size=(3, 1)).astype(np.float32))

    @tf.function(input_signature=[tf.TensorSpec([None], tf.string, name="examples")])
    def serve(self, examples):
        f = tf.io.parse_example(examples, {"a": tf.io.FixedLenFeature([3], tf.float32),
                                           "k": tf.io.FixedLenFeature([], tf.int64, default_value=2)})
        score = tf.sigmoid(tf.matmul(f["a"], self.w) + tf.cast(tf.expand_dims(f["k"], 1), tf.float32) * 0.25)
        return {"score": score, "k2": f["k"] * 3 + 1}


class Ragged(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None], tf.string, name="examples")])
    def serve(self, examples):
        f = tf.io.parse_example(examples, {"r": tf.io.RaggedFeature(tf.float32)})
        return {"s": tf.reduce_sum(f["r"], axis=1)}


class Sparse(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None], tf.string, name="examples")])
    def serve(self, examples):
        f = tf.io.parse_example(examples, {"v": tf.io.VarLenFeature(tf.float32)})
        return {"s": tf.sparse.reduce_sum(f["v"], axis=1)}


class Tiny(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None, 1], tf.float32, name="x")])
    def serve(self, x):
        return {"y": x * 2.0}


def save(m, name):
    tf.saved_model.save(m, os.path.join(out, name), signatures={"serving_default": m.serve})


def edit(name, fn):
    p = os.path.join(out, name, "saved_model.pb")
    sm = saved_model_pb2.SavedModel()
    with open(p, "rb") as f:
        sm.ParseFromString(f.read())
    for mg in sm.meta_graphs:
        fn(mg)
    with open(p, "wb") as f:
        f.write(sm.SerializeToString())


cf, ex = ControlFlow(), Example()
save(cf, "cf")
save(ex, "example")
save(Ragged(), "ragged")
save(Sparse(), "sparse")
save(Tiny(), "old")
save(Tiny(), "dev")
edit("old", lambda mg: setattr(mg.graph_def.versions, "producer", 19))
edit("dev", lambda mg: setattr(mg.meta_info_def, "tensorflow_version", "2.21.0-dev20251101"))

# TensorFlow's own output, one row at a time (as BigQuery feeds a row).
cf_fn = tf.saved_model.load(os.path.join(out, "cf")).signatures["serving_default"]
ex_fn = tf.saved_model.load(os.path.join(out, "example")).signatures["serving_default"]
pred = {"cf": [], "example": []}
for i in range(6):
    x = (rng.normal(size=(1, 3)) * 4).astype(np.float32)
    n = np.array([i + 1], np.int64)
    o = cf_fn(x=tf.constant(x), n=tf.constant(n))
    pred["cf"].append({"inputs": {"x": x[0].tolist(), "n": int(n[0])},
                       "outputs": {k: np.asarray(v.numpy()).ravel().tolist() for k, v in o.items()}})
for i in range(5):
    a = rng.normal(size=3).astype(np.float32)
    k = int(i * 2 - 3)
    e = tf.train.Example(features=tf.train.Features(feature={
        "a": tf.train.Feature(float_list=tf.train.FloatList(value=a.tolist())),
        "k": tf.train.Feature(int64_list=tf.train.Int64List(value=[k]))}))
    o = ex_fn(examples=tf.constant([e.SerializeToString()]))
    pred["example"].append({"inputs": {"a": a.tolist(), "k": k},
                            "outputs": {kk: np.asarray(v.numpy()).ravel().tolist() for kk, v in o.items()}})
for k in ("cf", "example"):
    for r in pred[k]:
        for kk, v in r["outputs"].items():
            r["outputs"][kk] = [float(z) if not isinstance(z, bool) else (1.0 if z else 0.0) for z in v]
with open(os.path.join(out, "predictions.json"), "w") as f:
    json.dump(pred, f, indent=1)
