# Builds the models of the builtin operators ops4.go, rnn.go and
# resource.go add (#1775) and records LiteRT's outputs on fixed inputs in
# litert_ops4.json, with LiteRT's reference kernels (BUILTIN_REF):
#   - rnn4.tflite: the fused recurrent kernels LSTM, UNIDIRECTIONAL_SEQUENCE_LSTM,
#     BIDIRECTIONAL_SEQUENCE_LSTM, RNN, UNIDIRECTIONAL_SEQUENCE_RNN and
#     BIDIRECTIONAL_SEQUENCE_RNN, and random4.tflite RANDOM_UNIFORM and
#     RANDOM_STANDARD_NORMAL, written directly with schema.fbs's Python
#     object API (TensorFlow ships it), since TensorFlow 2.20's converter
#     only fuses them from Keras 2 layers;
#   - ops4.tflite (NON_MAX_SUPPRESSION_V4/V5, CONV_3D, CONV_3D_TRANSPOSE,
#     RFFT2D, REAL, IMAG, COMPLEX_ABS) and table4.tflite (the HASHTABLE
#     operators, CALL_ONCE), and var4.tflite (VAR_HANDLE, READ_VARIABLE,
#     ASSIGN_VARIABLE), TensorFlow 2.20's TFLiteConverter's.
# It runs, with no network, in CloudBurrow's TensorFlow converter image
# (deploy/tfconvert: python:3.12-slim@sha256:5e2bdb1b0d54... with PyPI's
# tensorflow==2.20.0, Apache-2.0):
#   docker run --rm --network none -v $PWD/internal/tflitemodel/testdata:/w \
#     -v <out>:/out --entrypoint python3 cloudburrow/tfconvert:<tag> -I /w/gen_ops4.py /out
import json, os, sys, tempfile
import flatbuffers
import numpy as np
import tensorflow as tf
from tensorflow.lite.python import schema_py_generated as S

out = sys.argv[1]
rng = np.random.default_rng(41)
BO = S.BuiltinOptions
OP = S.BuiltinOperator
F32, I32, I64 = S.TensorType.FLOAT32, S.TensorType.INT32, S.TensorType.INT64


class Builder:
    """A one-subgraph model, written with schema.fbs's object API."""

    def __init__(self, parent=None):
        self.tensors, self.buffers, self.ops, self.codes = [], [S.BufferT()], [], []
        self.inputs, self.outputs = [], []
        if parent:  # another subgraph of parent's model
            self.buffers, self.codes = parent.buffers, parent.codes

    def tensor(self, name, shape, typ=F32, data=None, variable=False):
        t = S.TensorT()
        t.name, t.shape, t.type, t.isVariable = name, list(shape), typ, variable
        t.buffer = 0
        if data is not None:
            b = S.BufferT()
            b.data = np.frombuffer(np.ascontiguousarray(data).tobytes(), np.uint8)
            self.buffers.append(b)
            t.buffer = len(self.buffers) - 1
        self.tensors.append(t)
        return len(self.tensors) - 1

    def const(self, name, *shape, scale=0.5):
        return self.tensor(name, shape, data=(rng.standard_normal(shape) * scale).astype(np.float32))

    def input(self, name, shape, typ=F32):
        i = self.tensor(name, shape, typ)
        self.inputs.append(i)
        return i

    def op(self, code, ins, outs, otype=0, opts=None, v2=False):
        if code not in self.codes:
            self.codes.append(code)
        o = S.OperatorT()
        o.opcodeIndex, o.inputs, o.outputs = self.codes.index(code), list(ins), list(outs)
        if v2:  # schema.fbs's builtin_options_2 (the StableHLO options)
            o.builtinOptions2Type, o.builtinOptions2 = otype, opts
        else:
            o.builtinOptionsType, o.builtinOptions = otype, opts
        self.ops.append(o)

    def output(self, i):
        self.outputs.append(i)

    def graph(self, name):
        g = S.SubGraphT()
        g.tensors, g.inputs, g.outputs, g.operators, g.name = self.tensors, self.inputs, self.outputs, self.ops, name
        return g

    def bytes(self, subs=()):
        m = S.ModelT()
        m.version, m.description, m.buffers = 3, "gen_ops4.py", self.buffers
        for c in self.codes:
            oc = S.OperatorCodeT()
            oc.builtinCode, oc.deprecatedBuiltinCode, oc.version = c, min(c, 127), 1
            m.operatorCodes = (m.operatorCodes or []) + [oc]
        m.subgraphs = [self.graph("main")] + [x.graph(f"sub{i + 1}") for i, x in enumerate(subs)]
        b = flatbuffers.Builder(1024)
        b.Finish(m.Pack(b), file_identifier=b"TFL3")
        return bytes(b.Output())


def lstm_weights(b, p, n_in, n_cell, n_out, cifg=False, peephole=False, proj=False, proj_bias=False, ln=False, aux=0, diag=False):
    """The 17 weight inputs (1..17) of a direction, then its 4 layer-norm
    coefficients and aux weights, as LiteRT's LSTM kernels number them."""
    w = []
    gates = ["i", "f", "c", "o"]
    for g in gates:
        w.append(-1 if cifg and g == "i" else b.const(f"{p}_w_{g}", n_cell, n_in))
    for g in gates:
        w.append(-1 if cifg and g == "i" else (b.const(f"{p}_r_{g}", n_cell) if diag else b.const(f"{p}_r_{g}", n_cell, n_out)))
    for g in ["i", "f", "o"]:
        w.append(b.const(f"{p}_peep_{g}", n_cell) if peephole and not (cifg and g == "i") else -1)
    for g in gates:
        w.append(-1 if cifg and g == "i" else b.const(f"{p}_b_{g}", n_cell, scale=0.2))
    w.append(b.const(f"{p}_proj", n_out, n_cell) if proj else -1)
    w.append(b.const(f"{p}_proj_b", n_out, scale=0.2) if proj and proj_bias else -1)
    lnw = [(-1 if cifg and g == "i" else b.const(f"{p}_ln_{g}", n_cell, scale=0.3) + 0) if ln else -1 for g in gates]
    if ln:  # layer-norm coefficients around 1
        for k in lnw:
            if k >= 0:
                d = np.frombuffer(b.buffers[b.tensors[k].buffer].data.tobytes(), np.float32) + 1
                b.buffers[b.tensors[k].buffer].data = np.frombuffer(d.astype(np.float32).tobytes(), np.uint8)
    auxw = [(-1 if cifg and g == "i" else b.const(f"{p}_aux_{g}", n_cell, aux)) for g in gates] if aux else [-1] * 4
    return w, lnw, auxw


def rnn_model():
    b = Builder()
    B, T, N = 2, 5, 3
    xb = b.input("x_bm", [B, T, N])
    xt = b.input("x_tm", [T, B, N])
    xs = b.input("x_step", [B, N])
    # The aux input is as wide as the input: LiteRT 2.20 steps through it by
    # the input's width.
    at = b.input("aux_tm", [T, B, N])

    def state(name, *shape):
        return b.tensor(name, shape, variable=True)

    def uni_lstm(name, x, time_major, n_cell, n_out, act=4, cell_clip=0.0, proj_clip=0.0, **kw):
        w, lnw, _ = lstm_weights(b, name, N, n_cell, n_out, **kw)
        o = S.UnidirectionalSequenceLSTMOptionsT()
        o.fusedActivationFunction, o.cellClip, o.projClip, o.timeMajor = act, cell_clip, proj_clip, time_major
        o.diagonalRecurrentTensors = kw.get("diag", False)
        y = b.tensor(name, [T, B, n_out] if time_major else [B, T, n_out])
        b.op(OP.UNIDIRECTIONAL_SEQUENCE_LSTM, [x] + w + [state(name + "_h", B, n_out), state(name + "_c", B, n_cell)] + lnw,
             [y], BO.UnidirectionalSequenceLSTMOptions, o)
        b.output(y)

    uni_lstm("ulstm_bm", xb, False, 4, 4)
    uni_lstm("ulstm_tm_peep_proj", xt, True, 5, 3, cell_clip=0.6, proj_clip=0.4, peephole=True, proj=True, proj_bias=True)
    uni_lstm("ulstm_cifg_ln", xb, False, 4, 2, act=1, peephole=True, cifg=True, proj=True, ln=True)
    uni_lstm("ulstm_ln", xt, True, 3, 3, ln=True, peephole=True)
    uni_lstm("ulstm_diag", xb, False, 4, 4, diag=True)

    # LSTM, the one-step kernel (kernel_type FULL).
    w, lnw, _ = lstm_weights(b, "lstm_step", N, 4, 4, peephole=True)
    o = S.LSTMOptionsT()
    o.fusedActivationFunction, o.cellClip = 4, 0.8
    y = b.tensor("lstm_step", [B, 4])
    b.op(OP.LSTM, [xs] + w + [state("lstm_step_h", B, 4), state("lstm_step_c", B, 4)] + lnw, [y], BO.LSTMOptions, o)
    b.output(y)

    def bi_lstm(name, x, time_major, merge, aux=None, aux_weights=False, nf=3, nb=3):
        fw, _, fa = lstm_weights(b, name + "_fw", N, nf, nf, aux=N if aux_weights else 0)
        bw, _, ba = lstm_weights(b, name + "_bw", N, nb, nb, peephole=True, aux=N if aux_weights else 0)
        o = S.BidirectionalSequenceLSTMOptionsT()
        o.fusedActivationFunction, o.mergeOutputs, o.timeMajor = 4, merge, time_major
        lead = [T, B] if time_major else [B, T]
        ins = [x] + fw + bw + [state(name + "_fh", B, nf), state(name + "_fc", B, nf), state(name + "_bh", B, nb), state(name + "_bc", B, nb)]
        ins += [aux if aux is not None else -1] + fa + ba
        if merge:
            outs = [b.tensor(name, lead + [nf + nb])]
        else:
            outs = [b.tensor(name + "_fwd", lead + [nf]), b.tensor(name + "_bwd", lead + [nb])]
        b.op(OP.BIDIRECTIONAL_SEQUENCE_LSTM, ins, outs, BO.BidirectionalSequenceLSTMOptions, o)
        for t in outs:
            b.output(t)

    bi_lstm("bilstm_tm", xt, True, False)
    # Batch-major with merge_outputs is left out: LiteRT 2.20 leaves part of
    # that output unwritten (NaN), so there is nothing to match.
    bi_lstm("bilstm_bm", xb, False, False)
    bi_lstm("bilstm_aux", xt, True, True, aux=at, aux_weights=True)
    bi_lstm("bilstm_bwaux", xt, True, False, aux=b.tensor("aux_as_bw", [T, B, N], data=(rng.standard_normal((T, B, N))).astype(np.float32)))

    def rnn_w(p, n_in, units, aux=0):
        return [b.const(p + "_w", units, n_in), b.const(p + "_r", units, units), b.const(p + "_b", units, scale=0.2)], \
            (b.const(p + "_aux", units, aux) if aux else -1)

    def uni_rnn(name, x, time_major, units, act):
        (w, r, bias), _ = rnn_w(name, N, units)
        o = S.SequenceRNNOptionsT()
        o.timeMajor, o.fusedActivationFunction = time_major, act
        y = b.tensor(name, [T, B, units] if time_major else [B, T, units])
        b.op(OP.UNIDIRECTIONAL_SEQUENCE_RNN, [x, w, r, bias, state(name + "_h", B, units)], [y], BO.SequenceRNNOptions, o)
        b.output(y)

    uni_rnn("urnn_bm_tanh", xb, False, 4, 4)
    uni_rnn("urnn_tm_relu", xt, True, 3, 1)

    (w, r, bias), _ = rnn_w("rnn_step", N, 4)
    o = S.RNNOptionsT()
    o.fusedActivationFunction = 3
    y = b.tensor("rnn_step", [B, 4])
    b.op(OP.RNN, [xs, w, r, bias, state("rnn_step_h", B, 4)], [y], BO.RNNOptions, o)
    b.output(y)

    def bi_rnn(name, x, time_major, merge, aux=None, aux_weights=False):
        fw, fa = rnn_w(name + "_fw", N, 3, N if aux_weights else 0)
        bw, ba = rnn_w(name + "_bw", N, 3, N if aux_weights else 0)
        o = S.BidirectionalSequenceRNNOptionsT()
        o.timeMajor, o.fusedActivationFunction, o.mergeOutputs = time_major, 4, merge
        lead = [T, B] if time_major else [B, T]
        ins = [x] + fw + [state(name + "_fh", B, 3)] + bw + [state(name + "_bh", B, 3), aux if aux is not None else -1, fa, ba]
        outs = [b.tensor(name, lead + [6])] if merge else [b.tensor(name + "_fwd", lead + [3]), b.tensor(name + "_bwd", lead + [3])]
        b.op(OP.BIDIRECTIONAL_SEQUENCE_RNN, ins, outs, BO.BidirectionalSequenceRNNOptions, o)
        for t in outs:
            b.output(t)

    bi_rnn("birnn_bm", xb, False, False)
    bi_rnn("birnn_tm_merged_aux", xt, True, True, aux=at, aux_weights=True)
    feed = {"x_bm": rng.standard_normal((B, T, N)).astype(np.float32), "x_tm": rng.standard_normal((T, B, N)).astype(np.float32),
            "x_step": rng.standard_normal((B, N)).astype(np.float32), "aux_tm": rng.standard_normal((T, B, N)).astype(np.float32)}
    return b.bytes(), feed


def random_model():
    b = Builder()
    x = b.input("x", [3, 5])
    shape = b.tensor("shape", [2], I32, data=np.array([3, 5], np.int32))
    for code, name, s1, s2 in [(OP.RANDOM_UNIFORM, "uniform", 7, 11), (OP.RANDOM_STANDARD_NORMAL, "normal", 12345, 0),
                               (OP.RANDOM_UNIFORM, "uniform_big_seed", 1 << 40, -3)]:
        o = S.RandomOptionsT()
        o.seed, o.seed2 = s1, s2
        r = b.tensor(name + "_raw", [3, 5])
        b.op(code, [shape], [r], BO.RandomOptions, o)
        y = b.tensor(name, [3, 5])
        b.op(OP.ADD, [x, r], [y], BO.AddOptions, S.AddOptionsT())
        b.output(y)
    return b.bytes(), {"x": np.zeros((3, 5), np.float32)}


def run(blob, feed):
    # The reference kernels, or LiteRT's builtin ones for a model using an
    # operator the reference resolver lacks (RFFT2D).
    R = tf.lite.experimental.OpResolverType
    try:
        it = tf.lite.Interpreter(model_content=blob, experimental_op_resolver_type=R.BUILTIN_REF)
    except ValueError:
        it = tf.lite.Interpreter(model_content=blob, experimental_op_resolver_type=R.BUILTIN_WITHOUT_DEFAULT_DELEGATES)
    sigs = it.get_signature_list()
    if sigs:
        res = it.get_signature_runner()(**feed)
    else:
        it.allocate_tensors()
        for d in it.get_input_details():
            it.set_tensor(d["index"], feed[d["name"]])
        it.invoke()
        res = {d["name"]: it.get_tensor(d["index"]) for d in it.get_output_details()}
    return it, res


def tolist(v):
    v = np.asarray(v)
    if v.dtype.kind in "SUO":
        return np.vectorize(lambda x: x.decode() if isinstance(x, bytes) else x, otypes=[object])(v).tolist()
    return np.asarray(v, dtype=np.float64).tolist()


def case(name, blob, feed):
    open(os.path.join(out, name + ".tflite"), "wb").write(blob)
    it, res = run(blob, feed)
    ops = sorted({x["op_name"] for x in it._get_ops_details()})
    print(name, ops)
    c = {"ops": ops, "inputs": {k: tolist(v) for k, v in feed.items()}, "outputs": {k: tolist(v) for k, v in res.items()}}
    for k, v in res.items():
        if np.asarray(v).dtype.kind in "SUO":
            c.setdefault("shapes", {})[k] = list(np.asarray(v).shape)
    return c


def convert(mod):
    d = tempfile.mkdtemp()
    tf.saved_model.save(mod, d, signatures={"serving_default": mod.__call__})
    c = tf.lite.TFLiteConverter.from_saved_model(d)
    return c.convert()


class Ops4(tf.Module):
    def __init__(self):
        self.k3 = tf.constant(rng.standard_normal((2, 2, 3, 2, 3)).astype(np.float32) * 0.5)
        self.k3t = tf.constant(rng.standard_normal((2, 3, 2, 4, 2)).astype(np.float32) * 0.5)
        self.b3 = tf.constant(rng.standard_normal((3,)).astype(np.float32))

    @tf.function(input_signature=[tf.TensorSpec([8, 4], tf.float32, name="boxes"), tf.TensorSpec([8], tf.float32, name="scores"),
                                  tf.TensorSpec([1, 3, 4, 5, 2], tf.float32, name="volume"), tf.TensorSpec([2, 4, 6], tf.float32, name="signal")])
    def __call__(self, boxes, scores, vol, sig):
        r = {}
        idx, n = tf.image.non_max_suppression_padded(boxes, scores, 5, iou_threshold=0.4, score_threshold=0.1, pad_to_max_output_size=True)
        r["nms4_indices"], r["nms4_count"] = tf.cast(idx, tf.float32), tf.cast(tf.reshape(n, [1]), tf.float32)
        si, ss = tf.image.non_max_suppression_with_scores(boxes, scores, 6, iou_threshold=0.5, score_threshold=0.05, soft_nms_sigma=0.5)
        r["nms5_indices"], r["nms5_scores"] = tf.cast(tf.reshape(si, [1, -1]), tf.float32), tf.reshape(ss, [1, -1])
        r["conv3d_same"] = tf.nn.relu(tf.nn.conv3d(vol, self.k3, [1, 1, 2, 1, 1], "SAME") + self.b3)
        r["conv3d_valid_dil"] = tf.nn.conv3d(vol, self.k3, [1, 1, 1, 1, 1], "VALID", dilations=[1, 2, 1, 2, 1])
        r["conv3d_t"] = tf.nn.conv3d_transpose(vol, self.k3t, [1, 6, 8, 9, 4], [1, 2, 2, 2, 1], "SAME")
        r["conv3d_t_valid"] = tf.nn.conv3d_transpose(vol, self.k3t, [1, 4, 6, 6, 4], [1, 1, 1, 1, 1], "VALID")
        f = tf.signal.rfft2d(sig, fft_length=[4, 8])
        r["fft_real"], r["fft_imag"], r["fft_abs"] = tf.math.real(f), tf.math.imag(f), tf.math.abs(f)
        return r


class Table4(tf.Module):
    def __init__(self):
        self.words = tf.lookup.StaticHashTable(tf.lookup.KeyValueTensorInitializer(
            tf.constant(["the", "cat", "sat", "on", "mat"]), tf.constant([3, 1, 4, 1, 5], tf.int64)), default_value=-1)
        self.names = tf.lookup.StaticHashTable(tf.lookup.KeyValueTensorInitializer(
            tf.constant([10, 20, 30], tf.int64), tf.constant(["ten", "twenty", "thirty"])), default_value="?")

    @tf.function(input_signature=[tf.TensorSpec([2, 3], tf.string, name="tokens"), tf.TensorSpec([4], tf.int64, name="codes")])
    def __call__(self, tokens, codes):
        # The reshapes give the outputs a static shape: the converter leaves
        # HASHTABLE_FIND's unranked.
        return {"ids": tf.reshape(self.words.lookup(tokens), [2, 3]), "names": tf.reshape(self.names.lookup(codes), [4]),
                "size": tf.reshape(self.words.size(), [1])}


class Var4(tf.Module):
    def __init__(self):
        self.v = tf.Variable(tf.constant(rng.standard_normal((3,)).astype(np.float32)))

    @tf.function(input_signature=[tf.TensorSpec([2, 3], tf.float32, name="x")])
    def __call__(self, x):
        self.v.assign(self.v * 2.0 + 1.0)
        return {"y": x * self.v}


def convert_vars(mod):
    d = tempfile.mkdtemp()
    tf.saved_model.save(mod, d, signatures={"serving_default": mod.__call__})
    c = tf.lite.TFLiteConverter.from_saved_model(d)
    c.experimental_enable_resource_variables = True
    return c.convert()


cases = {}
blob, feed = rnn_model()
cases["rnn4"] = case("rnn4", blob, feed)
blob, feed = random_model()
cases["random4"] = case("random4", blob, feed)
boxes = np.array([[0, 0, 1, 1], [0, 0.1, 1, 1.1], [0, -0.1, 1, 0.9], [0, 10, 1, 11], [0, 10.1, 1, 11.1], [0, 100, 1, 101],
                  [1, 1, 0, 0], [0.5, 0.5, 0.6, 0.6]], np.float32)
cases["ops4"] = case("ops4", convert(Ops4()), {
    "boxes": boxes, "scores": np.array([0.9, 0.75, 0.6, 0.95, 0.5, 0.3, 0.08, 0.2], np.float32),
    "volume": rng.standard_normal((1, 3, 4, 5, 2)).astype(np.float32), "signal": rng.standard_normal((2, 4, 6)).astype(np.float32)})
cases["table4"] = case("table4", convert(Table4()), {"tokens": np.array([["the", "dog", "sat"], ["on", "the", "mat"]], object),
                                                    "codes": np.array([20, 5, 30, 10], np.int64)})
cases["var4"] = case("var4", convert_vars(Var4()), {"x": rng.standard_normal((2, 3)).astype(np.float32)})


def ops5_model():
    """WHERE, UNIQUE, DYNAMIC_UPDATE_SLICE, MATRIX_SET_DIAG, REVERSE_SEQUENCE,
    SPARSE_TO_DENSE, BROADCAST_ARGS, L2_POOL_2D, BITWISE_XOR, RIGHT_SHIFT,
    FAKE_QUANT, SVDF, HASHTABLE_LOOKUP, DILATE and the StableHLO operators
    LiteRT runs (ops5.go)."""
    b = Builder()
    x = b.input("x", [2, 3, 4])
    x2 = b.input("x2", [2, 3, 4])
    u = b.input("u", [8], I32)
    img = b.input("img", [1, 4, 5, 2])
    seq = b.input("seq", [2, 3])
    look = b.input("look", [4], I32)

    def ci(name, v, typ=I32, dt=np.int32):
        v = np.asarray(v, dt)
        return b.tensor(name, v.shape, typ, data=v)

    def cf(name, v):
        v = np.asarray(v, np.float32)
        return b.tensor(name, v.shape, F32, data=v)

    def out(name, shape, typ=F32):
        t = b.tensor(name, shape, typ)
        b.output(t)
        return t

    gt = b.tensor("gt", [2, 3, 4], S.TensorType.BOOL)
    b.op(OP.GREATER, [x, cf("zero", 0.0)], [gt])
    b.op(OP.WHERE, [gt], [out("where", [1, 3], I64)])
    o = S.UniqueOptionsT()
    o.idxOutType = I32
    b.op(OP.UNIQUE, [u], [out("unique_y", [1], I32), out("unique_idx", [8], I32)], BO.UniqueOptions, o)
    b.op(OP.DYNAMIC_UPDATE_SLICE, [x, cf("upd", rng.standard_normal((1, 2, 2))), ci("start", [0, 2, 3])], [out("dus", [2, 3, 4])])
    b.op(OP.MATRIX_SET_DIAG, [x, cf("diag", rng.standard_normal((2, 3)))], [out("set_diag", [2, 3, 4])], BO.MatrixSetDiagOptions, S.MatrixSetDiagOptionsT())
    o = S.ReverseSequenceOptionsT()
    o.seqDim, o.batchDim = 2, 1
    b.op(OP.REVERSE_SEQUENCE, [x, ci("lens", [4, 1, 3])], [out("rev_seq", [2, 3, 4])], BO.ReverseSequenceOptions, o)
    o = S.SparseToDenseOptionsT()
    b.op(OP.SPARSE_TO_DENSE, [ci("sp_idx", [[0, 1], [2, 3], [1, 0]]), ci("sp_shape", [3, 4]), cf("sp_vals", [1.5, -2.0, 3.25]), cf("sp_def", 0.5)],
         [out("sparse", [3, 4])], BO.SparseToDenseOptions, o)
    b.op(OP.BROADCAST_ARGS, [ci("s0", [2, 1, 4]), ci("s1", [3, 1])], [out("bargs", [3], I32)])
    o = S.Pool2DOptionsT()
    o.padding, o.strideW, o.strideH, o.filterWidth, o.filterHeight = S.Padding.SAME, 2, 2, 2, 3
    b.op(OP.L2_POOL_2D, [img], [out("l2pool", [1, 2, 3, 2])], BO.Pool2DOptions, o)
    b.op(OP.BITWISE_XOR, [u, ci("xor_b", [5])], [out("xor", [8], I32)])
    b.op(OP.RIGHT_SHIFT, [u, ci("shifts", [0, 1, 2, 3, 31, 40, -1, 4])], [out("rshift", [8], I32)])
    for name, lo, hi, nb, narrow in [("fq8", -1.0, 1.5, 8, False), ("fq4", -0.3, 2.0, 4, False)]:
        o = S.FakeQuantOptionsT()
        o.min, o.max, o.numBits, o.narrowRange = lo, hi, nb, narrow
        b.op(OP.FAKE_QUANT, [x], [out(name, [2, 3, 4])], BO.FakeQuantOptions, o)
    o = S.SVDFOptionsT()
    o.rank, o.fusedActivationFunction = 2, 1
    b.op(OP.SVDF, [seq, b.const("svdf_wf", 4, 3), b.const("svdf_wt", 4, 5), b.const("svdf_b", 2, scale=0.2),
                   b.tensor("svdf_state", [2, 20], variable=True)], [out("svdf", [2, 2])], BO.SVDFOptions, o)
    b.op(OP.HASHTABLE_LOOKUP, [look, ci("ht_keys", [-3, 4, 9]), cf("ht_vals", rng.standard_normal((3, 2)))],
         [out("ht_out", [4, 2]), out("ht_hits", [4], S.TensorType.UINT8)])
    b.op(OP.DILATE, [x, ci("dil", [1, 2, 3]), cf("dil_pad", -9.0)], [out("dilate", [2, 5, 10])])
    for code, name in [(OP.STABLEHLO_ADD, "hlo_add"), (OP.STABLEHLO_MULTIPLY, "hlo_mul"), (OP.STABLEHLO_MAXIMUM, "hlo_max"),
                       (OP.STABLEHLO_MINIMUM, "hlo_min")]:
        b.op(code, [x, x2], [out(name, [2, 3, 4])])
    b.op(OP.STABLEHLO_AND, [u, ci("and_b", [6, 3, -1, 0, 255, 7, 12, 1])], [out("hlo_and", [8], I32)])
    b.op(OP.STABLEHLO_SHIFT_LEFT, [u, ci("lshift", [0, 1, 2, 3, 30, 31, 4, 5])], [out("hlo_shl", [8], I32)])
    o = S.StablehloPadOptionsT()
    o.edgePaddingLow, o.edgePaddingHigh, o.interiorPadding = [1, 0, -1], [0, 2, 1], [1, 0, 1]
    b.op(OP.STABLEHLO_PAD, [x, cf("pad_v", 7.0)], [out("hlo_pad", [4, 5, 7])], S.BuiltinOptions2.StablehloPadOptions, o, v2=True)
    # STABLEHLO_COMPOSITE calling a decomposition subgraph: tanh(a) * b.
    d = Builder(b)
    da, db = d.input("a", [2, 3, 4]), d.input("b", [2, 3, 4])
    th, dr = d.tensor("th", [2, 3, 4]), d.tensor("r", [2, 3, 4])
    d.op(OP.TANH, [da], [th])
    d.op(OP.MUL, [th, db], [dr], BO.MulOptions, S.MulOptionsT())
    d.output(dr)
    o = S.StableHLOCompositeOptionsT()
    o.name, o.decompositionSubgraphIndex, o.version = "test.tanh_mul", 1, 1
    b.op(OP.STABLEHLO_COMPOSITE, [x, x2], [out("composite", [2, 3, 4])], S.BuiltinOptions2.StableHLOCompositeOptions, o, v2=True)
    feed = {"x": rng.standard_normal((2, 3, 4)).astype(np.float32), "x2": rng.standard_normal((2, 3, 4)).astype(np.float32),
            "u": np.array([7, -3, 7, 100, -3, 0, 42, 7], np.int32), "img": rng.standard_normal((1, 4, 5, 2)).astype(np.float32),
            "seq": rng.standard_normal((2, 3)).astype(np.float32), "look": np.array([4, 5, -3, 9], np.int32)}
    return b.bytes([d]), feed


blob, feed = ops5_model()
cases["ops5"] = case("ops5", blob, feed)
json.dump(cases, open(os.path.join(out, "litert_ops4.json"), "w"), indent=1)
