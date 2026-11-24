package dev.cloudburrow.dataflow;

import java.util.Arrays;
import org.apache.beam.sdk.Pipeline;
import org.apache.beam.sdk.io.TextIO;
import org.apache.beam.sdk.metrics.Counter;
import org.apache.beam.sdk.metrics.Metrics;
import org.apache.beam.sdk.options.Description;
import org.apache.beam.sdk.options.PipelineOptions;
import org.apache.beam.sdk.options.PipelineOptionsFactory;
import org.apache.beam.sdk.transforms.Count;
import org.apache.beam.sdk.transforms.DoFn;
import org.apache.beam.sdk.transforms.Filter;
import org.apache.beam.sdk.transforms.FlatMapElements;
import org.apache.beam.sdk.transforms.MapElements;
import org.apache.beam.sdk.transforms.ParDo;
import org.apache.beam.sdk.values.KV;
import org.apache.beam.sdk.values.TypeDescriptors;

/** Beam's word count, submitted with the Java SDK's DataflowRunner (#2215). */
public class WordCount {
  public interface Options extends PipelineOptions {
    @Description("Input files")
    String getInput();

    void setInput(String v);

    @Description("Output prefix")
    String getOutput();

    void setOutput(String v);
  }

  static class CountLines extends DoFn<String, String> {
    private final Counter lines = Metrics.counter(WordCount.class, "lines");

    @ProcessElement
    public void process(@Element String line, OutputReceiver<String> out) {
      lines.inc();
      out.output(line);
    }
  }

  public static void main(String[] args) {
    Options options = PipelineOptionsFactory.fromArgs(args).withValidation().as(Options.class);
    Pipeline p = Pipeline.create(options);
    p.apply("Read", TextIO.read().from(options.getInput()))
        .apply("Lines", ParDo.of(new CountLines()))
        .apply("Split", FlatMapElements.into(TypeDescriptors.strings())
            .via((String line) -> Arrays.asList(line.toLowerCase().split("[^a-z']+"))))
        .apply("NonEmpty", Filter.by((String w) -> !w.isEmpty()))
        .apply("Count", Count.perElement())
        .apply("Format", MapElements.into(TypeDescriptors.strings())
            .via((KV<String, Long> kv) -> kv.getKey() + ": " + kv.getValue()))
        .apply("Write", TextIO.write().to(options.getOutput()).withoutSharding());
    p.run();
  }
}
