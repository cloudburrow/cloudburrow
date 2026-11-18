"""Writes the TensorFlow SavedModels of BigQuery ML's imported TENSORFLOW
models (#1775) and TensorFlow's own serving_default output for them.

Run in CloudBurrow's converter image (deploy/tfconvert: TensorFlow 2.20.0
from PyPI), with no network:

  docker run --rm --network none --entrypoint python3 -u 0 \
    -v "$PWD/internal/tflitemodel/testdata:/out" cloudburrow/tfconvert:<tag> -I /out/gen_savedmodel.py

savedmodel/mlp: inputs x FLOAT32[None,3] and k INT64[None,1]; outputs
  score (sigmoid of a dense layer, [None,1]) and probs (softmax, [None,2])
  and kk (k*2+1, INT64). Variables, so variables/ is written.
savedmodel/flex: an SVD, which TensorFlow Lite has no builtin for (Flex).
tensorflow_predictions.json: TensorFlow's serving_default output of mlp
  for each row of rows.
"""

import json

import numpy as np
import tensorflow as tf

tf.random.set_seed(7)
rng = np.random.default_rng(7)


class MLP(tf.Module):
    def __init__(self):
        self.w1 = tf.Variable(rng.normal(size=(3, 4)).astype(np.float32))
        self.b1 = tf.Variable(rng.normal(size=(4,)).astype(np.float32))
        self.w2 = tf.Variable(rng.normal(size=(4, 1)).astype(np.float32))
        self.w3 = tf.Variable(rng.normal(size=(4, 2)).astype(np.float32))

    @tf.function(input_signature=[tf.TensorSpec([None, 3], tf.float32, name="x"), tf.TensorSpec([None, 1], tf.int64, name="k")])
    def serve(self, x, k):
        h = tf.nn.relu(tf.matmul(x, self.w1) + self.b1)
        return {"score": tf.sigmoid(tf.matmul(h, self.w2)), "probs": tf.nn.softmax(tf.matmul(h, self.w3)), "kk": k * 2 + 1}


class Flex(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None, 3, 3], tf.float32, name="m")])
    def serve(self, m):
        return {"s": tf.linalg.svd(m, compute_uv=False)}


class Loop(tf.Module):
    """Control flow: a WHILE that doubles x until its sum passes limit,
    counting the steps, and an IF on the count."""

    @tf.function(input_signature=[tf.TensorSpec([None, 2], tf.float32, name="x"), tf.TensorSpec([None], tf.float32, name="limit")])
    def serve(self, x, limit):
        def cond(i, v):
            return tf.reduce_sum(v) < tf.reduce_sum(limit)

        def body(i, v):
            return i + 1, v * 2.0

        n, y = tf.while_loop(cond, body, [tf.constant(0, tf.int32), x])
        big = tf.cond(n > 2, lambda: tf.reduce_sum(y) * 10.0, lambda: tf.reduce_sum(y) - 1.0)
        return {"y": y, "steps": tf.reshape(tf.cast(n, tf.int64), [1]), "big": tf.reshape(big, [1]),
                "pos": tf.where(x > 0.0, x, -x)}


class Ops2(tf.Module):
    """The operators #1775 adds: rounding and trigonometry, ELU,
    LOG_SOFTMAX, max/min/product reductions, ARG_MAX/ARG_MIN, SLICE, TILE,
    PAD, FLOOR_DIV/FLOOR_MOD (on INT32: TensorFlow Lite has no INT64
    FloorDiv builtin), ADD_N, SPLIT and UNPACK."""

    @tf.function(input_signature=[tf.TensorSpec([None, 6], tf.float32, name="v"), tf.TensorSpec([None, 4], tf.int64, name="n")])
    def serve(self, v, n):
        a, b = tf.split(v, 2, axis=1)
        p, q, r = tf.unstack(tf.reshape(a, [-1, 3]), axis=1)
        return {
            "rounded": tf.concat([tf.floor(v), tf.math.ceil(v), tf.round(v)], axis=1),
            "trig": tf.concat([tf.sin(v), tf.cos(v), tf.nn.elu(v)], axis=1),
            "logsm": tf.nn.log_softmax(v),
            "red": tf.stack([tf.reduce_max(v, axis=1), tf.reduce_min(v, axis=1), tf.reduce_prod(b, axis=1)], axis=1),
            "args": tf.stack([tf.argmax(v, axis=1), tf.argmin(v, axis=1)], axis=1),
            "cut": tf.tile(tf.pad(tf.slice(v, [0, 1], [-1, 3]), [[0, 0], [1, 2]], constant_values=0.5), [1, 2]),
            "fl": tf.concat([tf.math.floordiv(tf.cast(n, tf.int32), 3), tf.math.floormod(tf.cast(n, tf.int32), 3)], axis=1),
            "sum3": tf.add_n([p, q * 2.0, r * 3.0]),
        }


mlp = MLP()
tf.saved_model.save(mlp, "/out/savedmodel/mlp", signatures={"serving_default": mlp.serve})
loop = Loop()
tf.saved_model.save(loop, "/out/savedmodel/loop", signatures={"serving_default": loop.serve})
ops2 = Ops2()
tf.saved_model.save(ops2, "/out/savedmodel/ops2", signatures={"serving_default": ops2.serve})
flex = Flex()
tf.saved_model.save(flex, "/out/savedmodel/flex", signatures={"serving_default": flex.serve})

fn = tf.saved_model.load("/out/savedmodel/mlp").signatures["serving_default"]
rows = [
    {"x": [0.5, -1.25, 2.0], "k": [3]},
    {"x": [-0.75, 0.1, 0.33], "k": [-4]},
    {"x": [1.5, 2.5, -0.5], "k": [0]},
]
out = []
for r in rows:
    o = fn(x=tf.constant([r["x"]], tf.float32), k=tf.constant([r["k"]], tf.int64))
    out.append({"inputs": r, "outputs": {k: v.numpy()[0].tolist() for k, v in o.items()}})
fn = tf.saved_model.load("/out/savedmodel/loop").signatures["serving_default"]
loops = []
for r in [{"x": [0.5, -0.25], "limit": [3.0]}, {"x": [1.0, 2.0], "limit": [2.0]}, {"x": [-0.125, 0.5], "limit": [40.0]}]:
    o = fn(x=tf.constant([r["x"]], tf.float32), limit=tf.constant(r["limit"], tf.float32))
    loops.append({"inputs": r, "outputs": {k: v.numpy().reshape(-1).tolist() for k, v in o.items()}})
fn = tf.saved_model.load("/out/savedmodel/ops2").signatures["serving_default"]
ops = []
for r in [{"v": [0.5, -1.5, 2.25, -0.75, 3.5, 1.0], "n": [7, -7, 5, -2]}, {"v": [-2.5, 0.25, 1.75, 2.5, -3.0, 0.5], "n": [0, 9, -10, 4]}]:
    o = fn(v=tf.constant([r["v"]], tf.float32), n=tf.constant([r["n"]], tf.int64))
    ops.append({"inputs": r, "outputs": {k: v.numpy().reshape(-1).tolist() for k, v in o.items()}})
with open("/out/tensorflow_predictions.json", "w") as f:
    json.dump({"tensorflow": tf.__version__, "rows": out, "loop": loops, "ops2": ops}, f, indent=1)
