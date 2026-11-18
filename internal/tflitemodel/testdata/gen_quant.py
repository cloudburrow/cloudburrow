# Builds two quantized models with TensorFlow 2.20's TFLiteConverter, and
# records LiteRT's predictions in litert_quant.json (#1766): qdyn.tflite,
# dynamic range quantization (int8 weights, float input and output), and
# qint8.tflite, full integer quantization (int8 input, output and
# activations, from a representative dataset). Both tools are Google's:
#   python3.12 -m venv v && v/bin/pip install tensorflow==2.20.0 ai-edge-litert==2.3.0
#   v/bin/python -I gen_quant.py internal/tflitemodel/testdata
import json, os, sys, tempfile
import numpy as np
import tensorflow as tf
from ai_edge_litert.interpreter import Interpreter, OpResolverType

out = sys.argv[1]
rng = np.random.default_rng(23)
def W(*shape):
    return tf.constant(rng.standard_normal(shape).astype(np.float32) * 0.4)
class Net(tf.Module):
    def __init__(self):
        self.k, self.kb = W(3, 3, 1, 4), W(4)
        self.w1, self.b1 = W(36, 16), W(16)
        self.w2, self.b2 = W(16, 3), W(3)
    @tf.function(input_signature=[tf.TensorSpec([1, 6, 6, 1], tf.float32, name="image")])
    def __call__(self, img):
        h = tf.nn.relu(tf.nn.conv2d(img, self.k, strides=2, padding="SAME") + self.kb)
        h = tf.reshape(h, [1, 36])
        h = tf.nn.relu(tf.matmul(h, self.w1) + self.b1)
        return {"logits": tf.matmul(h, self.w2) + self.b2}
net = Net()
d = tempfile.mkdtemp()
tf.saved_model.save(net, d, signatures={"serving_default": net.__call__})
samples = [(rng.standard_normal((1, 6, 6, 1))).astype(np.float32) for _ in range(64)]
c = tf.lite.TFLiteConverter.from_saved_model(d)
c.optimizations = [tf.lite.Optimize.DEFAULT]
dyn = c.convert()
c = tf.lite.TFLiteConverter.from_saved_model(d)
c.optimizations = [tf.lite.Optimize.DEFAULT]
c.representative_dataset = lambda: ([s] for s in samples)
c.target_spec.supported_ops = [tf.lite.OpsSet.TFLITE_BUILTINS_INT8]
c.inference_input_type, c.inference_output_type = tf.int8, tf.int8
full = c.convert()
cases = {}
for name, blob in {"qdyn": dyn, "qint8": full}.items():
    open(os.path.join(out, name + ".tflite"), "wb").write(blob)
    it = Interpreter(model_content=blob, experimental_op_resolver_type=OpResolverType.BUILTIN_WITHOUT_DEFAULT_DELEGATES)
    sig = it.get_signature_runner()
    det = sig.get_input_details()["image"]
    x = samples[5]
    if name == "qint8":
        s, z = det["quantization"]
        x = np.clip(np.round(x / s) + z, -128, 127).astype(np.int8)
    res = sig(image=x)
    o = np.asarray(res["logits"], dtype=np.float64)
    # The tolerance: two steps of an int8 output's scale, or 5% of a float
    # output's largest value (LiteRT's hybrid kernels quantize the
    # activations as they run).
    tol = 2.0 if name == "qint8" else 0.05 * float(np.max(np.abs(o)))
    cases[name] = {"ops": sorted({x["op_name"] for x in it._get_ops_details()}), "tol": tol,
                   "inputs": {"image": np.asarray(x).tolist()}, "outputs": {"logits": o.tolist()}}
json.dump(cases, open(os.path.join(out, "litert_quant.json"), "w"), indent=1)
for k, v in cases.items():
    print(k, v["ops"], v["tol"])
