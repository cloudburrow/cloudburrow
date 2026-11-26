# Apache Beam's GreedyPCollectionFusers, FlinkBatchPortablePipelineTranslator and ExecutableStageDoFnOperator, fixed

Source: Apache Beam v2.78.0 (tag commit `adbb48f0a6cbe686993a95adc3107d9d689b719c`),
`sdks/java/core/src/main/java/org/apache/beam/sdk/util/construction/graph/GreedyPCollectionFusers.java`
`runners/flink/src/main/java/org/apache/beam/runners/flink/FlinkBatchPortablePipelineTranslator.java`
and `runners/flink/src/main/java/org/apache/beam/runners/flink/translation/wrappers/streaming/ExecutableStageDoFnOperator.java`,
Copyright The Apache Software Foundation, Apache License 2.0 (`LICENSE`).

The modified copies are in `internal/beamrun/assets/java/` (they are embedded in the CLI from
there). Each Dataflow job's Flink job server (`apache/beam_flink1.20_job_server` 2.78.0,
`dependencies.json` `components.dataflow.beamFlinkJobServer`) compiles them against its own jar
as the job server starts and puts the classes before the jar on the classpath
(`internal/beamrun/beamrun.go`, `jobServerScript`).

Why: Beam's greedy fuser fuses a Flatten into the stages that produce its inputs, but when one of
the Flatten's inputs is materialized (it has another consumer that does not fuse, or a
runner-executed producer) the Flatten also runs on the runner, reading an input that no stage
produces. The job's translation then fails with "PCollectionNodes [...] were consumed but never
produced" (apache/beam#34647, closed unfixed). The Google-provided template
Cloud_Spanner_to_GCS_Avro hits it at its "Combine all files" Flatten (#2215).

And Beam's portable Flink runner never runs a DoFn's `@OnWindowExpiration`: the stateful stage's
operator calls `onWindowExpiration` on its SDK harness runner when a key's window expires (the
state's garbage-collection timer), and that method is empty, so the SDK is never sent the
ParDo's window-expiration timer, as the portable model has the runner do. `GroupIntoBatches`
flushes its last batch there, so an auto-sharded file write (`TextIO.write()` with no shard
count: the Flex Template Cloud_PubSub_to_GCS_Text_Flex's default) dropped the elements of its
window's last batch unless its processing-time buffering timer fired first (#2215).

Changes (each marked "Modified by CloudBurrow" in the file's header, as the Apache License asks):

- `GreedyPCollectionFusers.java`: the Flatten URN's fusibility checker is `canFuseFlatten`, which
  does not fuse a Flatten when one of its inputs is materialized whatever the Flatten does
  (produced by a runner-executed transform, read as a side input, or consumed by another
  transform, other than a Flatten, that cannot fuse into its stage); every input is then
  materialized and the runner's Flatten reads them all. Every other Flatten fuses as before.
- `FlinkBatchPortablePipelineTranslator.java`: `translateFlatten` gives each input whose type
  differs from the Flatten output's the output's type (an identity map) before Flink's union,
  which refuses inputs of different types ("Cannot union inputs of different types": the
  export's `KV<String, List<String>>` beside its `KV<String, Iterable<String>>`). The
  `@AutoService` annotation on `IsFlinkNativeTransform` is dropped (its processor is not on the
  job server's classpath; the jar's `META-INF/services` entry stands).
- `ExecutableStageDoFnOperator.java`: `SdkHarnessDoFnRunner.onWindowExpiration` sends, within the
  bundle (the state is cleaned up after it), the timer of each of the stage's ParDos'
  `@OnWindowExpiration` timer family (`ParDoPayload.on_window_expiration_timer_family_spec`) for
  the key and window, firing at the window's end with its hold at the start of time (the DoFn's
  output may carry any timestamp of the window). The SpotBugs `@SuppressFBWarnings` annotation is
  dropped (not on the job server's classpath; it has no effect on the class).

To update: copy the new Beam version's file, reapply the change, record the version here and run
the Dataflow compat tests (`TestDataflowProvidedSpanner`, `TestDataflowProvided`, `TestDataflowProvidedFlexTemplates`).
