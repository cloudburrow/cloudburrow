# Builds the test .tflite models with TensorFlow 2.20's TFLiteConverter and
# records LiteRT's predictions on fixed inputs (#1755). Both are Google's:
#   python3.12 -m venv v && v/bin/pip install tensorflow==2.20.0 ai-edge-litert==2.3.0
#   v/bin/python -I gen.py internal/tflitemodel/testdata
import json, os, sys
import numpy as np
import tensorflow as tf
from ai_edge_litert.interpreter import Interpreter

out = sys.argv[1]
tf.keras.utils.set_random_seed(7)
rng = np.random.default_rng(3)

import tempfile
def conv_saved(mod, fn):
    d = tempfile.mkdtemp()
    tf.saved_model.save(mod, d, signatures={"serving_default": fn})
    return tf.lite.TFLiteConverter.from_saved_model(d).convert()

def conv_keras(m, specs, outs):
    mod = tf.Module()
    mod.m = m
    @tf.function(input_signature=specs)
    def serve(*xs):
        r = m(list(xs) if len(xs) > 1 else xs[0])
        r = r if isinstance(r, (list, tuple)) else [r]
        return dict(zip(outs, r))
    return conv_saved(mod, serve)

def conv_fn(fn, sigs):
    c = tf.lite.TFLiteConverter.from_concrete_functions([fn.get_concrete_function(*sigs)], fn)
    return c.convert()

models = {}
def W(*shape):
    return tf.constant(rng.standard_normal(shape).astype(np.float32) * 0.7)
class MLP(tf.Module):
    def __init__(self):
        self.w1, self.b1, self.w2, self.b2, self.w3, self.b3 = W(4, 8), W(8), W(8, 5), W(5), W(5, 3), W(3)
    @tf.function(input_signature=[tf.TensorSpec([None, 4], tf.float32, name="features")])
    def __call__(self, x):
        h = tf.nn.relu(tf.matmul(x, self.w1) + self.b1)
        h = tf.tanh(tf.matmul(h, self.w2) + self.b2)
        return {"probs": tf.nn.softmax(tf.matmul(h, self.w3) + self.b3)}
mlp = MLP()
models["mlp"] = (conv_saved(mlp, mlp.__call__), {"features": (4,)})
class Multi(tf.Module):
    def __init__(self):
        self.w1, self.b1, self.ws, self.bs, self.wl, self.bl = W(3, 6), W(6), W(6, 1), W(1), W(6, 2), W(2)
    @tf.function(input_signature=[tf.TensorSpec([None, 1], tf.float32, name="a"), tf.TensorSpec([None, 2], tf.float32, name="b")])
    def __call__(self, a, b):
        h = tf.sigmoid(tf.matmul(tf.concat([a, b], axis=1), self.w1) + self.b1)
        return {"score": tf.sigmoid(tf.matmul(h, self.ws) + self.bs), "logits": tf.matmul(h, self.wl) + self.bl, "pair": tf.stack([a[:, 0] * 2.0, b[:, 1] / 4.0], axis=1)}
multi = Multi()
models["multi"] = (conv_saved(multi, multi.__call__), {"a": (1,), "b": (2,)})
# 3. Element-wise and shape ops.
class Ops(tf.Module):
    @tf.function(input_signature=[tf.TensorSpec([None, 6], tf.float32, name="x"), tf.TensorSpec([None, 1], tf.int32, name="n")])
    def __call__(self, x, n):
        y = tf.exp(x * 0.5) - x / 3.0 + tf.maximum(x, 0.25) - tf.minimum(x, -0.5)
        y = y + tf.cast(n, tf.float32)
        z = tf.reshape(y, [-1, 2, 3])
        m = tf.reduce_mean(z, axis=1)
        q = tf.sqrt(tf.abs(m)) + tf.math.log(1.0 + tf.square(m))
        return {"y": tf.nn.relu6(y), "q": q, "s": tf.reduce_sum(q, axis=1)}
ops = Ops()
models["ops"] = (conv_saved(ops, ops.__call__), {"x": (6,), "n": (1,)})

cases = {}
for name, (blob, ins) in models.items():
    open(os.path.join(out, name + ".tflite"), "wb").write(blob)
    from ai_edge_litert.interpreter import OpResolverType
    it = Interpreter(model_content=blob, experimental_op_resolver_type=OpResolverType.BUILTIN_WITHOUT_DEFAULT_DELEGATES)
    sig = it.get_signature_runner()
    batch = 5
    feed = {}
    for k, shp in ins.items():
        if name == "ops" and k == "n":
            feed[k] = rng.integers(-3, 4, size=(batch,) + shp).astype(np.int32)
        else:
            feed[k] = (rng.standard_normal((batch,) + shp) * 1.5).astype(np.float32)
    res = sig(**feed)
    ops_used = sorted({d["op_name"] for d in it._get_ops_details()})
    cases[name] = {"ops": ops_used,
                   "inputs": {k: v.tolist() for k, v in feed.items()},
                   "outputs": {k: np.asarray(v, dtype=np.float64).tolist() for k, v in res.items()},
                   "signature": it.get_signature_list()}
json.dump(cases, open(os.path.join(out, "litert_predictions.json"), "w"), indent=1)
for k, v in cases.items():
    print(k, v["ops"], v["signature"])
