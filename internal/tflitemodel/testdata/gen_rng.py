# Builds rng.tflite, of STABLEHLO_RNG_BIT_GENERATOR (rng.go, #1775), and
# records LiteRT's outputs for it in litert_rng.json, every integer as a
# decimal string (UINT64s do not fit a JSON number). It is written directly
# with schema.fbs's Python object API, which TensorFlow ships. Four
# generators of one subgraph, each of its own UINT64 state input:
#   philox:      DEFAULT, state u64[2], UINT32 [7] (not a multiple of 4);
#   philox3:     PHILOX, state u64[3], INT64 [3, 2];
#   threefry:    THREEFRY, state u64[2], INT32 [5];
#   threefry64:  THREEFRY, state u64[2], UINT64 [2, 2].
# Each run's states include a counter one short of a 32-bit carry.
# It runs, on an --internal Docker network (no route out), in CloudBurrow's
# TensorFlow converter image (deploy/tfconvert: Google's
# tensorflow/tensorflow:2.20.0, pinned by digest, Apache-2.0):
#   docker run --rm --network <internal> -v $PWD/internal/tflitemodel/testdata:/w \
#     -v <out>:/out --entrypoint python3 cloudburrow/tfconvert:<tag> -I /w/gen_rng.py /out
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
tensors, ops, inputs, outputs = [], [], [], []


def tensor(name, shape, typ):
    t = S.TensorT()
    t.name, t.shape, t.type, t.buffer = name, list(shape), typ, 0
    tensors.append(t)
    return len(tensors) - 1


def gen(name, alg, n_state, otype, oshape):
    s = tensor(name + "_state", [n_state], T.UINT64)
    inputs.append(s)
    ns = tensor(name + "_next", [n_state], T.UINT64)
    o = tensor(name, oshape, otype)
    outputs.extend([ns, o])
    op = S.OperatorT()
    op.opcodeIndex, op.inputs, op.outputs = 0, [s], [ns, o]
    op.builtinOptions2Type = S.BuiltinOptions2.StablehloRngBitGeneratorOptions
    op.builtinOptions2 = S.StablehloRngBitGeneratorOptionsT()
    op.builtinOptions2.algorithm = alg
    ops.append(op)


A = S.RngAlgorithm
gen("philox", A.DEFAULT, 2, T.UINT32, [7])
gen("philox3", A.PHILOX, 3, T.INT64, [3, 2])
gen("threefry", A.THREEFRY, 2, T.INT32, [5])
gen("threefry64", A.THREEFRY, 2, T.UINT64, [2, 2])

g = S.SubGraphT()
g.tensors, g.inputs, g.outputs, g.operators, g.name = tensors, inputs, outputs, ops, "main"
m = S.ModelT()
m.version, m.description, m.buffers, m.subgraphs = 3, "gen_rng.py", [S.BufferT()], [g]
oc = S.OperatorCodeT()
oc.builtinCode, oc.deprecatedBuiltinCode, oc.version = S.BuiltinOperator.STABLEHLO_RNG_BIT_GENERATOR, 127, 1
m.operatorCodes = [oc]
b = flatbuffers.Builder(1024)
b.Finish(m.Pack(b), file_identifier=b"TFL3")
blob = bytes(b.Output())
with open(os.path.join(out, "rng.tflite"), "wb") as f:
    f.write(blob)

it = tf.lite.Interpreter(model_content=blob, experimental_op_resolver_type=tf.lite.experimental.OpResolverType.BUILTIN)
it.allocate_tensors()
cases = []
for run in range(4):
    feed = {}
    for d in it.get_input_details():
        v = rng.integers(0, 2**63, size=d["shape"], dtype=np.uint64) * np.uint64(2) + np.uint64(run % 2)
        if run == 3:  # the counter's low word one short of a carry
            v[1] = (v[1] & np.uint64(0xFFFFFFFF00000000)) | np.uint64(0xFFFFFFFE)
        it.set_tensor(d["index"], v)
        feed[d["name"]] = [str(int(x)) for x in v]
    it.invoke()
    cases.append({"inputs": feed, "outputs": {d["name"]: [str(int(x)) for x in it.get_tensor(d["index"]).ravel()]
                                             for d in it.get_output_details()}})
with open(os.path.join(out, "litert_rng.json"), "w") as f:
    json.dump(cases, f, indent=1)
