// Package vertexai serves the subset of Vertex AI's generateContent surface
// that CloudBurrow's local runtime can actually perform.
//
// The scope is deliberately narrow and stated rather than implied. This is a
// compatibility layer for local development, not a Vertex AI emulator and not
// a Gemini replica. Three rules follow from that, and they are the reason the
// code looks stricter than an emulator usually does:
//
//  1. **No silent substitution.** A request for `gemini-2.0-flash` is refused,
//     not quietly answered by Gemma. The model that will run is named in the
//     response's modelVersion, and an alias must be configured explicitly
//     before it resolves to anything.
//
//  2. **Unsupported options are refused, not ignored.** The LiteRT-LM runtime
//     exposes no temperature, topP, topK, seed or stopSequences control, so a
//     request carrying them cannot be honoured. Accepting one and generating
//     anyway would report compliance that did not happen, which is worse than
//     an error because the caller cannot see it.
//
//  3. **No fabricated accounting.** usageMetadata is omitted rather than
//     estimated. A token count we did not measure is a number the caller would
//     reasonably trust.
//
// What the model produces is not a claim of this package. Protocol behaviour
// is deterministic and tested as such; model output is not asserted beyond
// being non-empty and correctly framed.
package vertexai
