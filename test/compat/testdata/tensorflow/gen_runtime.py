"""Writes the SavedModels TestBigQueryMLTensorFlowRuntime imports (#1775):
models TensorFlow Lite's builtin operators cannot take (they need Flex,
SELECT_TF_OPS), which BigQuery runs with TensorFlow itself, and
TensorFlow's own serving_default output and Integrated Gradients for
their rows.

The test runs it, on an --internal Docker network (no route out), in
CloudBurrow's TensorFlow converter image (deploy/tfconvert: Google's
tensorflow/tensorflow:2.20.0, pinned by digest, Apache-2.0):

  docker run --rm --network <internal> --entrypoint python3 \
    -v <this folder>:/w:ro -v <out>:/out cloudburrow/tfconvert:<tag> -I /w/gen_runtime.py /out

<out>/exflex:     a tf.train.Example signature: one string input of
                  serialized Examples, parsed into text STRING[] and
                  a FLOAT32[3]; score FLOAT32[None] = a . w + the number
                  of words tf.strings.split finds in text (StringSplitV2:
                  Flex), words INT64[None] that number.
<out>/sparseflex: SparseTensor inputs (tf.compat.v1's sparse_placeholder,
                  coo_sparse TensorInfos): v FLOAT32[None,4]; chars
                  INT64[None] = the length of tf.strings.as_string of each
                  of v's dense values, summed (AsString: Flex), total
                  FLOAT32[None] = tf.sparse.reduce_sum of v.
<out>/mlp:        a dense x FLOAT32[None,3] -> tanh layer -> softmax
                  FLOAT32[None,3] (probs), whose ML.EXPLAIN_PREDICT the
                  test checks, with a tf.strings.as_string of x beside it
                  (Flex) so it runs on TensorFlow too.
<out>/predictions.json: each row's inputs, TensorFlow's outputs and, for
                  exflex and mlp, Integrated Gradients of the first output
                  ("score", "probs") from a baseline of zeros of each float
                  input over 15 steps (TensorFlow's integrated gradients
                  tutorial: the trapezoidal Riemann sum), of the largest
                  element of an ARRAY output; its baseline and prediction.
"""

import json
import os
import sys

import numpy as np
import tensorflow as tf

os.umask(0)
out = sys.argv[1]
rng = np.random.default_rng(1775)
W = tf.constant([0.5, -1.0, 2.0], tf.float32)
STEPS = 15


class ExFlex(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None], tf.string, name="examples")])
    def serve(self, examples):
        f = tf.io.parse_example(examples, {"text": tf.io.FixedLenFeature([], tf.string),
                                           "a": tf.io.FixedLenFeature([3], tf.float32)})
        n = tf.strings.split(f["text"]).row_lengths()
        return {"score": tf.reduce_sum(f["a"] * f["a"] * W, axis=1) + tf.cast(n, tf.float32), "words": n}


class MLP(tf.Module):
    def __init__(self):
        self.w = tf.Variable(rng.normal(size=(3, 3)).astype(np.float32))
        self.b = tf.Variable(rng.normal(size=3).astype(np.float32))

    @tf.function(input_signature=[tf.TensorSpec([None, 3], tf.float32, name="x")])
    def serve(self, x):
        return {"probs": tf.nn.softmax(tf.tanh(tf.matmul(x, self.w) + self.b)),
                "text": tf.strings.reduce_join(tf.strings.as_string(x, precision=2), axis=1, separator=",")}


def sparseflex():
    v = tf.compat.v1.sparse_placeholder(tf.float32, [None, 4], name="v")
    d = tf.sparse.to_dense(v)
    return {"v": v}, {"chars": tf.reduce_sum(tf.cast(tf.strings.length(tf.strings.as_string(d, precision=3)), tf.int64), axis=1),
                      "total": tf.sparse.reduce_sum(v, axis=1)}


def save_v1(name, build):
    g = tf.Graph()
    with g.as_default():
        ins, outs = build()
        with tf.compat.v1.Session() as s:
            b = tf.compat.v1.saved_model.Builder(os.path.join(out, name))
            b.add_meta_graph_and_variables(s, ["serve"], signature_def_map={
                "serving_default": tf.compat.v1.saved_model.predict_signature_def(ins, outs)})
            b.save()


ex, mlp = ExFlex(), MLP()
tf.saved_model.save(ex, os.path.join(out, "exflex"), signatures={"serving_default": ex.serve})
tf.saved_model.save(mlp, os.path.join(out, "mlp"), signatures={"serving_default": mlp.serve})
save_v1("sparseflex", sparseflex)


def flat(o):
    return {k: [z.decode() if isinstance(z, bytes) else float(z) for z in np.asarray(v.numpy()).ravel()] for k, v in o.items()}


def ig(f, x, output):
    """Integrated Gradients of f's output at x (a [1, ...] array) from
    zeros, its baseline and prediction, of the largest element."""
    y = np.asarray(f(x)[output].numpy()).ravel()
    idx = int(np.argmax(y))
    grads = []
    for a in np.linspace(0.0, 1.0, STEPS + 1):
        xa = tf.constant((a * x).astype(x.dtype))
        with tf.GradientTape() as tape:
            tape.watch(xa)
            o = tf.reshape(tf.cast(f(xa)[output], tf.float64), [-1])[idx]
        grads.append(np.asarray(tape.gradient(o, xa).numpy(), np.float64))
    gs = np.stack(grads)
    avg = (gs[:-1] + gs[1:]).mean(axis=0) / 2.0
    base = float(np.asarray(f(np.zeros_like(x))[output].numpy()).ravel()[idx])
    return float((x.astype(np.float64) * avg).sum()), base, float(y[idx])


pred = {"exflex": [], "sparseflex": [], "mlp": []}
ex_fn = tf.saved_model.load(os.path.join(out, "exflex")).signatures["serving_default"]
words = ["good", "bad", "plot", "movie", "fine"]
for i in range(20):
    text = " ".join(words[int(j)] for j in rng.integers(0, len(words), size=i % 6))
    a = rng.normal(size=3).astype(np.float32)

    def f(av, text=text):
        e = tf.train.Example(features=tf.train.Features(feature={
            "text": tf.train.Feature(bytes_list=tf.train.BytesList(value=[text.encode()])),
            "a": tf.train.Feature(float_list=tf.train.FloatList(value=np.asarray(av, np.float32).ravel().tolist()))}))
        return ex_fn(examples=tf.constant([e.SerializeToString()]))

    o = f(a)
    row = {"inputs": {"text": text, "a": a.tolist()}, "outputs": flat(o)}
    # The gradient of a through the parse is not TensorFlow's to take (the
    # Example is bytes), so the reference differentiates the parse's
    # output: the same function of a.
    def g(av, n=float(len(text.split()))):
        av = tf.convert_to_tensor(av, tf.float32)
        return {"score": tf.reduce_sum(av * av * W, axis=1) + n}

    row["ig"] = dict(zip(("a", "baseline", "prediction"), ig(g, a[None, :], "score")))
    pred["exflex"].append(row)

sf_fn = tf.saved_model.load(os.path.join(out, "sparseflex")).signatures["serving_default"]
for i in range(6):
    v = rng.normal(size=i % 5).astype(np.float32)
    n = len(v)
    o = sf_fn(v_indices=tf.constant([[0, j] for j in range(n)], tf.int64, shape=[n, 2]), v_values=tf.constant(v),
              v_dense_shape=tf.constant([1, 4], tf.int64))
    pred["sparseflex"].append({"inputs": {"v": v.tolist()}, "outputs": flat(o)})

mlp_loaded = tf.saved_model.load(os.path.join(out, "mlp"))  # holds its variables
mlp_fn = mlp_loaded.signatures["serving_default"]
for i in range(5):
    x = rng.normal(size=(1, 3)).astype(np.float32)
    row = {"inputs": {"x": x[0].tolist()}, "outputs": flat(mlp_fn(x=tf.constant(x)))}
    row["ig"] = dict(zip(("x", "baseline", "prediction"), ig(lambda z: mlp_fn(x=tf.convert_to_tensor(z, tf.float32)), x, "probs")))
    pred["mlp"].append(row)

with open(os.path.join(out, "predictions.json"), "w") as f:
    json.dump(pred, f, indent=1)
