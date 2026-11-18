# Builds conv.tflite, of the convolution, pooling and gather operators,
# with TensorFlow 2.20's TFLiteConverter, and records LiteRT's predictions
# on fixed inputs in litert_conv.json (#1766). Both are Google's:
#   python3.12 -m venv v && v/bin/pip install tensorflow==2.20.0 ai-edge-litert==2.3.0
#   v/bin/python -I gen_conv.py internal/tflitemodel/testdata
import json, os, sys, tempfile
import numpy as np
import tensorflow as tf
from ai_edge_litert.interpreter import Interpreter, OpResolverType

out = sys.argv[1]
rng = np.random.default_rng(11)
def W(*shape):
    return tf.constant(rng.standard_normal(shape).astype(np.float32) * 0.5)
class Conv(tf.Module):
    def __init__(self):
        self.k1, self.b1 = W(3, 3, 2, 3), W(3)
        self.k2 = W(3, 3, 3, 2)
        self.k3, self.b3 = W(2, 2, 2, 4), W(4)
        self.emb = W(7, 3)
    @tf.function(input_signature=[tf.TensorSpec([None, 6, 6, 2], tf.float32, name="image"), tf.TensorSpec([None, 2], tf.int32, name="ids")])
    def __call__(self, img, ids):
        h = tf.nn.relu(tf.nn.conv2d(img, self.k1, strides=1, padding="SAME") + self.b1)
        d = tf.nn.depthwise_conv2d(h, self.k2, strides=[1, 2, 2, 1], padding="VALID")
        m = tf.nn.max_pool2d(h, 2, 2, "SAME")
        a = tf.nn.avg_pool2d(h, 3, 2, "SAME")
        c = tf.nn.relu6(tf.nn.conv2d(img, self.k3, strides=2, padding="VALID", dilations=1) + self.b3)
        e = tf.gather(self.emb, ids)
        return {"depthwise": d, "maxpool": m, "avgpool": a, "strided": c, "embed": e}
mod = Conv()
d = tempfile.mkdtemp()
tf.saved_model.save(mod, d, signatures={"serving_default": mod.__call__})
blob = tf.lite.TFLiteConverter.from_saved_model(d).convert()
open(os.path.join(out, "conv.tflite"), "wb").write(blob)
it = Interpreter(model_content=blob, experimental_op_resolver_type=OpResolverType.BUILTIN_WITHOUT_DEFAULT_DELEGATES)
sig = it.get_signature_runner()
feed = {"image": (rng.standard_normal((3, 6, 6, 2)) * 1.5).astype(np.float32), "ids": rng.integers(0, 7, size=(3, 2)).astype(np.int32)}
res = sig(**feed)
case = {"ops": sorted({x["op_name"] for x in it._get_ops_details()}), "inputs": {k: v.tolist() for k, v in feed.items()},
        "outputs": {k: np.asarray(v, dtype=np.float64).tolist() for k, v in res.items()}}
json.dump({"conv": case}, open(os.path.join(out, "litert_conv.json"), "w"), indent=1)
print(case["ops"])

# STRING tensors: a STRING input reshaped, and a STRING
# constant looked up (strings.tflite, litert_strings.json).
class Str(tf.Module):
    def __init__(self):
        self.names = tf.constant(["zero", "one", "two", "three", "quatre"])
    @tf.function(input_signature=[tf.TensorSpec([None, 3], tf.string, name="words"), tf.TensorSpec([None, 2], tf.int32, name="ids")])
    def __call__(self, words, ids):
        return {"names": tf.gather(self.names, ids), "cols": tf.reshape(words, [-1, 3, 1])}
st = Str()
d = tempfile.mkdtemp()
tf.saved_model.save(st, d, signatures={"serving_default": st.__call__})
blob = tf.lite.TFLiteConverter.from_saved_model(d).convert()
open(os.path.join(out, "strings.tflite"), "wb").write(blob)
it = Interpreter(model_content=blob, experimental_op_resolver_type=OpResolverType.BUILTIN_WITHOUT_DEFAULT_DELEGATES)
sig = it.get_signature_runner()
feed = {"words": np.array([[b"a", b"bb", b"ccc"], [b"x", b"", b"z\xc3\xa9"]], dtype=object), "ids": np.array([[0, 4], [3, 1]], dtype=np.int32)}
res = sig(**feed)
dec = lambda a: np.vectorize(lambda b: b.decode("utf-8"))(np.asarray(a)).tolist()
case = {"ops": sorted({x["op_name"] for x in it._get_ops_details()}), "inputs": {"words": dec(feed["words"]), "ids": feed["ids"].tolist()},
        "outputs": {k: dec(v) for k, v in res.items()}}
json.dump({"strings": case}, open(os.path.join(out, "litert_strings.json"), "w"), indent=1)
print(case["ops"], case["outputs"])
