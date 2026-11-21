# CloudBurrow patches to LiteRT-LM

The Dockerfile (`deploy/litert-lm/Dockerfile`) applies every `*.patch` here,
in file-name order, with `git apply` to the LiteRT-LM checkout at
`LITERT_LM_REF` (dependencies.json `localai.litertLM.commit`), before the Bazel
build. A patch that no longer applies fails the image build, so moving the
pin means re-cutting the patches against the new commit.

| Patch | Upstream | Why | Author, licence |
|---|---|---|---|
| `0001-decode-step-logprobs.patch` | Not upstream. Against `0082833d` (2026-11-18). | Vertex AI's `responseLogprobs` and `logprobs` (#2107): no LiteRT-LM API reports the top candidates of a decode step, and the samplers' own score is of the temperature-scaled, top-k/top-p-truncated distribution (0 for every greedy token). The patch adds `runtime/core/decode_step_observer.{h,cc}` (a process-wide observer, with the log-softmax and top-k over one row of logits) and calls it from `tasks.cc`'s external-sampler decode step with the logits as the model produced them, copied before the decoding constraints mask them. `cloudburrow_generate --response_logprobs [--logprobs=N]` installs the observer. | CloudBurrow, Apache-2.0 (LiteRT-LM's own licence) |
