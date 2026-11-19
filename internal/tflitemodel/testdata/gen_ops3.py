# Builds ops3.tflite and lstm.tflite, of the builtin operators ops3.go
# adds (#1775), with TensorFlow 2.20's TFLiteConverter, and records
# LiteRT's predictions on fixed inputs in litert_ops3.json. All Google's:
#   python3.12 -m venv v && v/bin/pip install tensorflow==2.20.0 ai-edge-litert==2.3.0
#   v/bin/python -I gen_ops3.py internal/tflitemodel/testdata
import json, os, sys, tempfile
import numpy as np
import tensorflow as tf
from ai_edge_litert.interpreter import Interpreter, OpResolverType

out = sys.argv[1]
rng = np.random.default_rng(23)
def W(*shape):
    return tf.constant(rng.standard_normal(shape).astype(np.float32) * 0.5)

class Ops3(tf.Module):
    def __init__(self):
        self.tk = W(3, 3, 4, 2)  # TRANSPOSE_CONV: [h, w, out, in]
        self.alpha = W(1, 1, 2)
        self.emb = W(6, 3)
        self.bounds = [-1.0, 0.0, 0.5, 2.0]
    @tf.function(input_signature=[tf.TensorSpec([2, 4, 4, 2], tf.float32, name="image"),
                                  tf.TensorSpec([2, 3], tf.int32, name="ids"),
                                  tf.TensorSpec([2, 5], tf.float32, name="x")])
    def __call__(self, img, ids, x):
        r = {}
        r["tconv"] = tf.nn.conv2d_transpose(img, self.tk, [2, 8, 8, 4], strides=2, padding="SAME")
        r["tconv_valid"] = tf.nn.conv2d_transpose(img, self.tk, [2, 9, 9, 4], strides=2, padding="VALID")
        r["prelu"] = tf.maximum(img, 0.0) + self.alpha * tf.minimum(img, 0.0)
        r["mirror_reflect"] = tf.pad(x, [[0, 0], [2, 1]], mode="REFLECT")
        r["mirror_symmetric"] = tf.pad(x, [[1, 0], [1, 2]], mode="SYMMETRIC")
        r["d2s"] = tf.nn.depth_to_space(tf.concat([img, img * 2], -1), 2)
        r["s2d"] = tf.nn.space_to_depth(img, 2)
        r["s2b"] = tf.space_to_batch_nd(img, [2, 2], [[0, 0], [1, 1]])
        r["b2s"] = tf.batch_to_space(tf.concat([img, img + 1], 0), [2, 1], [[0, 1], [0, 0]])
        r["l2norm"] = tf.math.l2_normalize(x, axis=-1)
        r["lrn"] = tf.nn.local_response_normalization(img, depth_radius=1, bias=1.5, alpha=0.3, beta=0.6)
        k = tf.math.floormod(ids, 4)
        r["gather_nd"] = tf.gather_nd(tf.reshape(x[:, :4], [2, 2, 2]), tf.stack([k[:, :2] % 2, k[:, 1:] % 2], -1))
        r["one_hot"] = tf.one_hot(k, 4, on_value=2.5, off_value=-1.0)
        # [1, n]: a row-major ARRAY of one row in BigQuery, not a batch of n.
        r["range"] = tf.reshape(tf.cast(tf.range(k[0, 0], k[0, 0] + 7, 2), tf.float32), [1, -1])
        r["reverse"] = tf.reverse(x, [1])
        r["broadcast_to"] = tf.broadcast_to(x[:, :1], [2, 3])
        r["cumsum"] = tf.cumsum(x, axis=1)
        r["cumsum_ex_rev"] = tf.cumsum(x, axis=1, exclusive=True, reverse=True)
        r["any"] = tf.cast(tf.reduce_any(x > 0.5, axis=1), tf.float32)
        r["all"] = tf.cast(tf.reduce_all(x > -1.0, axis=1), tf.float32)
        v, i = tf.math.top_k(x, 3)
        r["topk_values"], r["topk_indices"] = v, tf.cast(i, tf.float32)
        r["sign"] = tf.sign(x)
        r["atan2"] = tf.math.atan2(x, x[::-1])
        r["embed"] = tf.nn.embedding_lookup(self.emb, k)
        r["segsum"] = tf.math.unsorted_segment_sum(x, k[:, 0] % 2, 3)
        r["segmax"] = tf.math.unsorted_segment_max(tf.transpose(x), [0, 1, 0, 2, 1], 3)
        r["segmin"] = tf.math.unsorted_segment_min(tf.transpose(x), [0, 1, 0, 2, 1], 3)
        r["segprod"] = tf.math.unsorted_segment_prod(tf.transpose(x), [0, 1, 0, 2, 1], 3)
        r["bucketize"] = tf.cast(tf.raw_ops.Bucketize(input=x, boundaries=self.bounds), tf.float32)
        r["scatter_nd"] = tf.scatter_nd(tf.reshape(k[:, :2] % 3, [4, 1]), tf.reshape(x[:, :4], [4, 2]), [3, 2])
        r["segment_sum"] = tf.math.segment_sum(tf.transpose(x), [0, 0, 1, 1, 2])
        r["matrix_diag"] = tf.linalg.diag(x)
        r["relu_0_to_1"] = tf.minimum(tf.maximum(x, 0.0), 1.0)
        r["rank"] = tf.cast(tf.rank(tf.reshape(x, [-1, 1, 5])), tf.float32) + 0 * tf.reduce_sum(x)
        return r

# A Keras PReLU (PRELU), and a Keras LSTM, which TFLiteConverter.from_keras_model
# writes as a WHILE loop of builtin operators; a bidirectional one adds
# REVERSE_V2.
def lstm_model():
    seq = tf.keras.Input([5, 3], batch_size=2, name="seq")
    h = tf.keras.layers.LSTM(4, return_sequences=True, name="sequence")(seq)
    b = tf.keras.layers.Bidirectional(tf.keras.layers.LSTM(2, return_sequences=True), name="bidi")(h)
    last = tf.keras.layers.LSTM(3, name="last")(b)
    p = tf.keras.layers.PReLU(alpha_initializer=tf.keras.initializers.RandomUniform(0.1, 0.9, seed=5), shared_axes=[1], name="prelu")(seq)
    return tf.keras.Model(seq, {"sequence": h, "bidi": b, "last": last, "prelu": p})

def build(mod, name, feed):
    if isinstance(mod, tf.keras.Model):
        blob = tf.lite.TFLiteConverter.from_keras_model(mod).convert()
    else:
        d = tempfile.mkdtemp()
        tf.saved_model.save(mod, d, signatures={"serving_default": mod.__call__})
        blob = tf.lite.TFLiteConverter.from_saved_model(d).convert()
    open(os.path.join(out, name + ".tflite"), "wb").write(blob)
    it = Interpreter(model_content=blob, experimental_op_resolver_type=OpResolverType.BUILTIN_WITHOUT_DEFAULT_DELEGATES)
    sig = it.get_signature_runner()
    res = sig(**feed)
    case = {"ops": sorted({x["op_name"] for x in it._get_ops_details()}), "inputs": {k: v.tolist() for k, v in feed.items()},
            "outputs": {k: np.asarray(v, dtype=np.float64).tolist() for k, v in res.items()}}
    print(name, case["ops"])
    return case

cases = {}
cases["ops3"] = build(Ops3(), "ops3", {"image": (rng.standard_normal((2, 4, 4, 2)) * 1.5).astype(np.float32),
                                       "ids": rng.integers(0, 9, size=(2, 3)).astype(np.int32),
                                       "x": (rng.standard_normal((2, 5)) * 1.2).astype(np.float32)})
cases["lstm"] = build(lstm_model(), "lstm", {"seq": rng.standard_normal((2, 5, 3)).astype(np.float32)})
json.dump(cases, open(os.path.join(out, "litert_ops3.json"), "w"), indent=1)
