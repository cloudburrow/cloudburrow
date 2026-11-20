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
           "ragged": a RaggedTensor input or output, or a ragged
                     tf.train.Example feature ("RaggedTensors aren't
                     supported");
           "sparse": a SparseTensor input or a VarLenFeature/SparseFeature
                     tf.train.Example feature, which BigQuery feeds and
                     CloudBurrow's TensorFlow Lite run cannot;
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
    fn = loaded.signatures["serving_default"]
    for side, specs in (("input", fn.structured_input_signature[1]), ("output", fn.structured_outputs)):
        for name, spec in sorted(specs.items()):
            if isinstance(spec, tf.RaggedTensorSpec):
                fail("ragged", "serving_default's %s %s is a RaggedTensor" % (side, name))
            if isinstance(spec, tf.SparseTensorSpec):
                fail("sparse", "serving_default's %s %s is a SparseTensor" % (side, name))
    src = "/model"
    parse = example_parse(fn)
    if parse is not None:
        src = example_rewrite(fn, parse)
    conv = tf.lite.TFLiteConverter.from_saved_model(src, signature_keys=["serving_default"])
    conv.target_spec.supported_ops = [tf.lite.OpsSet.TFLITE_BUILTINS]
    try:
        data = conv.convert()
    except Exception as e:  # noqa: BLE001
        msg = str(e).strip()
        lines = [l.strip() for l in msg.splitlines() if l.strip()]
        # The converter names the ops it has no builtin for on its
        # "Custom ops: ...", "Flex ops: ..." and "TF Select ops: ..." lines.
        named = [l for l in lines if l.startswith(("Custom ops:", "Flex ops:", "TF Select ops:"))]
        if named or "ERROR_NEEDS_FLEX_OPS" in msg or "ERROR_NEEDS_CUSTOM_OPS" in msg or "nor a flex op" in msg:
            fail("flex", "some ops are not TensorFlow Lite builtins (" + "; ".join(named or ["see the converter's log"]) + ")")
        errs = [l for l in lines if "error:" in l] or lines[:3]
        fail("convert", " ".join(errs)[:4000])
    with open("/out/model.tflite", "wb") as f:
        f.write(data)


PARSE_OPS = ("ParseExample", "ParseExampleV2")


def example_parse(fn):
    """The ParseExample op of a serving_default whose one input is a string
    tensor of serialized tf.train.Example protos, with the frozen graph
    holding it, or None for any other signature."""
    import tensorflow as tf
    from tensorflow.python.framework.convert_to_constants import convert_variables_to_constants_v2

    ins = fn.structured_input_signature[1]
    if len(ins) != 1 or list(ins.values())[0].dtype != tf.string or list(ins.values())[0].shape.rank != 1:
        return None
    try:
        frozen = convert_variables_to_constants_v2(fn)
    except Exception:  # noqa: BLE001 - not a graph this rewrite handles; convert it as it is
        return None
    gd = frozen.graph.as_graph_def()
    nodes = {n.name: n for n in gd.node}
    placeholders = {n.name for n in gd.node if n.op == "Placeholder" and n.attr["dtype"].type == tf.string.as_datatype_enum}

    def source(inp):
        n = nodes.get(inp.split(":")[0].lstrip("^"))
        while n is not None and n.op == "Identity":
            n = nodes.get(n.input[0].split(":")[0])
        return n

    parses = [n for n in gd.node if n.op in PARSE_OPS and source(n.input[0]) is not None and source(n.input[0]).name in placeholders]
    if not parses:
        return None
    if len(parses) > 1:
        fail("convert", "serving_default parses its tf.train.Example input %d times; CloudBurrow converts a model that parses it once" % len(parses))
    return frozen, gd, nodes, source, parses[0]


def example_rewrite(fn, parse):
    """Converts a tf.train.Example signature from its parse's dense outputs
    on: as the create-tensorflow page says, "BigQuery ML automatically
    determines the feature names and converts the input BigQuery columns",
    so the model written takes each dense feature as an input of its name,
    dtype and shape, and the parse is gone. Answers the SavedModel's path."""
    import numpy as np
    import tensorflow as tf

    frozen, gd, nodes, source, p = parse

    def strings(inp):
        n = source(inp)
        if n is None or n.op != "Const":
            fail("convert", "the tf.train.Example parse's keys are not constants")
        return [x.decode() for x in np.atleast_1d(tf.make_ndarray(n.attr["value"].tensor))]

    if p.op == "ParseExampleV2":
        nsparse = p.attr["num_sparse"].i
        keys = strings(p.input[3])
        if p.attr["Tragged_value"].list.type or strings(p.input[4]):
            fail("ragged", "the tf.train.Example input has ragged features (%s)" % ", ".join(strings(p.input[4])))
        sparse = strings(p.input[2])
    else:
        nsparse = p.attr["Nsparse"].i
        nd = p.attr["Ndense"].i
        sparse = [strings(i)[0] for i in p.input[2:2 + nsparse]]
        keys = [strings(i)[0] for i in p.input[2 + nsparse:2 + nsparse + nd]]
    if nsparse:
        fail("sparse", "the tf.train.Example input has variable-length (SparseTensor) features: %s" % ", ".join(sparse))
    tdense = [tf.dtypes.as_dtype(t) for t in p.attr["Tdense"].list.type]
    shapes = [tf.TensorShape(sh) for sh in p.attr["dense_shapes"].list.shape]
    for k in keys:
        if not k.replace("_", "a").isalnum() or k[0].isdigit():
            fail("convert", "the tf.train.Example feature %r is not a name BigQuery can give a column" % k)
    out_keys = sorted(fn.structured_outputs.keys())
    out_names = [t.name for t in frozen.outputs]

    def body():
        phs = [tf.compat.v1.placeholder(dt, [None] + sh.as_list(), name=k) for k, dt, sh in zip(keys, tdense, shapes)]
        tf.graph_util.import_graph_def(gd, input_map={"%s:%d" % (p.name, 3 * nsparse + i): ph for i, ph in enumerate(phs)}, name="m")

    wrapped = tf.compat.v1.wrap_function(body, [])
    g = wrapped.graph
    pruned = wrapped.prune([g.get_tensor_by_name(k + ":0") for k in keys],
                           {k: g.get_tensor_by_name("m/" + n) for k, n in zip(out_keys, out_names)})
    module = tf.Module()
    module.serve = pruned
    dst = "/tmp/example_model"
    tf.saved_model.save(module, dst, signatures={"serving_default": pruned})
    return dst


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
