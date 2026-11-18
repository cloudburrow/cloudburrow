"""CloudBurrow's TensorFlow SavedModel converter (#1775).

convert: reads the SavedModel at /model and writes /out/model.tflite, the
         TensorFlow Lite flatbuffer Google's TFLiteConverter makes of its
         serving_default signature with the builtin operators alone, or
         /out/error.json, {"kind": ..., "message": ...}, when it cannot:
           "format": /model is not a SavedModel;
           "signature": it has no serving_default signature;
           "custom": its graph uses an op TensorFlow does not register
                     (a custom or tf.contrib op);
           "flex": a graph needs TensorFlow ops (Flex, SELECT_TF_OPS) that
                   TensorFlow Lite's builtins do not have;
           "convert": any other converter failure.
predict: reads JSON {"name": [values...]} on stdin and prints TensorFlow's
         own serving_default output for it (the reference tests check
         CloudBurrow's TensorFlow Lite run against).
"""

import json
import os
import sys


def fail(kind, message):
    with open("/out/error.json", "w") as f:
        json.dump({"kind": kind, "message": message}, f)
    sys.exit(0)


def convert():
    if not (os.path.exists("/model/saved_model.pb") or os.path.exists("/model/saved_model.pbtxt")):
        fail("format", "no saved_model.pb or saved_model.pbtxt: TensorFlow models must be in SavedModel format")
    import tensorflow as tf

    try:
        loaded = tf.saved_model.load("/model")
    except Exception as e:  # noqa: BLE001 - the loader's own message is the answer
        msg = str(e).strip()
        if "Op type not registered" in msg:
            fail("custom", [l for l in msg.splitlines() if "Op type not registered" in l][0].strip())
        fail("format", "TensorFlow could not load the SavedModel: " + (msg.splitlines() or [type(e).__name__])[0])
    if "serving_default" not in loaded.signatures:
        fail("signature", "the SavedModel has no serving_default signature (it has: %s)" % (", ".join(sorted(loaded.signatures.keys())) or "none"))
    conv = tf.lite.TFLiteConverter.from_saved_model("/model", signature_keys=["serving_default"])
    conv.target_spec.supported_ops = [tf.lite.OpsSet.TFLITE_BUILTINS]
    try:
        data = conv.convert()
    except Exception as e:  # noqa: BLE001
        msg = str(e).strip()
        lines = [l.strip() for l in msg.splitlines() if l.strip()]
        # The converter names the ops it has no builtin for on its
        # "Custom ops: ..." and "Flex ops: ..." lines.
        named = [l for l in lines if l.startswith("Custom ops:") or l.startswith("Flex ops:")]
        if named or "ERROR_NEEDS_FLEX_OPS" in msg or "ERROR_NEEDS_CUSTOM_OPS" in msg or "nor a flex op" in msg:
            fail("flex", "some ops are not TensorFlow Lite builtins (" + "; ".join(named or ["see the converter's log"]) + ")")
        errs = [l for l in lines if "error:" in l] or lines[:3]
        fail("convert", " ".join(errs)[:4000])
    with open("/out/model.tflite", "wb") as f:
        f.write(data)


def predict():
    import numpy as np
    import tensorflow as tf

    req = json.load(sys.stdin)
    fn = tf.saved_model.load("/model").signatures["serving_default"]
    args = {}
    for name, spec in fn.structured_input_signature[1].items():
        args[name] = tf.constant(np.array(req[name]), dtype=spec.dtype)
    out = fn(**args)
    json.dump({k: v.numpy().tolist() if v.dtype != tf.string else [s.decode() for s in v.numpy().ravel()] for k, v in out.items()}, sys.stdout)


if __name__ == "__main__":
    {"convert": convert, "predict": predict}[sys.argv[1] if len(sys.argv) > 1 else "convert"]()
