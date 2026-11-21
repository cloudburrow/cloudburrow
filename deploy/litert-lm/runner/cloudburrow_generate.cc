// Copyright 2025 The ODML Authors.
// Modifications Copyright 2026 The CloudBurrow Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// cloudburrow_generate: one prompt in, the model's answer out, with the
// sampler set per request.
//
// Derived from LiteRT-LM's runtime/engine/litert_lm_main.cc, which builds its
// session with SessionConfig::CreateDefault() and so cannot take temperature,
// top-k, top-p or a seed. This runner sets them on the session's
// SamplerParameters through Google's own C++ API and nothing else differs:
// same engine, same conversation, same model template.
//
// Its output contract is narrower than litert_lm_main's, on purpose: standard
// output carries the model's text and nothing else (no prompt echo, no
// benchmark block), flushed as each chunk decodes, so the caller needs no
// marker parsing. Errors go to standard error with a non-zero exit. After a
// successful generation the last line on standard error is
//   cloudburrow_generate: decoded_tokens=N
// from the engine's own benchmark counters, so the caller can tell a reply cut
// off at --max_output_tokens (finish reason MAX_TOKENS) from one that ended.
//
// Flags (the sampler and token-limit flags are LiteRT-LM's shared ones):
//   --model_path, --input_prompt, --backend (default cpu)
//   --max_output_tokens=N       caps decoded tokens (N > 0)
//   --sampler_temperature=T     0 selects greedy decoding
//   --sampler_top_k=K, --sampler_top_p=P, --sampler_seed=S
//   --constraint_regex=R        output must match R (#1779)
//   --constraint_json_schema=J  output must be JSON valid against schema J
//   --presence_penalty=P, --frequency_penalty=F   LiteRT-LM's
//                               RepetitionPenaltyConfig (OpenAI semantics)
//   --vision_backend=cpu        load the model's vision encoder (image parts)
//   --request_stdin             read a whole conversation from standard input
//   --enable_thinking=B         set the conversation's ThinkingConfig (#2107):
//                               the model's own thinking mode (Gemma 4's
//                               template injects <|think|>), its thought
//                               channel streamed as {"thought":"..."} lines
//   --thinking_budget=N         the thinking token budget, -1 unlimited
//                               (LiteRT-LM's ThinkingBudgetConstraint)
//   --response_logprobs         decode on an Engine::Session instead of the
//                               Conversation, and write each sampled token's
//                               {"text":"...","logprob":{"token":"<base64>",
//                               "id":N,"score":S}} (#2107)
//
// --response_logprobs exists because the Conversation API drops the scores:
// the sampler writes each sampled token's log-probability
// (Sampler::SampleToIdAndScoreBuffer) and the session streams it in
// Responses::GetScores, but Conversation turns each step into a Message
// without it. The prompt is the Conversation's own rendering of the same
// preface and message (RenderPrefaceIntoString, RenderMessageIntoString: the
// model's chat template and generation prompt), prefilled on a session with
// the conversation's own SessionConfig and the CPU sampler as an external
// sampler, the one path on which LiteRT-LM reports scores. That path parses
// no channels, tool calls or constraints, so the caller sends it plain text
// requests only. No LiteRT-LM API exposes the other candidates' scores at a
// step (what Vertex's logprobs, the top candidates, needs): the sampler
// returns the sampled token's alone, and RunTextScoring scores only targets it
// is given, one full-vocabulary sort each.
//
// With --request_stdin the runner takes one JSON object on standard input
// (#1937) instead of --input_prompt:
//   {"preface": [messages...],      system turn and earlier turns
//    "tools": [{"type":"function","function":{...}}],   optional
//    "tool_choice": {"type":"auto"|"none"|"required"|"allowed",
//                    "names":[...]},                     optional
//    "message": {...},              the turn the model answers
//    "count_only": false,
//    "compute": ["text", ...]}       optional: tokenize only (computeTokens)
// in LiteRT-LM's Conversation message format, so the model's own chat
// template (Gemma 4's, with its native tool-call syntax) frames every turn.
// Its standard output is JSON lines instead of raw text:
//   {"prompt_tokens":N}             Conversation::CountTokens of the turn,
//                                   preface, tools and image tokens included
//   {"text":"..."}                  each decoded chunk
//   {"tool_calls":[...]}            parsed calls, arguments as JSON objects
//   {"thought":"..."}               thought channel text (--enable_thinking)
// and with count_only only the first line is written and nothing decodes.
// With thinking on, standard error also gets
//   cloudburrow_generate: thought_tokens=N
// the model's tokenizer's count of the thought channel's text.
// With "compute" the model's tokenizer splits each text (no template) and the
// one line written is {"compute":[{"ids":[...],"tokens":["<base64>",...]}]},
// each token the bytes its id decodes to.
// Tool choice is applied by the Gemma 4 processor's LLGuidance tool-call
// grammar (the image builds with --define=LITERT_LM_FST_CONSTRAINTS_DISABLED=1;
// the FST provider supports only "auto").
//
// Constraints are LiteRT-LM's llguidance provider, applied to the decode of
// this conversation's message, so the model's chat template still frames the
// prompt. litert_lm_main accepts --constraint_regex and ignores it (it never
// reaches the conversation, #1779); litert_lm_advanced_main honours it only in
// its raw session mode, which skips the template.

#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <iterator>
#include <memory>
#include <optional>
#include <string>
#include <utility>
#include <vector>

#include "absl/base/log_severity.h"  // from @com_google_absl
#include "absl/flags/flag.h"  // from @com_google_absl
#include "absl/flags/parse.h"  // from @com_google_absl
#include "absl/functional/any_invocable.h"  // from @com_google_absl
#include "absl/log/globals.h"  // from @com_google_absl
#include "absl/status/status.h"  // from @com_google_absl
#include "absl/status/status_macros.h"  // from @com_google_absl
#include "absl/status/statusor.h"  // from @com_google_absl
#include "absl/strings/escaping.h"  // from @com_google_absl
#include "absl/strings/str_cat.h"  // from @com_google_absl
#include "absl/time/time.h"  // from @com_google_absl
#include "absl/types/span.h"  // from @com_google_absl
#include "nlohmann/json.hpp"  // from @nlohmann_json
#include "runtime/components/constrained_decoding/constraint_provider_config.h"
#include "runtime/components/constrained_decoding/llg_constraint_config.h"
#include "runtime/components/constrained_decoding/repetition_penalty_config.h"
#include "runtime/components/constrained_decoding/tool_choice.h"
#include "runtime/conversation/conversation.h"
#include "runtime/conversation/end_of_turn.h"
#include "runtime/conversation/io_types.h"
#include "runtime/engine/engine.h"
#include "runtime/engine/engine_factory.h"
#include "runtime/engine/engine_settings.h"
#include "runtime/engine/io_types.h"
#include "runtime/engine/shared_flags.h"
#include "runtime/executor/executor_settings_base.h"
#include "runtime/executor/llm_executor_settings.h"
#include "runtime/proto/sampler_params.pb.h"
#include "support/tokenizer/tokenizer.h"

ABSL_FLAG(std::string, backend, "cpu", "Executor backend (cpu, gpu, ...).");
ABSL_FLAG(std::string, model_path, "", "Path of the .litertlm model.");
ABSL_FLAG(std::string, input_prompt, "", "The prompt.");
ABSL_FLAG(std::string, constraint_json_schema, "",
          "Constrain the output to JSON valid against this JSON schema.");
ABSL_FLAG(bool, request_stdin, false,
          "Read the conversation as JSON from standard input (#1937).");
ABSL_FLAG(std::optional<bool>, enable_thinking, std::nullopt,
          "Set the conversation's thinking mode (#2107).");
ABSL_FLAG(int, thinking_budget, -1,
          "Thinking token budget when thinking is enabled; -1 is unlimited.");
ABSL_FLAG(bool, response_logprobs, false,
          "Decode on an Engine::Session and write each sampled token's "
          "log-probability (#2107).");

namespace {

using ::litert::lm::Backend;
using ::litert::lm::Conversation;
using ::litert::lm::ConversationConfig;
using ::litert::lm::EngineSettings;
using ::litert::lm::Message;
using ::litert::lm::ModelAssets;
using ::litert::lm::OptionalArgs;
using ::litert::lm::ThinkingConfig;
using ::litert::lm::ToolChoice;
using ::litert::lm::proto::SamplerParameters;
using ::nlohmann::json;

// Upstream's conventional top-k when only top-p or temperature is given
// (litert_lm_lib.cc uses the same 40), so a stochastic request never
// degenerates to k = 1.
constexpr int kDefaultTopK = 40;

bool AnySamplerFlag() {
  return absl::GetFlag(FLAGS_sampler_top_k).has_value() ||
         absl::GetFlag(FLAGS_sampler_top_p).has_value() ||
         absl::GetFlag(FLAGS_sampler_temperature).has_value() ||
         absl::GetFlag(FLAGS_sampler_seed).has_value();
}

// Builds the request's sampler: the model's own defaults, with each flag the
// caller set laid over them. Starting from the model's defaults (rather than
// from a fixed set) means a request that sets only a seed, or only top-k,
// keeps everything else exactly as litert_lm_main would have decoded it.
absl::StatusOr<SamplerParameters> RequestSampler(
    const EngineSettings& settings) {
  SamplerParameters p;
  if (settings.GetLlmMetadata().has_value() &&
      settings.GetLlmMetadata()->has_sampler_params()) {
    p = settings.GetLlmMetadata()->sampler_params();
  }
  if (p.type() == SamplerParameters::TYPE_UNSPECIFIED ||
      p.type() == SamplerParameters::GREEDY) {
    // A greedy or absent default has no k/p/temperature to inherit.
    p.set_type(SamplerParameters::TOP_P);
    p.set_k(kDefaultTopK);
    p.set_p(1.0f);
    p.set_temperature(1.0f);
  }
  if (auto k = absl::GetFlag(FLAGS_sampler_top_k); k.has_value()) {
    if (*k < 1) {
      return absl::InvalidArgumentError(
          absl::StrCat("sampler_top_k must be >= 1, got ", *k));
    }
    p.set_k(*k);
  }
  if (auto tp = absl::GetFlag(FLAGS_sampler_top_p); tp.has_value()) {
    if (!(*tp >= 0.0 && *tp <= 1.0)) {
      return absl::InvalidArgumentError(
          absl::StrCat("sampler_top_p must be in [0, 1], got ", *tp));
    }
    p.set_p(static_cast<float>(*tp));
  }
  if (auto s = absl::GetFlag(FLAGS_sampler_seed); s.has_value()) {
    p.set_seed(*s);
  }
  if (auto t = absl::GetFlag(FLAGS_sampler_temperature); t.has_value()) {
    if (!(*t >= 0.0 && *t < 1e6)) {
      return absl::InvalidArgumentError(
          absl::StrCat("sampler_temperature must be >= 0, got ", *t));
    }
    if (*t == 0.0) {
      // Temperature 0 is argmax by definition; GREEDY says so to the
      // sampler instead of dividing logits by zero.
      p.set_type(SamplerParameters::GREEDY);
      p.set_k(1);
    } else {
      p.set_temperature(static_cast<float>(*t));
    }
  }
  return p;
}

// ToolChoiceOf reads the request's tool_choice object.
absl::StatusOr<ToolChoice> ToolChoiceOf(const Message& tc) {
  ToolChoice out;
  const std::string type = tc.value("type", "auto");
  std::vector<std::string> names;
  if (tc.contains("names")) {
    for (const auto& n : tc["names"]) names.push_back(n.get<std::string>());
  }
  if (type == "auto") {
    out.choice = ToolChoice::Auto{};
  } else if (type == "none") {
    out.choice = ToolChoice::None{};
  } else if (type == "required") {
    if (names.size() == 1) {
      out.choice = ToolChoice::ForcedFunction{.name = names[0]};
    } else if (!names.empty()) {
      out.choice = ToolChoice::AllowedTools{.names = names, .required = true};
    } else {
      out.choice = ToolChoice::Required{};
    }
  } else if (type == "allowed") {
    out.choice = ToolChoice::AllowedTools{.names = names, .required = false};
  } else {
    return absl::InvalidArgumentError(
        absl::StrCat("unknown tool_choice type ", type));
  }
  return out;
}

void JsonLine(const json& j) { std::cout << j.dump() << "\n" << std::flush; }

// DecodeWithScores answers the request on an Engine::Session that streams
// the sampler's score of each token it samples (see --response_logprobs).
absl::Status DecodeWithScores(litert::lm::Engine& engine,
                              Conversation& conversation,
                              const ConversationConfig& conversation_config,
                              const Message& request, const OptionalArgs& args) {
  const Message& message = request["message"];
  ABSL_ASSIGN_OR_RETURN(int prompt_tokens,
                        conversation.CountTokens({message}, args));
  JsonLine({{"prompt_tokens", prompt_tokens}});
  // The text the Conversation would prefill: the preface (when the
  // conversation prefilled it at creation, RenderMessageIntoString renders
  // only the turn after it), the message and the generation prompt.
  std::string prompt;
  if (conversation_config.prefill_preface_on_init()) {
    ABSL_ASSIGN_OR_RETURN(prompt,
                          conversation.RenderPrefaceIntoString(OptionalArgs()));
  }
  ABSL_ASSIGN_OR_RETURN(std::string turn, conversation.RenderMessageIntoString(
                                              message, OptionalArgs()));
  prompt += turn;

  litert::lm::SessionConfig session_config =
      conversation_config.GetSessionConfig();
  session_config.SetApplyPromptTemplateInSession(false);
  session_config.SetUseExternalSampler(true);
  session_config.SetSamplerBackend(Backend::CPU);
  ABSL_ASSIGN_OR_RETURN(auto session, engine.CreateSession(session_config));
  std::vector<litert::lm::InputData> inputs;
  inputs.emplace_back(litert::lm::InputText(prompt));
  ABSL_RETURN_IF_ERROR(session->RunPrefill(inputs));

  auto decode_config = litert::lm::DecodeConfig::CreateDefault();
  if (args.max_output_tokens.has_value()) {
    decode_config.SetMaxOutputTokens(*args.max_output_tokens);
  }
  if (args.repetition_penalty_config.has_value()) {
    decode_config.SetRepetitionPenaltyConfig(*args.repetition_penalty_config);
  }
  auto& tokenizer =
      const_cast<litert::support::Tokenizer&>(engine.GetTokenizer());
  absl::Status failure = absl::OkStatus();
  int decoded = 0;
  ABSL_ASSIGN_OR_RETURN(
      auto task,
      session->RunDecodeAsync(
          [&](absl::StatusOr<litert::lm::Responses> responses) {
            if (!responses.ok()) {
              failure = responses.status();
              return;
            }
            if (responses->GetTexts().empty()) return;
            const std::string& text = responses->GetTexts()[0];
            const auto& ids = responses->GetTokenIds();
            const auto& scores = responses->GetScores();
            if (text.empty()) return;
            if (scores.empty()) {
              // The decode's flush: text held back until the end (a partial
              // UTF-8 sequence or stop-token prefix). The session reports no
              // score with it, so it has no logprob line.
              JsonLine({{"text", text}});
              return;
            }
            if (ids.empty() || ids[0].size() != 1) {
              // A step without exactly one token and its score has no
              // score to report: fail rather than invent one.
              if (failure.ok()) {
                failure = absl::InternalError(absl::StrCat(
                    "the session reported ", scores.size(), " scores for ",
                    ids.empty() ? 0 : ids[0].size(), " tokens in one step"));
              }
              return;
            }
            const int id = ids[0][0];
            std::vector<int> one = {id};
            auto piece = tokenizer.TokenIdsToText(absl::MakeConstSpan(one));
            ++decoded;
            JsonLine({{"text", text},
                      {"logprob",
                       {{"token", absl::Base64Escape(piece.ok() ? *piece : text)},
                        {"id", id},
                        {"score", scores[0]}}}});
          },
          decode_config));
  ABSL_RETURN_IF_ERROR(task->WaitUntilDone(absl::Minutes(10)));
  ABSL_RETURN_IF_ERROR(failure);
  std::cout << std::flush;
  std::cerr << "cloudburrow_generate: decoded_tokens=" << decoded << std::endl;
  return absl::OkStatus();
}

absl::Status Run(int argc, char** argv) {
  absl::ParseCommandLine(argc, argv);
  absl::SetMinLogLevel(absl::LogSeverityAtLeast::kError);
  absl::SetStderrThreshold(absl::LogSeverityAtLeast::kError);

  const std::string model_path = absl::GetFlag(FLAGS_model_path);
  if (model_path.empty()) {
    return absl::InvalidArgumentError("--model_path is empty");
  }
  const bool request_stdin = absl::GetFlag(FLAGS_request_stdin);
  const std::string prompt = absl::GetFlag(FLAGS_input_prompt);
  Message request;
  if (request_stdin) {
    std::string body((std::istreambuf_iterator<char>(std::cin)),
                     std::istreambuf_iterator<char>());
    // Parsed in order: a tool's parameters keep the caller's property
    // order when the template renders them.
    request = Message::parse(body, nullptr, /*allow_exceptions=*/false);
    if (request.is_discarded() || !request.is_object() ||
        !request.contains("message")) {
      return absl::InvalidArgumentError(
          "--request_stdin: standard input is not a request object");
    }
  } else if (prompt.empty()) {
    return absl::InvalidArgumentError("--input_prompt is empty");
  }
  const std::optional<std::string> vision_flag =
      absl::GetFlag(FLAGS_vision_backend);
  std::optional<Backend> vision_backend;
  if (vision_flag.has_value() && !vision_flag->empty()) {
    ABSL_ASSIGN_OR_RETURN(Backend vb,
                          litert::lm::GetBackendFromString(*vision_flag));
    vision_backend = vb;
  }
  ABSL_ASSIGN_OR_RETURN(ModelAssets assets, ModelAssets::Create(model_path));
  ABSL_ASSIGN_OR_RETURN(
      Backend backend,
      litert::lm::GetBackendFromString(absl::GetFlag(FLAGS_backend)));
  ABSL_ASSIGN_OR_RETURN(
      EngineSettings engine_settings,
      EngineSettings::CreateDefault(std::move(assets), backend,
                                    vision_backend));
  // Benchmark counters on, as litert_lm_main has them: they are what reports
  // how many tokens the decode produced.
  engine_settings.GetMutableBenchmarkParams() =
      litert::lm::proto::BenchmarkParams();
  if (backend == Backend::CPU) {
    auto& executor = engine_settings.GetMutableMainExecutorSettings();
    ABSL_ASSIGN_OR_RETURN(auto cpu,
                          executor.MutableBackendConfig<litert::lm::CpuConfig>());
    if (const int n = absl::GetFlag(FLAGS_num_cpu_threads); n > 0) {
      cpu.number_of_threads = n;
    }
    executor.SetBackendConfig(cpu);
  }
  ABSL_ASSIGN_OR_RETURN(auto engine, litert::lm::EngineFactory::CreateDefault(
                                         std::move(engine_settings)));

  if (request_stdin && request.contains("compute")) {
    auto& tokenizer =
        const_cast<litert::support::Tokenizer&>(engine->GetTokenizer());
    json out = json::array();
    for (const auto& text : request["compute"]) {
      ABSL_ASSIGN_OR_RETURN(auto ids,
                            tokenizer.TextToTokenIds(text.get<std::string>()));
      json id_list = json::array();
      json tokens = json::array();
      for (int id : ids) {
        id_list.push_back(id);
        std::vector<int> one = {id};
        ABSL_ASSIGN_OR_RETURN(std::string piece, tokenizer.TokenIdsToText(
                                                     absl::MakeConstSpan(one)));
        tokens.push_back(absl::Base64Escape(piece));
      }
      out.push_back({{"ids", id_list}, {"tokens", tokens}});
    }
    JsonLine({{"compute", out}});
    return absl::OkStatus();
  }

  auto session_config = litert::lm::SessionConfig::CreateDefault();
  if (vision_backend.has_value()) {
    session_config.SetVisionModalityEnabled(true);
  }
  if (AnySamplerFlag()) {
    ABSL_ASSIGN_OR_RETURN(SamplerParameters sampler,
                          RequestSampler(engine->GetEngineSettings()));
    session_config.GetMutableSamplerParams() = sampler;
    std::cerr << "cloudburrow_generate: sampler type=" << sampler.type()
              << " k=" << sampler.k() << " p=" << sampler.p()
              << " temperature=" << sampler.temperature()
              << " seed=" << sampler.seed() << std::endl;
  }
  const std::string regex = absl::GetFlag(FLAGS_constraint_regex);
  const std::string schema = absl::GetFlag(FLAGS_constraint_json_schema);
  if (!regex.empty() && !schema.empty()) {
    return absl::InvalidArgumentError(
        "--constraint_regex and --constraint_json_schema are exclusive");
  }
  ConversationConfig::Builder builder;
  builder.SetSessionConfig(session_config);
  const bool has_tools = request_stdin && request.contains("tools") &&
                         request["tools"].is_array() &&
                         !request["tools"].empty();
  if (request_stdin) {
    litert::lm::JsonPreface preface;
    preface.messages = request.contains("preface")
                           ? request["preface"]
                           : Message::array();
    if (has_tools) {
      preface.tools = request["tools"];
      // The tool-call grammar parses the calls' arguments against their
      // declared schemas and applies tool_choice (#1937).
      builder.SetEnableConstrainedDecoding(true);
    }
    builder.SetPreface(preface);
  }
  const std::optional<bool> thinking = absl::GetFlag(FLAGS_enable_thinking);
  if (thinking.has_value()) {
    builder.SetThinkingConfig(
        ThinkingConfig(*thinking, absl::GetFlag(FLAGS_thinking_budget)));
  }
  if (!regex.empty() || !schema.empty() || has_tools) {
    // Without a provider the conversation drops decoding_constraint silently.
    builder.SetConstraintProviderConfig(litert::lm::LlGuidanceConfig());
  }
  ABSL_ASSIGN_OR_RETURN(auto conversation_config, builder.Build(*engine));
  ABSL_ASSIGN_OR_RETURN(std::unique_ptr<Conversation> conversation,
                        Conversation::Create(*engine, conversation_config));

  OptionalArgs args;
  const double presence = absl::GetFlag(FLAGS_presence_penalty);
  const double frequency = absl::GetFlag(FLAGS_frequency_penalty);
  if (presence != 0.0 || frequency != 0.0) {
    args.repetition_penalty_config = litert::lm::RepetitionPenaltyConfig(
        1.0f, static_cast<float>(presence), static_cast<float>(frequency),
        /*window_size=*/0);
  }
  if (has_tools && request.contains("tool_choice")) {
    ABSL_ASSIGN_OR_RETURN(ToolChoice choice,
                          ToolChoiceOf(request["tool_choice"]));
    args.tool_choice = std::move(choice);
  }
  if (const int n = absl::GetFlag(FLAGS_max_output_tokens); n > 0) {
    args.max_output_tokens = n;
  }
  if (!regex.empty()) {
    args.decoding_constraint = litert::lm::LlGuidanceConstraintArg{
        .constraint_type = litert::lm::LlgConstraintType::kRegex,
        .constraint_string = regex};
  } else if (!schema.empty()) {
    args.decoding_constraint = litert::lm::LlGuidanceConstraintArg{
        .constraint_type = litert::lm::LlgConstraintType::kJsonSchema,
        .constraint_string = schema};
  }

  if (absl::GetFlag(FLAGS_response_logprobs)) {
    if (!request_stdin) {
      return absl::InvalidArgumentError(
          "--response_logprobs needs --request_stdin");
    }
    return DecodeWithScores(*engine, *conversation, conversation_config,
                            request, args);
  }

  absl::Status failure = absl::OkStatus();
  std::string thought_text;
  Message message;
  if (request_stdin) {
    message = request["message"];
    ABSL_ASSIGN_OR_RETURN(int prompt_tokens,
                          conversation->CountTokens({message}, args));
    JsonLine({{"prompt_tokens", prompt_tokens}});
    if (request.value("count_only", false)) {
      return absl::OkStatus();
    }
  } else {
    json content = json::array();
    content.push_back({{"type", "text"}, {"text", prompt}});
    message = Message(json::object({{"role", "user"}, {"content", content}}));
  }
  ABSL_RETURN_IF_ERROR(conversation->SendMessageAsync(
      message,
      [&failure, &thought_text, request_stdin](absl::StatusOr<Message> message) {
        if (!message.ok()) {
          failure = message.status();
          return;
        }
        if (::litert::lm::IsEndOfTurnMessage(*message)) {
          return;
        }
        if (request_stdin) {
          if (message->contains("channels") &&
              (*message)["channels"].is_object()) {
            for (const auto& [name, text] : (*message)["channels"].items()) {
              if (name == "thought" && text.is_string() &&
                  !text.get<std::string>().empty()) {
                thought_text += text.get<std::string>();
                JsonLine({{"thought", text.get<std::string>()}});
              }
            }
          }
          if (message->contains("content") &&
              (*message)["content"].is_array()) {
            for (const auto& part : (*message)["content"]) {
              if (part.contains("text") &&
                  !part["text"].get<std::string>().empty()) {
                JsonLine({{"text", part["text"].get<std::string>()}});
              }
            }
          }
          if (message->contains("tool_calls") &&
              !(*message)["tool_calls"].empty()) {
            JsonLine({{"tool_calls", json::parse((*message)["tool_calls"].dump())}});
          }
          return;
        }
        for (const auto& part : (*message)["content"]) {
          if (part.contains("text")) {
            std::cout << part["text"].get<std::string>();
          }
        }
        std::cout << std::flush;
      },
      std::move(args)));
  ABSL_RETURN_IF_ERROR(engine->WaitUntilDone(absl::Minutes(10)));
  ABSL_RETURN_IF_ERROR(failure);
  std::cout << std::flush;
  auto info = conversation->GetBenchmarkInfo();
  if (info.ok() && info->GetTotalDecodeTurns() > 0) {
    uint64_t decoded = 0;
    for (int i = 0; i < static_cast<int>(info->GetTotalDecodeTurns()); ++i) {
      if (auto turn = info->GetDecodeTurn(i); turn.ok()) {
        decoded += turn->num_tokens;
      }
    }
    std::cerr << "cloudburrow_generate: decoded_tokens=" << decoded
              << std::endl;
  }
  if (thinking.value_or(false)) {
    int thought_tokens = 0;
    if (!thought_text.empty()) {
      auto& tokenizer =
          const_cast<litert::support::Tokenizer&>(engine->GetTokenizer());
      auto ids = tokenizer.TextToTokenIds(thought_text);
      if (ids.ok()) thought_tokens = static_cast<int>(ids->size());
    }
    std::cerr << "cloudburrow_generate: thought_tokens=" << thought_tokens
              << std::endl;
  }
  return absl::OkStatus();
}

}  // namespace

int main(int argc, char** argv) {
  absl::Status status = Run(argc, argv);
  std::cout << std::flush;
  if (!status.ok()) {
    std::cerr << "cloudburrow_generate: " << status << std::endl;
    return 1;
  }
  return 0;
}
