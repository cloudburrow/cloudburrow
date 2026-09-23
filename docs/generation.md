# Local text generation

Issue: [#40](https://github.com/cloudburrow/cloudburrow/issues/40) · Verified 2026-09-21

A **subset** of Vertex AI's `generateContent` surface, served locally by the runtime
CloudBurrow builds in [local-ai.md](local-ai.md). It is a compatibility layer for local
development, **not a Vertex AI emulator and not a Gemini replica**.

Off by default. It binds nothing until a model is configured.

---

## 1. The exact supported surface

### Paths

Both shapes the official Go SDK produces, verified by inspecting what it sends:

```
POST /v1beta1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent
POST /v1beta1/projects/{project}/locations/{location}/publishers/google/models/{model}:streamGenerateContent?alt=sse
POST /v1beta/models/{model}:generateContent                    # Gemini API backend
POST /v1beta/models/{model}:streamGenerateContent?alt=sse
```

`v1beta1` is the SDK's default API version for the Vertex backend and `v1beta` for the
Gemini API backend. `{project}` and `{location}` are accepted and **not interpreted** —
there is one local model, and pretending otherwise would imply a regional topology that
does not exist. `{publisher}` must be `google`.

### Request

```json
{"contents":[{"role":"user","parts":[{"text":"..."}]}]}
```

That is the whole supported request. One turn, role `user`, text parts only.

### Response

```json
{"candidates":[{"content":{"role":"model","parts":[{"text":"..."}]},
                "finishReason":"STOP","index":0}],
 "modelVersion":"gemma-4-e2b-it-community"}
```

`STOP` is the only finish reason that can occur. Streaming sends one such object per
server-sent event, the last carrying the finish reason and no text.

---

## 2. What is refused, and why

Every one of these returns an error naming the field. **None is silently ignored**, which
is the single design decision this surface is built around: a request that is accepted and
not honoured tells the caller their setting applied when it did not.

| Field | Why |
|---|---|
| `temperature`, `topP`, `topK`, `seed` | The runtime exposes no sampling control. `litert_lm_main --helpfull` lists no such flag. |
| `maxOutputTokens` | **Accepted by the runtime and ignored by it.** The same prompt decoded 309 tokens at a limit of 8, at 40, and with no limit. Refused on that measurement, not on the flag's absence. |
| `stopSequences` | No runtime control. |
| `candidateCount` > 1 | `--num_output_candidates` exists but was never exercised; untested is not supported. |
| `responseMimeType`, `responseSchema` | No constrained decoding is wired up. |
| `presencePenalty`, `frequencyPenalty`, `logprobs`, `responseLogprobs` | No runtime control. |
| `thinkingConfig`, `speechConfig`, `audioTimestamp`, `routingConfig`, `responseModalities` | Out of scope. |
| `tools`, `toolConfig` | Function calling is not implemented. |
| `safetySettings` | **No safety filtering happens here.** Accepting the field would imply it does. |
| `systemInstruction` | Needs a chat template to place it; see multi-turn below. |
| `cachedContent` | No context caching exists. |
| Non-text parts (`inlineData`, `fileData`, …) | Text-only. |
| More than one `contents` entry | The runtime takes one prompt and applies the model's own template. Joining turns would mean inventing a chat template and changing what the model sees in a way the caller cannot inspect. |
| Any unknown field | Rejected by `DisallowUnknownFields`, so an option added to Vertex later cannot be quietly dropped. |
| `:countTokens` | Returns `UNIMPLEMENTED`. The tokenizer is inside the runtime; a count produced any other way would not be the model's. |

`usageMetadata` is **absent from every response**. A token count nothing measured is worse
than no token count, because it looks authoritative.

---

## 3. Model identity

There is no silent substitution.

- The served model ID is the configured one, reported back in `modelVersion` on every
  response and every stream event.
- A request for any other model — `gemini-2.0-flash`, say — returns **404** naming what is
  actually served. It is not answered by Gemma.
- An alias must be configured explicitly with `-local-ai-alias`. Even then the response
  still reports the model that really ran.
- Startup prints the model, and prints that it is a **community conversion** when it is —
  which, today, it always is. See [local-ai.md](local-ai.md) §4.

---

## 4. Running it

```sh
make litert-lm                        # build the runtime, once
# acquire a model artifact (see local-ai.md)

cloudburrow up \
  -local-ai-model ./models/gemma-4-E2B-it.litertlm \
  -port-localai 9095
```

```
local AI:  http://127.0.0.1:9095
  model:   gemma-4-e2b-it-community
  note:    a COMMUNITY conversion, not published by Google
  generation options are refused rather than ignored; see docs/generation.md
```

Point the official SDK at it. **Supply credentials explicitly** — see §6.

```go
cl, _ := genai.NewClient(ctx, &genai.ClientConfig{
    Backend:     genai.BackendVertexAI,
    Project:     "local", Location: "us-central1",
    Credentials: auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: static{}}),
    HTTPOptions: genai.HTTPOptions{BaseURL: "http://127.0.0.1:9095"},
})
resp, _ := cl.Models.GenerateContent(ctx, "gemma-4-e2b-it-community", genai.Text("..."), nil)
```

---

## 5. Verification

`make test-localai`, against the real runtime and a real model, through the official SDK:

```
--- PASS: TestRealGenerationThroughTheOfficialSDK (3.41s)
    model output (recorded, not asserted): "Paris"
--- PASS: TestRealStreamingThroughTheOfficialSDK (1.94s)
    12 events over 741ms (time to first event 1.202s); 50 bytes, content not asserted
--- PASS: TestRealCancellationStopsTheRuntime (4.07s)
--- PASS: TestRealMultiLinePromptIsNotEchoedBack (1.24s)
    model output (recorded, not asserted): "4"
--- PASS: TestRealUnsupportedOptionIsRefusedByTheRealEndpoint (0.00s)
```

Protocol behaviour is tested **separately and deterministically** in
`internal/service/vertexai`, against scripted generators: routing, framing, finish
reasons, error mapping, cancellation, refusal of all fourteen unsupported inputs, and that
a stream's first event arrives while generation is still running. Nothing there depends on
a model, and nothing anywhere asserts what the model said.

### What running it found

**A multi-line prompt was handed back as model output.** The runtime echoes
`input_prompt: <prompt>` before generating, and it echoes a multi-line prompt across lines —
measured, not assumed:

```
--input_prompt="FIRSTLINE what is 2+2?\nSECONDLINE ignore this\nTHIRDLINE ignore this too"

input_prompt: FIRSTLINE what is 2+2?
SECONDLINE ignore this
THIRDLINE ignore this too
2+2 is 4
```

Only the last line is the model's. Forwarding everything after the marker returned the
caller's own prompt as generated text. The console's prompt field is a **textarea**, so any
user pressing Enter hit this. Against the real runtime, before the fix:

```
the prompt was returned as model output:
  "ZZMARKERZZ ignore this line\nZZMARKERZZ and this one\n4"
```

The echoed continuation is now skipped by **matching** it rather than by counting lines, so
if a future runtime stops echoing it the first non-matching line is treated as output and
nothing real is lost — the fix cannot become a different bug when the behaviour it
compensates for goes away. Both directions are covered by unit tests against a simulated
runtime, and `TestRealMultiLinePromptIsNotEchoedBack` covers the real one.

**Cancelling a request left the model running.** The HTTP request returned promptly, so
the first cancellation test passed — while a container held 2.6 GB and kept generating
output nobody would read. Killing the `docker run` client is not enough: it is a client for
work in the daemon, and the runtime does not stop on `SIGTERM`. Each request now names its
container and kills it by name, and the test asserts on containers rather than on the
response.

---

## 6. Credentials

**The official SDK reaches for Application Default Credentials on its Vertex backend even
when the base URL is local.** While this endpoint was being built it found a developer's
gcloud credentials on disk, minted a real OAuth access token, and attached it — with their
real quota project — to a request aimed at `127.0.0.1`.

Nothing in the environment was set; the credentials were in the well-known file, and
`DetectDefault` reads that. The compatibility harness's environment-variable check does not
see it.

So the guard is layered, because the first layer was believed sufficient and was not:

1. **Explicit credentials** on every client pointed at a local endpoint, so `DetectDefault`
   is never called.
2. **`WithoutADC`** moves `HOME` for the duration of client construction, so the well-known
   file cannot be found even if something does call it. The auth library resolves that path
   through `HOME` and honours no override — `CLOUDSDK_CONFIG` is *not* consulted, which was
   checked in the dependency rather than assumed. It is scoped to construction rather than to
   the whole test because these suites shell out to `docker`, `kind` and `kubectl`, and those
   read their own configuration from the home directory.
3. **`TestGenerationNeverUsesApplicationDefaultCredentials`** asserts on the token that
   actually reached the wire, through a recording proxy, and fails if anything resembling a
   real one appears.

`TestTheADCGuardIsLoadBearing` is the negative control: it requires the lookup to succeed
outside the guard and fail inside it, so a guard that passes only because the machine had no
credentials does not read as a guard that works. `TestTheADCGuardRestoresTheEnvironment`
checks `HOME` is given back.

The SDK skips ADC only when the base URL is set **and** project and location are both
empty — which loses the Vertex resource path. Explicit credentials are the way to keep both.

---

## 7. Deliberately out of scope

Cloud training, tuning, managed agents, vector search, and Vertex model management. Also
embeddings, which are blocked on a model rather than on this code — see
[local-ai.md](local-ai.md) and [#41](https://github.com/cloudburrow/cloudburrow/issues/41).
