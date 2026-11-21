# cloudburrow_generate: LiteRT-LM's litert_lm_main with the sampler and the
# token limit set per request (see cloudburrow_generate.cc).
# decode_step_observer is CloudBurrow's own target, added to the checkout by
# ../patches/0001-decode-step-logprobs.patch. The Dockerfile
# copies this directory into the LiteRT-LM checkout as //cloudburrow, so every
# dependency is one of upstream's public targets and the link options are
# litert_lm_main's own. The exported-symbols list is upstream's, copied beside
# this file by the Dockerfile because upstream does not export it.
cc_binary(
    name = "cloudburrow_generate",
    srcs = ["cloudburrow_generate.cc"],
    additional_linker_inputs = [":litert_lm_main.exported_symbols"],
    linkopts = ["-Wl,--dynamic-list=$(location :litert_lm_main.exported_symbols)"],
    deps = [
        "//runtime/components/constrained_decoding:constraint_provider_config",
        "//runtime/components/constrained_decoding:llg_constraint_config",
        "//runtime/components/constrained_decoding:repetition_penalty_config",
        "//runtime/components/constrained_decoding:tool_choice",
        "//runtime/conversation",
        "//runtime/conversation:end_of_turn",
        "//runtime/conversation:io_types",
        "//runtime/core:decode_step_observer",
        "//runtime/engine:engine_factory",
        "//runtime/engine:engine_impl_selected",
        "//runtime/engine:engine_interface",
        "//runtime/engine:engine_settings",
        "//runtime/engine:io_types",
        "//runtime/engine:shared_flags",
        "//runtime/executor:executor_settings_base",
        "//runtime/executor:llm_executor_settings",
        "//runtime/proto:sampler_params_cc_proto",
        "//support/tokenizer",
        "@com_google_absl//absl/base:log_severity",
        "@com_google_absl//absl/flags:flag",
        "@com_google_absl//absl/flags:parse",
        "@com_google_absl//absl/functional:any_invocable",
        "@com_google_absl//absl/log:globals",
        "@com_google_absl//absl/status",
        "@com_google_absl//absl/status:status_macros",
        "@com_google_absl//absl/status:statusor",
        "@com_google_absl//absl/strings",
        "@com_google_absl//absl/time",
        "@com_google_absl//absl/types:span",
        "@nlohmann_json//:json",
    ],
)
