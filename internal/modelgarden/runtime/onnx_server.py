# CloudBurrow Model Garden serving runtime (#2148), Apache-2.0.
#
# Serves an ONNX-only Hugging Face repository on ONNX Runtime, through
# optimum's ORTModel classes and a transformers pipeline of the model's
# task, with the request and answer shapes of Google's Hugging Face PyTorch
# Inference container on Vertex AI (huggingface-inference-toolkit's
# VertexAIHandler): POST $AIP_PREDICT_ROUTE takes {"instances": [...],
# "parameters": {...}} and answers {"predictions": [...]}, one prediction
# per instance, each instance being the pipeline's inputs (a dict of them
# is passed as keyword arguments). A body that is an image or a WAV file
# (its Content-Type image/* or audio/*) is one instance. GET
# $AIP_HEALTH_ROUTE answers once the model is loaded.
import io
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np
from transformers import AutoConfig, pipeline

import optimum.onnxruntime as ort

MODEL_CLASSES = {
    "text-classification": "ORTModelForSequenceClassification",
    "zero-shot-classification": "ORTModelForSequenceClassification",
    "token-classification": "ORTModelForTokenClassification",
    "question-answering": "ORTModelForQuestionAnswering",
    "fill-mask": "ORTModelForMaskedLM",
    "feature-extraction": "ORTModelForFeatureExtraction",
    "sentence-similarity": "ORTModelForFeatureExtraction",
    "text-generation": "ORTModelForCausalLM",
    "text2text-generation": "ORTModelForSeq2SeqLM",
    "summarization": "ORTModelForSeq2SeqLM",
    "translation": "ORTModelForSeq2SeqLM",
    "image-classification": "ORTModelForImageClassification",
    "automatic-speech-recognition": "ORTModelForSpeechSeq2Seq",
    "audio-classification": "ORTModelForAudioClassification",
}


def load(model_dir: str, task: str, onnx_file: str):
    cls = getattr(ort, MODEL_CLASSES[task])
    sub, name = os.path.split(onnx_file)
    kwargs = {"subfolder": sub} if sub else {}
    if task not in ("text2text-generation", "summarization", "translation", "automatic-speech-recognition"):
        kwargs["file_name"] = name
    model = cls.from_pretrained(model_dir, **kwargs)
    parts = {}
    for key, loader in (("tokenizer", "AutoTokenizer"), ("image_processor", "AutoImageProcessor"),
                        ("feature_extractor", "AutoFeatureExtractor")):
        try:
            parts[key] = getattr(__import__("transformers", fromlist=[loader]), loader).from_pretrained(model_dir)
        except Exception:  # the repository has none of this kind
            pass
    pipe_task = "feature-extraction" if task == "sentence-similarity" else task
    return pipeline(pipe_task, model=model, config=AutoConfig.from_pretrained(model_dir), **parts)


def plain(v):
    if isinstance(v, np.ndarray):
        return v.tolist()
    if isinstance(v, (np.floating, np.integer)):
        return v.item()
    if hasattr(v, "tolist"):
        return v.tolist()
    if isinstance(v, dict):
        return {k: plain(x) for k, x in v.items()}
    if isinstance(v, (list, tuple)):
        return [plain(x) for x in v]
    return v


def main() -> None:
    task = os.environ["HF_TASK"]
    pipe = load(sys.argv[1], task, os.environ["ONNX_FILE"])
    health, predict = os.environ.get("AIP_HEALTH_ROUTE", "/health"), os.environ.get("AIP_PREDICT_ROUTE", "/predict")

    def run(inputs, parameters):
        return plain(pipe(**inputs, **parameters) if isinstance(inputs, dict) else pipe(inputs, **parameters))

    class Handler(BaseHTTPRequestHandler):
        def reply(self, status, body):
            b = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

        def do_GET(self):
            if self.path == health:
                return self.reply(200, {"status": "ok"})
            self.reply(404, {"error": "not found"})

        def do_POST(self):
            if self.path != predict:
                return self.reply(404, {"error": "not found"})
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
            ctype = (self.headers.get("Content-Type") or "application/json").split(";")[0].strip().lower()
            try:
                if ctype.startswith("image/"):
                    from PIL import Image

                    return self.reply(200, {"predictions": [run(Image.open(io.BytesIO(body)).convert("RGB"), {})]})
                if ctype.startswith("audio/"):
                    from cloudburrow_hf_audio import wav_read

                    rate = pipe.feature_extractor.sampling_rate
                    return self.reply(200, {"predictions": [run({"inputs": wav_read(body, rate)}, {})]})
                req = json.loads(body or b"{}")
                if not isinstance(req, dict) or not isinstance(req.get("instances"), list):
                    return self.reply(400, {"error": "The request body must contain a key 'instances' with a list of instances."})
                params = req.get("parameters") or {}
                if not isinstance(params, dict):
                    return self.reply(400, {"error": "parameters must be an object"})
                self.reply(200, {"predictions": [run(i, params) for i in req["instances"]]})
            except Exception as e:  # the pipeline's own words, as the toolkit answers them
                self.reply(400, {"error": str(e)})

        def log_message(self, *args):
            pass

    port = int(os.environ.get("AIP_HTTP_PORT", "8080"))
    print(f"cloudburrow: serving {os.environ.get('MODEL_ID')} ({task}) on ONNX Runtime, port {port}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
