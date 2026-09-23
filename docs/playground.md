# Console: AI Playground

Issue: [#48](https://github.com/cloudburrow/cloudburrow/issues/48) · Verified 2026-09-21

A console screen that runs **real inference** through the **same HTTP API the official SDK
drives**. It is off unless local AI is configured, and its absence changes nothing else.

---

## 1. It is a relay, not a second client

The screen never talks to the model. Every generation is forwarded, unchanged, to:

```
POST /v1beta1/projects/console/locations/us-central1/publishers/google/models/{model}:streamGenerateContent?alt=sse
```

— the path and body [the SDK sends](generation.md). The relay copies bytes back and does not
parse candidates, so it cannot reshape what the API said or invent a response when the API
failed. A console that reached the generator directly would be a second implementation, and
the first thing a second implementation does is disagree with the first.

`TestPlaygroundGeneratesThroughTheRealAPIPath` asserts the exact path and body, and that no
generation option is smuggled in.

## 2. What the screen says about the model

| Shown | Why |
|---|---|
| Model ID | The model that will actually run. |
| Publisher | `community` today, and labelled as such. |
| **"This model is a COMMUNITY conversion, not published by Google. Its output is not a Gemini result and is not labelled as one."** | The requirement, stated where the output appears rather than only in documentation. |
| Endpoint | The address being called. |
| Readiness | A **live probe**, not a guess from configuration. |

Readiness asks the endpoint for a model it does not serve and requires a **404**. That proves
the service is answering *and routing*, without starting a generation that would take seconds
and load a model. Anything else is reported as unavailable, with the reason.

## 3. Options are refused, and the screen says so

The screen offers no generation controls, because the API honours none. It lists all thirteen
refused inputs rather than omitting them silently — a control absent with no explanation reads
as an oversight; absent with a reason it reads as a decision.

That list is a claim about another package's behaviour, so it is checked:
`TestPlaygroundRefusalsMatchTheAPI` sends each one to the real API and fails if any is
accepted, or if the counts drift apart. The day the runtime gains a real temperature control,
the test fails and the screen stops telling users it is refused.

## 4. History

Bounded to 20 entries, held in memory for the page only. **Nothing is written to storage and
nothing leaves the browser** — which is why there is no setting to disable storing it: there
is nothing to disable. Each row records the prompt, time to first token, total, and outcome.

A cancelled run is recorded as **Cancelled**, not Failed. Recording a user's own cancellation
as a failure teaches them to distrust the history.

## 5. Verified in a browser

Against the real runtime and the real model, driven through Chromium:

```
Model       gemma-4-e2b-it-community
Publisher   community          Readiness  Ready
Notice      "…COMMUNITY conversion, not published by Google. Its output is not a
             Gemini result and is not labelled as one."
Refused     13 options, enumerated

prompt: "Count from one to twelve in words, one per line."
  output grew in 6 separate steps, 1628 ms → 2209 ms   ← streaming, not one late write
  first token 1543 ms · total 2410 ms
  12 lines produced

cancellation:
  Cancel enabled during the run, Run disabled       ✓
  output stopped growing after Cancel               ✓ (0 bytes after)
  "[cancelled]" shown, Run re-enabled               ✓
  history row: "Cancelled" (warn), not "Failed"     ✓
  runtime containers left behind: none              ✓

empty prompt → 400 {"error":"a prompt is required"}, nothing reaches the generation API
```

The time to first token is higher here than the [1.2 s measured against the endpoint
directly](generation.md): each request starts a container, and that start is included. It is
the honest cost of a runtime published as a one-shot binary.

### What the browser found

**A cancelled run was recorded as "Failed".** Every Go test passed — the request did end, the
stream did stop, the container was cleaned up. Only using the screen showed that the history
called the user's own cancellation a failure. Fixed, and the distinction is now in the render.

## 6. Off by default

With no `-local-ai-model`, `/api/services` does not advertise the playground, the navigation
does not show it, and the rest of the console is untouched.
`TestConsoleWithoutLocalAIDoesNotOfferThePlayground` asserts all three, and that a generation
request refuses rather than producing anything.

## 7. Not done

- **Model download / load / unload from the UI.** Acquisition is a CLI concern today
  (`internal/localai`); the screen reports what is configured and does not manage artifacts.
- **Embedding test view.** Blocked on a model — see [embeddings.md](embeddings.md). An
  embedding view with nothing behind it would be a screen that cannot work.
- **Custom prediction request/response UI.** [prediction.md](prediction.md) covers that
  contract; it is not wired into this screen.
- **Pixel parity with the Vertex reference screens.** Structural parity only, for the reason
  in [console-verification.md](console-verification.md): no dated reference screenshots exist.
