"""Writes the Keras SavedModels TestBigQueryMLTensorFlowKeras imports
(#1775), and TensorFlow's own serving_default output for its rows.

The test runs it, on an --internal Docker network (no route out), in
CloudBurrow's TensorFlow converter image (deploy/tfconvert: Google's
tensorflow/tensorflow:2.20.0, pinned by digest, Apache-2.0), the image the
instance converts with:

  docker run --rm --network <internal> --entrypoint python3 \
    -v <this folder>:/w:ro -v <out>:/out cloudburrow/tfconvert:<tag> -I /w/gen_keras.py /out

Each is a Keras 3 model written with model.export(), as Keras documents
for TensorFlow Serving, so its serving_default signature takes the Input
layers' names and answers output_0.

<out>/dense:   keras.Sequential of Dense layers: features FLOAT32[None,4]
               -> Dense(8, relu) -> Dense(3, softmax); output_0
               FLOAT32[None,3].
<out>/text:    a text classifier over string tokens: tokens STRING[None,4]
               -> StringLookup (a vocabulary of 8 words, one OOV index)
               -> Embedding(9, 4) -> LSTM(5) (a tf.while_loop over the
               tokens) -> Dense(2, softmax); output_0 FLOAT32[None,2].
<out>/word:    a scalar string per row: word STRING[None] -> StringLookup
               -> CategoryEncoding(one_hot) -> Dense(3); output_0
               FLOAT32[None,3].
<out>/split:   a sentence STRING[None] split into words with
               tf.strings.split before its StringLookup: TensorFlow Lite
               has no builtin for StringSplit, so it needs Flex, and runs
               on TensorFlow as on BigQuery.
<out>/predictions.json: {"dense": [...], "text": [...], "word": [...],
               "split": [...]},
               each row's inputs and TensorFlow's outputs.
"""

import json
import os
import sys

os.environ.setdefault("KERAS_BACKEND", "tensorflow")

import keras
import numpy as np
import tensorflow as tf

os.umask(0)
out = sys.argv[1]
keras.utils.set_random_seed(1775)
rng = np.random.default_rng(1775)
VOCAB = ["good", "bad", "great", "awful", "movie", "plot", "fine", "boring"]


def export(m, name):
    m.export(os.path.join(out, name), format="tf_saved_model", verbose=False)


dense = keras.Sequential([
    keras.Input(shape=(4,), name="features"),
    keras.layers.Dense(8, activation="relu"),
    keras.layers.Dense(3, activation="softmax"),
])
export(dense, "dense")

tok = keras.Input(shape=(4,), dtype="string", name="tokens")
h = keras.layers.StringLookup(vocabulary=VOCAB)(tok)
h = keras.layers.Embedding(len(VOCAB) + 1, 4)(h)
h = keras.layers.LSTM(5)(h)
text = keras.Model(tok, keras.layers.Dense(2, activation="softmax")(h))
export(text, "text")

w = keras.Input(shape=(), dtype="string", name="word")
h = keras.layers.StringLookup(vocabulary=VOCAB, output_mode="one_hot")(w)
word = keras.Model(w, keras.layers.Dense(3)(h))
export(word, "word")


class Split(keras.layers.Layer):
    def call(self, s):
        return tf.strings.split(s).to_tensor(default_value="", shape=[None, 4])


s = keras.Input(shape=(), dtype="string", name="sentence")
h = keras.layers.StringLookup(vocabulary=VOCAB)(Split()(s))
h = keras.layers.Embedding(len(VOCAB) + 1, 2)(h)
split = keras.Model(s, keras.layers.Dense(1)(keras.layers.GlobalAveragePooling1D()(h)))
export(split, "split")


def serve(name):
    return tf.saved_model.load(os.path.join(out, name)).signatures["serving_default"]


def flat(o):
    return {k: [float(z) for z in np.asarray(v.numpy()).ravel()] for k, v in o.items()}


pred = {"dense": [], "text": [], "word": [], "split": []}
fn = serve("dense")
for _ in range(5):
    x = rng.normal(size=(1, 4)).astype(np.float32)
    pred["dense"].append({"inputs": {"features": x[0].tolist()}, "outputs": flat(fn(features=tf.constant(x)))})
fn = serve("text")
words = VOCAB + ["unseen", ""]
for _ in range(5):
    t = [words[int(i)] for i in rng.integers(0, len(words), size=4)]
    pred["text"].append({"inputs": {"tokens": t}, "outputs": flat(fn(tokens=tf.constant([t])))})
fn = serve("word")
for t in ["good", "boring", "plot", "unseen", ""]:
    pred["word"].append({"inputs": {"word": t}, "outputs": flat(fn(word=tf.constant([t])))})
fn = serve("split")
for t in ["good movie", "bad boring plot", "", "unseen words here", "great fine movie plot extra"]:
    pred["split"].append({"inputs": {"sentence": t}, "outputs": flat(fn(sentence=tf.constant([t])))})
with open(os.path.join(out, "predictions.json"), "w") as f:
    json.dump(pred, f, indent=1)
