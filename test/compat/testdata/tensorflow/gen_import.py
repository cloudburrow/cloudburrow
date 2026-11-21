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
<out>/sparse:  a tf.train.Example signature with VarLenFeatures v FLOAT32
               and ids INT64 and a FixedLenFeature a FLOAT32[2]; outputs
               total FLOAT32[None] (tf.sparse.reduce_sum of v plus a's
               sum) and emb FLOAT32[None,3]
               (tf.nn.embedding_lookup_sparse of ids, combiner "sqrtn").
<out>/sparsein: SparseTensor inputs, written with tf.compat.v1's
               sparse_placeholder and predict_signature_def (coo_sparse
               TensorInfos): v FLOAT32[None,4], ids INT64[None,None] and a
               dense x FLOAT32[None,2]; outputs total FLOAT32[None]
               (tf.sparse.reduce_sum of v plus x's sum), proj
               FLOAT32[None,3] (tf.sparse.sparse_dense_matmul of v),
               dense FLOAT32[None,4] (tf.sparse.to_dense of v) and emb
               FLOAT32[None,3] (tf.nn.embedding_lookup_sparse of ids,
               combiner "mean").
<out>/sparse3: a SparseTensor input of rank 3, which no column fills.
<out>/hlocase: a jax2tf-style SavedModel: an XlaCallModule (a core
               TensorFlow op) whose StableHLO module is a stablehlo.case
               of three branches (x + x, x * x, x - 1) on i INT32[], over
               x FLOAT32[3]; TFLiteConverter writes it as STABLEHLO_CASE.
<out>/predictions.json: {"cf": [...], "example": [...], "sparse": [...],
               "sparsein": [...], "hlocase": [...]}, each row's inputs and TensorFlow's
               outputs; a SparseTensor is fed as BigQuery's "converts
               them into Sparse format" does, a row's n values at
               indices [0, 0] .. [0, n-1].
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
    def __init__(self):
        self.e = tf.Variable(rng.normal(size=(10, 3)).astype(np.float32))

    @tf.function(input_signature=[tf.TensorSpec([None], tf.string, name="examples")])
    def serve(self, examples):
        f = tf.io.parse_example(examples, {"v": tf.io.VarLenFeature(tf.float32), "ids": tf.io.VarLenFeature(tf.int64),
                                           "a": tf.io.FixedLenFeature([2], tf.float32)})
        return {"total": tf.sparse.reduce_sum(f["v"], axis=1) + tf.reduce_sum(f["a"], axis=1),
                "emb": tf.nn.embedding_lookup_sparse(self.e, f["ids"], None, combiner="sqrtn")}


E = rng.normal(size=(10, 3)).astype(np.float32)
W = rng.normal(size=(4, 3)).astype(np.float32)


def save_v1(name, build):
    """A SavedModel written with tf.compat.v1's Builder, whose
    serving_default inputs are the placeholders build makes (a
    sparse_placeholder's TensorInfo is coo_sparse)."""
    g = tf.Graph()
    with g.as_default():
        ins, outs = build()
        with tf.compat.v1.Session() as s:
            b = tf.compat.v1.saved_model.Builder(os.path.join(out, name))
            b.add_meta_graph_and_variables(s, ["serve"], signature_def_map={
                "serving_default": tf.compat.v1.saved_model.predict_signature_def(ins, outs)})
            b.save()


def sparsein():
    v = tf.compat.v1.sparse_placeholder(tf.float32, [None, 4], name="v")
    ids = tf.compat.v1.sparse_placeholder(tf.int64, [None, None], name="ids")
    x = tf.compat.v1.placeholder(tf.float32, [None, 2], name="x")
    return {"v": v, "ids": ids, "x": x}, {
        "total": tf.sparse.reduce_sum(v, axis=1) + tf.reduce_sum(x, axis=1),
        "proj": tf.sparse.sparse_dense_matmul(v, tf.constant(W)),
        "dense": tf.sparse.to_dense(v),
        "emb": tf.nn.embedding_lookup_sparse(tf.constant(E), ids, None, combiner="mean")}


def sparse3():
    v = tf.compat.v1.sparse_placeholder(tf.float32, [None, 2, None], name="v")
    return {"v": v}, {"s": tf.sparse.reduce_sum(v, axis=[1, 2])}


class Tiny(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None, 1], tf.float32, name="x")])
    def serve(self, x):
        return {"y": x * 2.0}


HLO_CASE = """
module @jit_f attributes {mhlo.num_partitions = 1 : i32, mhlo.num_replicas = 1 : i32} {
  func.func public @main(%arg0: tensor<i32>, %arg1: tensor<3xf32>) -> (tensor<3xf32>) {
    %0 = "stablehlo.case"(%arg0) ({
      %1 = stablehlo.add %arg1, %arg1 : tensor<3xf32>
      stablehlo.return %1 : tensor<3xf32>
    }, {
      %1 = stablehlo.multiply %arg1, %arg1 : tensor<3xf32>
      stablehlo.return %1 : tensor<3xf32>
    }, {
      %c = stablehlo.constant dense<1.0> : tensor<3xf32>
      %1 = stablehlo.subtract %arg1, %c : tensor<3xf32>
      stablehlo.return %1 : tensor<3xf32>
    }) : (tensor<i32>) -> tensor<3xf32>
    return %0 : tensor<3xf32>
  }
}
"""


class HloCase(tf.Module):
    """What jax2tf writes for a lax.switch: an XlaCallModule op."""

    def __init__(self):
        from tensorflow.compiler.mlir.stablehlo import stablehlo

        self.module = stablehlo.serialize_portable_artifact_str(HLO_CASE, stablehlo.get_minimum_version())

    @tf.function(input_signature=[tf.TensorSpec([], tf.int32, name="i"), tf.TensorSpec([3], tf.float32, name="x")])
    def serve(self, i, x):
        from tensorflow.compiler.tf2xla.ops import gen_xla_ops

        y = gen_xla_ops.xla_call_module([i, x], version=9, module=self.module, Sout=[[3]], Tout=[tf.float32], dim_args_spec=[],
                                        platforms=["CPU"])[0]
        return {"y": y}


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
sp = Sparse()
save(sp, "sparse")
save_v1("sparsein", sparsein)
save_v1("sparse3", sparse3)
hc = HloCase()
save(hc, "hlocase")
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


def coo(vals, dtype):
    """A row's ARRAY as BigQuery feeds a SparseTensor: its n values at
    [0, 0] .. [0, n-1], dense shape [1, n]."""
    vals = np.asarray(vals, dtype)
    return (tf.constant([[0, i] for i in range(len(vals))], tf.int64, shape=[len(vals), 2]), tf.constant(vals),
            tf.constant([1, len(vals)], tf.int64))


sp_fn = tf.saved_model.load(os.path.join(out, "sparse")).signatures["serving_default"]
si_fn = tf.saved_model.load(os.path.join(out, "sparsein")).signatures["serving_default"]
pred["sparse"], pred["sparsein"] = [], []
# v has 0..4 values (none: an empty ARRAY); ids 1..3.
for i in range(5):
    v = rng.normal(size=i).astype(np.float32)
    ids = [int(z) for z in rng.integers(0, 10, size=1 + i % 3)]
    a = rng.normal(size=2).astype(np.float32)
    e = tf.train.Example(features=tf.train.Features(feature={
        "v": tf.train.Feature(float_list=tf.train.FloatList(value=v.tolist())),
        "ids": tf.train.Feature(int64_list=tf.train.Int64List(value=ids)),
        "a": tf.train.Feature(float_list=tf.train.FloatList(value=a.tolist()))}))
    o = sp_fn(examples=tf.constant([e.SerializeToString()]))
    pred["sparse"].append({"inputs": {"v": v.tolist(), "ids": ids, "a": a.tolist()},
                           "outputs": {kk: np.asarray(vv.numpy()).ravel().tolist() for kk, vv in o.items()}})
    vi, vv, vs = coo(v, np.float32)
    ii, iv, isz = coo(ids, np.int64)
    vs = tf.constant([1, 4], tf.int64)  # v's dense shape is [batch, 4]
    x = rng.normal(size=(1, 2)).astype(np.float32)
    o = si_fn(v_indices=vi, v_values=vv, v_dense_shape=vs, ids_indices=ii, ids_values=iv, ids_dense_shape=isz, x=tf.constant(x))
    pred["sparsein"].append({"inputs": {"v": v.tolist(), "ids": ids, "x": x[0].tolist()},
                             "outputs": {kk: np.asarray(vv.numpy()).ravel().tolist() for kk, vv in o.items()}})
hc_fn = tf.saved_model.load(os.path.join(out, "hlocase")).signatures["serving_default"]
pred["hlocase"] = []
for i in (-1, 0, 1, 2, 7):  # out of range is the last branch
    x = rng.normal(size=3).astype(np.float32)
    o = hc_fn(i=tf.constant(i, tf.int32), x=tf.constant(x))
    pred["hlocase"].append({"inputs": {"i": i, "x": x.tolist()}, "outputs": {"y": o["y"].numpy().tolist()}})
for k in ("cf", "example", "sparse", "sparsein", "hlocase"):
    for r in pred[k]:
        for kk, v in r["outputs"].items():
            r["outputs"][kk] = [float(z) if not isinstance(z, bool) else (1.0 if z else 0.0) for z in v]
with open(os.path.join(out, "predictions.json"), "w") as f:
    json.dump(pred, f, indent=1)
