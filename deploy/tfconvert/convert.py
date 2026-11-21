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
           "sparse": a SparseTensor input of a rank other than 2 or a
                     SparseTensor output, which no BigQuery column fills;
           "convert": any other converter failure.
SparseTensor inputs (a serving_default input whose TensorInfo is
coo_sparse, as tf.compat.v1.saved_model.predict_signature_def writes a
SparseTensor) and VarLenFeature tf.train.Example features: "You can pass
SparseTensors as dense arrays and BigQuery ML automatically converts them
into Sparse format", so the model written takes each as a dense
[batch, n] input of its name, the row's ARRAY, and builds the SparseTensor
of its n values at indices [0, 0] .. [0, n-1] in the graph (sparse_dense).
The sparse operators TensorFlow Lite has no builtin for and whose answer
a dense operator gives exactly (SparseReduceSum, SparseTensorDenseMatMul,
SparseSegmentSum/Mean/SqrtN) are rewritten to builtins (lower_sparse).
The model's metadata "cloudburrow_sparse_inputs" names those inputs, with
the SparseTensor's dense shape after the batch (-1 where free).

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
    sparse_in, sparse_out = coo_sparse_ports()
    if sparse_out:
        fail("sparse", "serving_default's output %s is a SparseTensor, which no BigQuery column holds" % ", ".join(sparse_out))
    src = "/model"
    sparse = {}
    parse = example_parse(fn)
    if parse is not None:
        src, sparse = example_rewrite(fn, parse)
    elif sparse_in:
        src, sparse = sparse_rewrite(fn, sparse_in)
    src = lower_sparse(src)
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
    if sparse:
        data = with_metadata(data, "cloudburrow_sparse_inputs", json.dumps(sparse, sort_keys=True).encode())
    with open("/out/model.tflite", "wb") as f:
        f.write(data)


def coo_sparse_ports():
    """The serving_default inputs and outputs whose TensorInfo is
    coo_sparse (a SparseTensor, as tf.compat.v1's build_tensor_info and
    predict_signature_def write one): {name: (dtype, dims)} and [names]."""
    from google.protobuf import text_format
    from tensorflow.core.protobuf import saved_model_pb2

    sm = saved_model_pb2.SavedModel()
    if os.path.exists("/model/saved_model.pb"):
        with open("/model/saved_model.pb", "rb") as f:
            sm.ParseFromString(f.read())
    else:
        with open("/model/saved_model.pbtxt") as f:
            text_format.Parse(f.read(), sm)
    ins, outs = {}, []
    for mg in sm.meta_graphs:
        sig = mg.signature_def.get("serving_default")
        if sig is None:
            continue
        for name, info in sig.inputs.items():
            if info.WhichOneof("encoding") == "coo_sparse":
                ins[name] = (info.dtype, [d.size for d in info.tensor_shape.dim] if not info.tensor_shape.unknown_rank else None)
        outs += [n for n, info in sig.outputs.items() if info.WhichOneof("encoding") == "coo_sparse"]
        break
    return ins, sorted(outs)


def sparse_dense(v, dims):
    """The SparseTensor components BigQuery's "converts them into Sparse
    format" makes of v, a dense [batch, n] tensor of a row's ARRAY: its n
    values at indices [b, 0] .. [b, n-1], of dense shape [batch, dims[1]]
    (or [batch, n] where the model leaves the dimension free)."""
    import tensorflow as tf

    idx = tf.where(tf.ones_like(v, tf.bool))
    vals = tf.reshape(v, [-1])
    shape = tf.shape(v, out_type=tf.int64)
    if dims is not None and len(dims) == 2 and dims[1] >= 0:
        shape = tf.stack([shape[0], tf.constant(dims[1], tf.int64)])
    return idx, vals, shape


def sparse_rewrite(fn, sparse_in):
    """Writes the SavedModel whose serving_default takes each SparseTensor
    input as a dense [batch, n] tensor of its name and feeds fn the
    SparseTensor sparse_dense makes of it. Answers its path and the
    metadata {name: [dims after the batch]}."""
    import tensorflow as tf

    ins = fn.structured_input_signature[1]
    comps = {}
    meta = {}
    for name, (dtype, dims) in sorted(sparse_in.items()):
        if dims is None or len(dims) != 2:
            fail("sparse", "serving_default's input %s is a SparseTensor of rank %s; a BigQuery column (a NULL, a value or an ARRAY) fills one of rank 2, "
                 "[batch, n]" % (name, "unknown" if dims is None else len(dims)))
        names = [name + "_indices", name + "_values", name + "_dense_shape"]
        if any(n not in ins for n in names):
            fail("sparse", "serving_default's SparseTensor input %s does not load as its indices, values and dense_shape" % name)
        comps[name] = (tf.dtypes.as_dtype(dtype), dims)
        meta[name] = dims[1:]
    sparse_names = {n for name in comps for n in (name + "_indices", name + "_values", name + "_dense_shape")}
    specs = {k: tf.TensorSpec(s.shape, s.dtype, name=k) for k, s in ins.items() if k not in sparse_names}
    for name, (dtype, dims) in comps.items():
        if name in specs:
            fail("sparse", "serving_default has a SparseTensor input %s and a dense input of the same name" % name)
        specs[name] = tf.TensorSpec([None, None], dtype, name=name)

    def serve(**kw):
        args = {k: v for k, v in kw.items() if k not in comps}
        for name, (_, dims) in comps.items():
            args[name + "_indices"], args[name + "_values"], args[name + "_dense_shape"] = sparse_dense(kw[name], dims)
        return fn(**args)

    cf = tf.function(serve).get_concrete_function(**specs)
    module = tf.Module()
    module.serve = cf
    dst = "/tmp/sparse_model"
    tf.saved_model.save(module, dst, signatures={"serving_default": cf})
    return dst, meta


# The sparse operators lower_sparse rewrites: TensorFlow Lite has no
# builtin for them, and a dense one answers exactly what they do.
SPARSE_LOWERED = ("SparseReduceSum", "SparseTensorDenseMatMul", "SparseSegmentSum", "SparseSegmentMean", "SparseSegmentSqrtN")


def lower_sparse(src):
    """Rewrites the SPARSE_LOWERED operators of the SavedModel at src to
    TensorFlow Lite builtins: SparseReduceSum to Sum of SparseToDense,
    SparseTensorDenseMatMul to MatMul of SparseToDense (a missing value is
    0 in both), and SparseSegmentSum, Mean and SqrtN (embedding lookups)
    to SegmentSum of a Gather, divided by the segment's count or its
    square root, at least 1. Answers the SavedModel to convert: src itself
    when it has none of them."""
    import tensorflow as tf
    from tensorflow.python.framework.convert_to_constants import convert_variables_to_constants_v2

    fn = tf.saved_model.load(src).signatures["serving_default"]
    try:
        frozen = convert_variables_to_constants_v2(fn)
    except Exception:  # noqa: BLE001 - convert it as it is
        return src
    gd = frozen.graph.as_graph_def()
    found = [n for n in gd.node if n.op in SPARSE_LOWERED]
    for f in gd.library.function:
        found += [n for n in f.node_def if n.op in SPARSE_LOWERED]
    if not found:
        return src
    rewrite_nodes(gd.node, False)
    for f in gd.library.function:
        rewrite_nodes(f.node_def, True)
    specs = fn.structured_input_signature[1]
    # frozen's inputs are fn's, in fn's flat (keyword) order.
    order = list(getattr(fn, "_arg_keywords", None) or sorted(specs.keys()))
    if len(order) != len(frozen.inputs) or set(order) != set(specs.keys()):
        return src
    out_keys = sorted(fn.structured_outputs.keys())
    out_names = [t.name for t in frozen.outputs]

    def body():
        imap = {t.name: tf.compat.v1.placeholder(specs[k].dtype, specs[k].shape, name=k) for k, t in zip(order, frozen.inputs)}
        tf.graph_util.import_graph_def(gd, input_map=imap, name="m")

    wrapped = tf.compat.v1.wrap_function(body, [])
    g = wrapped.graph
    pruned = wrapped.prune([g.get_tensor_by_name(k + ":0") for k in order],
                           {k: g.get_tensor_by_name("m/" + n) for k, n in zip(out_keys, out_names)})
    module = tf.Module()
    module.serve = pruned
    dst = src.rstrip("/") + "_lowered" if src.startswith("/tmp/") else "/tmp/lowered_model"
    tf.saved_model.save(module, dst, signatures={"serving_default": pruned})
    return dst


def rewrite_nodes(nodes, in_function):
    """Replaces each SPARSE_LOWERED node of nodes (a GraphDef's, or a
    FunctionDef's when in_function) by builtin operators, the last of them
    keeping the node's name and output so its consumers are unchanged."""
    from tensorflow.core.framework import node_def_pb2, types_pb2

    def ref(name, arg):
        return name + ":" + arg + ":0" if in_function else name

    def node(name, op, inputs, device="", **attrs):
        n = node_def_pb2.NodeDef(name=name, op=op, input=inputs, device=device)
        for k, v in attrs.items():
            if isinstance(v, bool):
                n.attr[k].b = v
            elif isinstance(v, int):
                n.attr[k].type = v
            else:
                n.attr[k].CopyFrom(v)
        return n

    def const(name, value, dtype):
        import tensorflow as tf
        from tensorflow.core.framework import attr_value_pb2

        n = node(name, "Const", [], dtype=dtype)
        n.attr["value"].CopyFrom(attr_value_pb2.AttrValue(tensor=tf.make_tensor_proto(value, dtype=tf.dtypes.as_dtype(dtype))))
        return n

    out = []
    for n in nodes:
        if n.op not in SPARSE_LOWERED:
            out.append(n)
            continue
        T = n.attr["T"].type
        dev = n.device
        if n.op in ("SparseReduceSum", "SparseTensorDenseMatMul"):
            idx, vals, shape = n.input[0], n.input[1], n.input[2]
            ti = n.attr["Tindices"].type if "Tindices" in n.attr else types_pb2.DT_INT64
            zero = const(n.name + "_cb_zero", 0, T)
            dense = node(n.name + "_cb_dense", "SparseToDense", [idx, shape, vals, ref(zero.name, "output")], dev,
                         T=T, Tindices=ti, validate_indices=False)
            out += [zero, dense]
            if n.op == "SparseReduceSum":
                out.append(node(n.name, "Sum", [ref(dense.name, "dense"), n.input[3]], dev, T=T, Tidx=types_pb2.DT_INT32,
                                keep_dims=n.attr["keep_dims"].b))
            else:
                out.append(node(n.name, "MatMul", [ref(dense.name, "dense"), n.input[3]], dev, T=T,
                                transpose_a=n.attr["adjoint_a"].b, transpose_b=n.attr["adjoint_b"].b))
            continue
        data, idx, seg = n.input[0], n.input[1], n.input[2]
        tidx = n.attr["Tidx"].type
        tseg = n.attr["Tsegmentids"].type if "Tsegmentids" in n.attr else types_pb2.DT_INT32
        axis = const(n.name + "_cb_axis", 0, types_pb2.DT_INT32)
        gather = node(n.name + "_cb_gather", "GatherV2", [data, idx, ref(axis.name, "output")], dev, Tparams=T, Tindices=tidx,
                      Taxis=types_pb2.DT_INT32)
        gather.attr["batch_dims"].i = 0
        total = node(n.name + "_cb_sum" if n.op != "SparseSegmentSum" else n.name, "SegmentSum", [ref(gather.name, "output"), seg], dev,
                     T=T, Tindices=tseg)
        out += [axis, gather, total]
        if n.op == "SparseSegmentSum":
            continue
        # count of each segment, at least 1, shaped to broadcast over the
        # gathered rows.
        ones = node(n.name + "_cb_ones", "OnesLike", [seg], dev, T=tseg)
        onesf = node(n.name + "_cb_onesf", "Cast", [ref(ones.name, "y")], dev, SrcT=tseg, DstT=T)
        onesf.attr["Truncate"].b = False
        count = node(n.name + "_cb_count", "SegmentSum", [ref(onesf.name, "y"), seg], dev, T=T, Tindices=tseg)
        one = const(n.name + "_cb_one", 1, T)
        atleast = node(n.name + "_cb_atleast", "Maximum", [ref(count.name, "output"), ref(one.name, "output")], dev, T=T)
        div = ref(atleast.name, "z")
        out += [ones, onesf, count, one, atleast]
        if n.op == "SparseSegmentSqrtN":
            sq = node(n.name + "_cb_sqrt", "Sqrt", [div], dev, T=T)
            out.append(sq)
            div = ref(sq.name, "y")
        # [segments] -> [segments, 1, ..., 1] as the data's rank needs:
        # reshape to [-1] + [1] * (rank(total) - 1).
        rank = node(n.name + "_cb_rank", "Rank", [ref(total.name, "output")], dev, T=T)
        one_i = const(n.name + "_cb_one_i", [1], types_pb2.DT_INT32)
        rest = node(n.name + "_cb_rest", "Sub", [ref(rank.name, "output"), ref(one_i.name, "output")], dev, T=types_pb2.DT_INT32)
        one_s = const(n.name + "_cb_one_s", 1, types_pb2.DT_INT32)
        fillv = node(n.name + "_cb_fill", "Fill", [ref(rest.name, "z"), ref(one_s.name, "output")], dev, T=types_pb2.DT_INT32,
                     index_type=types_pb2.DT_INT32)
        minus = const(n.name + "_cb_minus", [-1], types_pb2.DT_INT32)
        cat_axis = const(n.name + "_cb_cat_axis", 0, types_pb2.DT_INT32)
        shape = node(n.name + "_cb_shape", "ConcatV2", [ref(minus.name, "output"), ref(fillv.name, "output"), ref(cat_axis.name, "output")], dev,
                     T=types_pb2.DT_INT32, Tidx=types_pb2.DT_INT32)
        shape.attr["N"].i = 2
        resh = node(n.name + "_cb_div", "Reshape", [div, ref(shape.name, "output")], dev, T=T, Tshape=types_pb2.DT_INT32)
        quot = node(n.name + "_cb_quot", "RealDiv", [ref(total.name, "output"), ref(resh.name, "output")], dev, T=T)
        ident = node(n.name, "Identity", [ref(quot.name, "z")], dev, T=T)
        out += [rank, one_i, rest, one_s, fillv, minus, cat_axis, shape, resh, quot, ident]
    del nodes[:]
    nodes.extend(out)


def with_metadata(data, name, value):
    """data, a TensorFlow Lite flatbuffer, with a metadata entry name whose
    buffer holds value."""
    import flatbuffers
    from tensorflow.lite.python import schema_py_generated as schema

    m = schema.ModelT.InitFromObj(schema.Model.GetRootAsModel(data, 0))
    buf = schema.BufferT()
    buf.data = list(value)
    m.buffers.append(buf)
    md = schema.MetadataT()
    md.name = name
    md.buffer = len(m.buffers) - 1
    m.metadata = (m.metadata or []) + [md]
    b = flatbuffers.Builder(len(data) + 1024)
    b.Finish(m.Pack(b), file_identifier=b"TFL3")
    return bytes(b.Output())


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
    """Converts a tf.train.Example signature from its parse's outputs on:
    as the create-tensorflow page says, "BigQuery ML automatically
    determines the feature names and converts the input BigQuery columns",
    so the model written takes each feature as an input of its name: a
    FixedLenFeature of its dtype and shape, a VarLenFeature (a
    SparseTensor) as the dense [batch, n] ARRAY sparse_dense turns into
    the parse's SparseTensor; and the parse is gone. Answers the
    SavedModel's path and the sparse features' metadata."""
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
        sparse = strings(p.input[2]) if nsparse else []
    else:
        nsparse = p.attr["Nsparse"].i
        nd = p.attr["Ndense"].i
        sparse = [strings(i)[0] for i in p.input[2:2 + nsparse]]
        keys = [strings(i)[0] for i in p.input[2 + nsparse:2 + nsparse + nd]]
    tsparse = [tf.dtypes.as_dtype(t) for t in p.attr["sparse_types"].list.type]
    tdense = [tf.dtypes.as_dtype(t) for t in p.attr["Tdense"].list.type]
    shapes = [tf.TensorShape(sh) for sh in p.attr["dense_shapes"].list.shape]
    for k in keys + sparse:
        if not k.replace("_", "a").isalnum() or k[0].isdigit():
            fail("convert", "the tf.train.Example feature %r is not a name BigQuery can give a column" % k)
    if len(set(keys + sparse)) != len(keys) + len(sparse):
        fail("convert", "the tf.train.Example input parses a feature twice")
    out_keys = sorted(fn.structured_outputs.keys())
    out_names = [t.name for t in frozen.outputs]

    def body():
        imap = {}
        for i, (k, dt) in enumerate(zip(sparse, tsparse)):
            ph = tf.compat.v1.placeholder(dt, [None, None], name=k)
            idx, vals, shape = sparse_dense(ph, None)
            imap["%s:%d" % (p.name, i)] = idx
            imap["%s:%d" % (p.name, nsparse + i)] = vals
            imap["%s:%d" % (p.name, 2 * nsparse + i)] = shape
        for i, (k, dt, sh) in enumerate(zip(keys, tdense, shapes)):
            imap["%s:%d" % (p.name, 3 * nsparse + i)] = tf.compat.v1.placeholder(dt, [None] + sh.as_list(), name=k)
        tf.graph_util.import_graph_def(gd, input_map=imap, name="m")

    wrapped = tf.compat.v1.wrap_function(body, [])
    g = wrapped.graph
    pruned = wrapped.prune([g.get_tensor_by_name(k + ":0") for k in keys + sparse],
                           {k: g.get_tensor_by_name("m/" + n) for k, n in zip(out_keys, out_names)})
    module = tf.Module()
    module.serve = pruned
    dst = "/tmp/example_model"
    tf.saved_model.save(module, dst, signatures={"serving_default": pruned})
    return dst, {k: [-1] for k in sparse}


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
