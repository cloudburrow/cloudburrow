// The Java fixture's CloudEvent function (#1319).
package functions;

import com.google.cloud.functions.CloudEventsFunction;
import io.cloudevents.CloudEvent;
import java.nio.charset.StandardCharsets;

/** Event receives a CloudEvent in the Google schema and logs it. */
public class Event implements CloudEventsFunction {
  @Override
  public void accept(CloudEvent event) {
    String data =
        event.getData() == null
            ? ""
            : new String(event.getData().toBytes(), StandardCharsets.UTF_8);
    System.out.println(
        "received CloudEvent type=" + event.getType()
            + " subject=" + event.getSubject()
            + " data=" + data);
    System.out.flush();
  }
}
