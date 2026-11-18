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
//
// Constraints are LiteRT-LM's llguidance provider, applied to the decode of
// this conversation's message, so the model's chat template still frames the
// prompt. litert_lm_main accepts --constraint_regex and ignores it (it never
// reaches the conversation, #1779); litert_lm_advanced_main honours it only in
// its raw session mode, which skips the template.

#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <memory>
#include <optional>
#include <string>
#include <utility>

#include "absl/base/log_severity.h"  // from @com_google_absl
#include "absl/flags/flag.h"  // from @com_google_absl
#include "absl/flags/parse.h"  // from @com_google_absl
#include "absl/functional/any_invocable.h"  // from @com_google_absl
#include "absl/log/globals.h"  // from @com_google_absl
#include "absl/status/status.h"  // from @com_google_absl
#include "absl/status/status_macros.h"  // from @com_google_absl
#include "absl/status/statusor.h"  // from @com_google_absl
#include "absl/strings/str_cat.h"  // from @com_google_absl
#include "absl/time/time.h"  // from @com_google_absl
#include "nlohmann/json.hpp"  // from @nlohmann_json
#include "runtime/components/constrained_decoding/constraint_provider_config.h"
#include "runtime/components/constrained_decoding/llg_constraint_config.h"
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

ABSL_FLAG(std::string, backend, "cpu", "Executor backend (cpu, gpu, ...).");
ABSL_FLAG(std::string, model_path, "", "Path of the .litertlm model.");
ABSL_FLAG(std::string, input_prompt, "", "The prompt.");
ABSL_FLAG(std::string, constraint_json_schema, "",
          "Constrain the output to JSON valid against this JSON schema.");

namespace {

using ::litert::lm::Backend;
using ::litert::lm::Conversation;
using ::litert::lm::ConversationConfig;
using ::litert::lm::EngineSettings;
using ::litert::lm::Message;
using ::litert::lm::ModelAssets;
using ::litert::lm::OptionalArgs;
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

absl::Status Run(int argc, char** argv) {
  absl::ParseCommandLine(argc, argv);
  absl::SetMinLogLevel(absl::LogSeverityAtLeast::kError);
  absl::SetStderrThreshold(absl::LogSeverityAtLeast::kError);

  const std::string model_path = absl::GetFlag(FLAGS_model_path);
  if (model_path.empty()) {
    return absl::InvalidArgumentError("--model_path is empty");
  }
  const std::string prompt = absl::GetFlag(FLAGS_input_prompt);
  if (prompt.empty()) {
    return absl::InvalidArgumentError("--input_prompt is empty");
  }
  ABSL_ASSIGN_OR_RETURN(ModelAssets assets, ModelAssets::Create(model_path));
  ABSL_ASSIGN_OR_RETURN(
      Backend backend,
      litert::lm::GetBackendFromString(absl::GetFlag(FLAGS_backend)));
  ABSL_ASSIGN_OR_RETURN(
      EngineSettings engine_settings,
      EngineSettings::CreateDefault(std::move(assets), backend));
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

  auto session_config = litert::lm::SessionConfig::CreateDefault();
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
  if (!regex.empty() || !schema.empty()) {
    // Without a provider the conversation drops decoding_constraint silently.
    builder.SetConstraintProviderConfig(litert::lm::LlGuidanceConfig());
  }
  ABSL_ASSIGN_OR_RETURN(auto conversation_config, builder.Build(*engine));
  ABSL_ASSIGN_OR_RETURN(std::unique_ptr<Conversation> conversation,
                        Conversation::Create(*engine, conversation_config));

  OptionalArgs args;
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

  absl::Status failure = absl::OkStatus();
  json content = json::array();
  content.push_back({{"type", "text"}, {"text", prompt}});
  ABSL_RETURN_IF_ERROR(conversation->SendMessageAsync(
      json::object({{"role", "user"}, {"content", content}}),
      [&failure](absl::StatusOr<Message> message) {
        if (!message.ok()) {
          failure = message.status();
          return;
        }
        if (::litert::lm::IsEndOfTurnMessage(*message)) {
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
