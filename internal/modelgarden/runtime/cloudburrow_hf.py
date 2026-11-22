# CloudBurrow Model Garden serving runtime (#2148), Apache-2.0.
#
# Google's Hugging Face PyTorch Inference container serves
# huggingface-inference-toolkit's Starlette app; this module is that app,
# unchanged, with one difference: audio sent as bytes (a :rawPredict with an
# audio content type) is decoded by transformers' ffmpeg_read, which runs the
# ffmpeg binary. ffmpeg is not shipped here (its builds are LGPL or GPL), so
# WAV is decoded with Python's own wave module instead, and resampled to the
# rate the model's feature extractor asks for. Other audio formats are
# refused with that reason.
from cloudburrow_hf_audio import wav_read  # noqa: E402

import transformers.pipelines.audio_classification as _ac
import transformers.pipelines.audio_utils as _au  # noqa: E402
import transformers.pipelines.automatic_speech_recognition as _asr  # noqa: E402

for _m in (_au, _ac, _asr):
    if hasattr(_m, "ffmpeg_read"):
        _m.ffmpeg_read = wav_read

from huggingface_inference_toolkit.webservice_starlette import app  # noqa: E402,F401
