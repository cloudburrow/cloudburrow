package dev.cloudburrow.dataflow;

import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;
import org.apache.beam.sdk.Pipeline;
import org.apache.beam.sdk.io.gcp.pubsub.PubsubIO;
import org.apache.beam.sdk.io.gcp.pubsub.PubsubMessage;
import org.apache.beam.sdk.options.Description;
import org.apache.beam.sdk.options.PipelineOptionsFactory;
import org.apache.beam.sdk.options.StreamingOptions;
import org.apache.beam.sdk.metrics.Counter;
import org.apache.beam.sdk.metrics.Metrics;
import org.apache.beam.sdk.transforms.MapElements;
import org.apache.beam.sdk.transforms.SimpleFunction;

/**
 * A streaming pipeline that reads a Pub/Sub subscription's messages with
 * their attributes (PubsubIO.readMessagesWithAttributes) and publishes each
 * to a topic, its data upper-cased and an attribute added naming the Java
 * version it ran on (#2215), submitted with the Java SDK's DataflowRunner.
 */
public class PubsubCopy {
  public interface Options extends StreamingOptions {
    @Description("Subscription to read, projects/P/subscriptions/S")
    String getInputSubscription();

    void setInputSubscription(String v);

    @Description("Topic to publish to, projects/P/topics/T")
    String getOutputTopic();

    void setOutputTopic(String v);
  }

  static class Upper extends SimpleFunction<PubsubMessage, PubsubMessage> {
    private final Counter messages = Metrics.counter(PubsubCopy.class, "messages");

    @Override
    public PubsubMessage apply(PubsubMessage m) {
      messages.inc();
      Map<String, String> attrs = new HashMap<>();
      if (m.getAttributeMap() != null) {
        attrs.putAll(m.getAttributeMap());
      }
      attrs.put("java", System.getProperty("java.specification.version"));
      String data = new String(m.getPayload(), StandardCharsets.UTF_8).toUpperCase();
      return new PubsubMessage(data.getBytes(StandardCharsets.UTF_8), attrs);
    }
  }

  public static void main(String[] args) {
    Options options = PipelineOptionsFactory.fromArgs(args).withValidation().as(Options.class);
    options.setStreaming(true);
    Pipeline p = Pipeline.create(options);
    p.apply("ReadPubsub", PubsubIO.readMessagesWithAttributes().fromSubscription(options.getInputSubscription()))
        .apply("Upper", MapElements.via(new Upper()))
        .apply("WritePubsub", PubsubIO.writeMessages().to(options.getOutputTopic()));
    p.run();
  }
}
