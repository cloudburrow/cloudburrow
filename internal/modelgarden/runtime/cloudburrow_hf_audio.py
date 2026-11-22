# CloudBurrow Model Garden serving runtime (#2148), Apache-2.0.
#
# WAV decoding for the audio pipelines, in place of transformers'
# ffmpeg_read: ffmpeg is not shipped (its builds are LGPL or GPL), so PCM WAV
# is read with Python's own wave module and resampled to the rate the
# model's feature extractor asks for. Other audio formats are refused with
# that reason.
import io
import wave

import numpy as np


def wav_read(bpayload: bytes, sampling_rate: int) -> np.ndarray:
    try:
        with wave.open(io.BytesIO(bpayload)) as w:
            channels, width, rate = w.getnchannels(), w.getsampwidth(), w.getframerate()
            frames = w.readframes(w.getnframes())
    except (wave.Error, EOFError) as e:
        raise ValueError(
            "the audio is not a PCM WAV file: this runtime decodes WAV only (ffmpeg, which reads other formats, "
            f"is not shipped): {e}"
        ) from None
    if width == 1:
        audio = (np.frombuffer(frames, dtype=np.uint8).astype(np.float32) - 128.0) / 128.0
    elif width == 2:
        audio = np.frombuffer(frames, dtype="<i2").astype(np.float32) / 32768.0
    elif width == 3:
        raw = np.frombuffer(frames, dtype=np.uint8).reshape(-1, 3)
        ints = (raw[:, 0].astype(np.int32) | (raw[:, 1].astype(np.int32) << 8) | (raw[:, 2].astype(np.int32) << 16))
        ints = np.where(ints >= 1 << 23, ints - (1 << 24), ints)
        audio = ints.astype(np.float32) / float(1 << 23)
    elif width == 4:
        audio = np.frombuffer(frames, dtype="<i4").astype(np.float32) / float(1 << 31)
    else:
        raise ValueError(f"unsupported WAV sample width {width}")
    if channels > 1:
        audio = audio.reshape(-1, channels).mean(axis=1)
    if rate != sampling_rate and len(audio) > 0:
        n = max(1, int(round(len(audio) * sampling_rate / rate)))
        audio = np.interp(np.linspace(0, len(audio) - 1, n), np.arange(len(audio)), audio).astype(np.float32)
    return audio
