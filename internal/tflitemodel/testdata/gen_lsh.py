# Builds lsh.tflite, of LSH_PROJECTION (lsh.go, #1775), and records
# LiteRT's outputs for it on seeded inputs in litert_lsh.json. TensorFlow's
# converter writes no LSH_PROJECTION (no TensorFlow op lowers to it), so the
# model is written directly with schema.fbs's Python object API, which
# TensorFlow ships. Four projections of one subgraph:
#   sparse_w: SPARSE, INT32 [3, 2] rows, a FLOAT32 [3] weight, hash [4, 3];
#   sparse:   SPARSE, FLOAT32 [5, 3] rows, no weight, hash [3, 6];
#   dense:    DENSE, INT64 [4] rows, no weight, hash [2, 5];
#   dense_u8: DENSE, UINT8 [2, 7] rows (keys over 16 bytes), a weight, hash [3, 4].
# It runs, on an --internal Docker network (no route out), in CloudBurrow's
# TensorFlow converter image (deploy/tfconvert: Google's
# tensorflow/tensorflow:2.20.0, pinned by digest, Apache-2.0):
#   docker run --rm --network <internal> -v $PWD/internal/tflitemodel/testdata:/w \
#     -v <out>:/out --entrypoint python3 cloudburrow/tfconvert:<tag> -I /w/gen_lsh.py /out
import json
import os
import sys

import flatbuffers
import numpy as np
import tensorflow as tf
from tensorflow.lite.python import schema_py_generated as S

out = sys.argv[1]
rng = np.random.default_rng(1775)
T = S.TensorType
tensors, buffers, ops, inputs, outputs = [], [S.BufferT()], [], [], []


def tensor(name, shape, typ, data=None):
    t = S.TensorT()
    t.name, t.shape, t.type, t.buffer = name, list(shape), typ, 0
    if data is not None:
        b = S.BufferT()
        b.data = np.frombuffer(np.ascontiguousarray(data).tobytes(), np.uint8)
        buffers.append(b)
        t.buffer = len(buffers) - 1
    tensors.append(t)
    return len(tensors) - 1


def lsh(name, kind, x_shape, x_type, num_hash, num_bits, weighted):
    seeds = (rng.standard_normal((num_hash, num_bits)) * 1000).astype(np.float32)
    h = tensor(name + "_hash", seeds.shape, T.FLOAT32, seeds)
    x = tensor(name + "_x", x_shape, x_type)
    inputs.append(x)
    w = -1
    if weighted:
        w = tensor(name + "_w", [x_shape[0]], T.FLOAT32)
        inputs.append(w)
    o = tensor(name, [num_hash] if kind == S.LSHProjectionType.SPARSE else [num_hash * num_bits], T.INT32)
    outputs.append(o)
    op = S.OperatorT()
    op.opcodeIndex, op.inputs, op.outputs = 0, [h, x] + ([w] if weighted else []), [o]
    op.builtinOptionsType = S.BuiltinOptions.LSHProjectionOptions
    op.builtinOptions = S.LSHProjectionOptionsT()
    op.builtinOptions.type = kind
    ops.append(op)


lsh("sparse_w", S.LSHProjectionType.SPARSE, [3, 2], T.INT32, 4, 3, True)
lsh("sparse", S.LSHProjectionType.SPARSE, [5, 3], T.FLOAT32, 3, 6, False)
lsh("dense", S.LSHProjectionType.DENSE, [4], T.INT64, 2, 5, False)
lsh("dense_u8", S.LSHProjectionType.DENSE, [2, 7], T.UINT8, 3, 4, True)

g = S.SubGraphT()
g.tensors, g.inputs, g.outputs, g.operators, g.name = tensors, inputs, outputs, ops, "main"
m = S.ModelT()
m.version, m.description, m.buffers, m.subgraphs = 3, "gen_lsh.py", buffers, [g]
oc = S.OperatorCodeT()
oc.builtinCode = oc.deprecatedBuiltinCode = S.BuiltinOperator.LSH_PROJECTION
oc.version = 1
m.operatorCodes = [oc]
b = flatbuffers.Builder(1024)
b.Finish(m.Pack(b), file_identifier=b"TFL3")
blob = bytes(b.Output())
with open(os.path.join(out, "lsh.tflite"), "wb") as f:
    f.write(blob)

R = tf.lite.experimental.OpResolverType
it = tf.lite.Interpreter(model_content=blob, experimental_op_resolver_type=R.BUILTIN_REF)
it.allocate_tensors()
cases = []
for _ in range(4):
    feed = {}
    for d in it.get_input_details():
        shape, dt = d["shape"], d["dtype"]
        if dt == np.float32:
            v = rng.standard_normal(shape).astype(np.float32)
        elif dt == np.uint8:
            v = rng.integers(0, 256, size=shape).astype(np.uint8)
        else:
            v = rng.integers(-1000, 1000, size=shape).astype(dt)
        it.set_tensor(d["index"], v)
        feed[d["name"]] = v.tolist()
    it.invoke()
    cases.append({"inputs": feed, "outputs": {d["name"]: it.get_tensor(d["index"]).tolist() for d in it.get_output_details()}})
with open(os.path.join(out, "litert_lsh.json"), "w") as f:
    json.dump(cases, f, indent=1)
