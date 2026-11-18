# Builds the image fixtures of BigQuery ML's ML.DECODE_IMAGE,
# ML.RESIZE_IMAGE, ML.CONVERT_COLOR_SPACE, ML.CONVERT_IMAGE_TYPE and
# ML.PREDICT of an imported image model over an object table (#1798),
# with TensorFlow 2.20 (Google's), and records TensorFlow's own output for
# them in tensorflow_image.json:
#   python3.12 -m venv v && v/bin/pip install tensorflow==2.20.0 ai-edge-litert==2.3.0
#   v/bin/python -I gen_image.py internal/tflitemodel/testdata
#
# image_small.png (6x5 RGB), image_big.png (8x10 RGB), image_gray.png
# (4x3, one channel) and image.jpg (6x5 RGB, JPEG) are random pixels
# TensorFlow encoded; decode is tf.io.decode_image then
# tf.image.convert_image_dtype to float32 (the [0, 1] scale), resize
# tf.image.resize (bilinear), the colour spaces tf.image.rgb_to_hsv /
# rgb_to_grayscale / rgb_to_yiq / rgb_to_yuv, and the INT64 form
# tf.image.convert_image_dtype to uint8. image.tflite and
# savedmodel/image are one model, input image FLOAT32[None,6,5,3], output
# probs (softmax, [None,3]); predict is TensorFlow's serving_default
# output of it (and LiteRT's, litert, for the .tflite).
import json, os, sys, shutil
import numpy as np
import tensorflow as tf
from ai_edge_litert.interpreter import Interpreter, OpResolverType

out = sys.argv[1]
rng = np.random.default_rng(1798)
def px(*shape):
    return tf.constant(rng.integers(0, 256, size=shape).astype(np.uint8))
files = {
    "image_small.png": tf.io.encode_png(px(6, 5, 3)).numpy(),
    "image_big.png": tf.io.encode_png(px(8, 10, 3)).numpy(),
    "image_gray.png": tf.io.encode_png(px(4, 3, 1)).numpy(),
    "image.jpg": tf.io.encode_jpeg(px(6, 5, 3), quality=95).numpy(),
}
for name, b in files.items():
    open(os.path.join(out, name), "wb").write(b)

def decode(b):
    return tf.image.convert_image_dtype(tf.io.decode_image(b, expand_animations=False), tf.float32)
def struct(img):
    a = np.asarray(img, dtype=np.float64)
    return {"shape": list(a.shape), "data": a.reshape(-1).tolist()}

res = {"decode": {n: struct(decode(b)) for n, b in files.items()}}
# tf.io.decode_image decodes JPEG with libjpeg's fast integer IDCT; the
# accurate one is what Go's image/jpeg computes.
res["decode_jpeg_accurate"] = struct(tf.image.convert_image_dtype(tf.io.decode_jpeg(files["image.jpg"], dct_method="INTEGER_ACCURATE"), tf.float32))
big = decode(files["image_big.png"])
small = decode(files["image_small.png"])
res["resize"] = {
    "big_6x5": struct(tf.image.resize(big, [6, 5], method="bilinear")),
    "big_4x4_preserve": struct(tf.image.resize(big, [4, 4], method="bilinear", preserve_aspect_ratio=True)),
    "small_9x7": struct(tf.image.resize(small, [9, 7], method="bilinear")),
}
res["color"] = {
    "HSV": struct(tf.image.rgb_to_hsv(small)),
    "GRAYSCALE": struct(tf.image.rgb_to_grayscale(small)),
    "YIQ": struct(tf.image.rgb_to_yiq(small)),
    "YUV": struct(tf.image.rgb_to_yuv(small)),
}
res["convert_type"] = struct(tf.image.convert_image_dtype(small, tf.uint8))

class Img(tf.Module):
    def __init__(self):
        g = np.random.default_rng(98)
        self.k = tf.Variable(g.standard_normal((3, 3, 3, 4)).astype(np.float32))
        self.b = tf.Variable(g.standard_normal((4,)).astype(np.float32))
        self.w = tf.Variable(g.standard_normal((4, 3)).astype(np.float32))
    @tf.function(input_signature=[tf.TensorSpec([None, 6, 5, 3], tf.float32, name="image")])
    def __call__(self, image):
        h = tf.nn.relu(tf.nn.conv2d(image, self.k, strides=1, padding="SAME") + self.b)
        m = tf.reduce_mean(h, axis=[1, 2])
        return {"probs": tf.nn.softmax(tf.matmul(m, self.w) * 0.2)}
mod = Img()
sm = os.path.join(out, "savedmodel", "image")
shutil.rmtree(sm, ignore_errors=True)
tf.saved_model.save(mod, sm, signatures={"serving_default": mod.__call__})
blob = tf.lite.TFLiteConverter.from_saved_model(sm).convert()
open(os.path.join(out, "image.tflite"), "wb").write(blob)
serve = tf.saved_model.load(sm).signatures["serving_default"]
it = Interpreter(model_content=blob, experimental_op_resolver_type=OpResolverType.BUILTIN_WITHOUT_DEFAULT_DELEGATES)
sig = it.get_signature_runner()
inputs = {"small": small, "big_6x5": tf.image.resize(big, [6, 5], method="bilinear"), "jpg": decode(files["image.jpg"])}
res["predict"] = {}
for k, v in inputs.items():
    x = tf.expand_dims(v, 0)
    res["predict"][k] = {
        "tensorflow": np.asarray(serve(image=x)["probs"], dtype=np.float64)[0].tolist(),
        "litert": np.asarray(sig(image=x.numpy())["probs"], dtype=np.float64)[0].tolist(),
    }
res["ops"] = sorted({x["op_name"] for x in it._get_ops_details()})
json.dump(res, open(os.path.join(out, "tensorflow_image.json"), "w"), indent=1)
print(res["ops"], res["predict"])
