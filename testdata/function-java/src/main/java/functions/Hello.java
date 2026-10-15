// The Java fixture used to verify Google Functions Framework and Buildpacks
// support (#1319). It is an ordinary Functions Framework function: no
// CloudBurrow-specific code, because the point is that a normal function runs
// unchanged. It answers as testdata/function does.
package functions;

import com.google.cloud.functions.HttpFunction;
import com.google.cloud.functions.HttpRequest;
import com.google.cloud.functions.HttpResponse;
import com.google.gson.Gson;
import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.google.gson.JsonParseException;
import com.google.gson.JsonParser;
import java.io.IOException;

/** Hello answers an HTTP request, echoing a name when given one. */
public class Hello implements HttpFunction {
  private static final Gson GSON = new Gson();

  @Override
  public void service(HttpRequest request, HttpResponse response) throws IOException {
    String name = "world";
    try {
      JsonElement body = JsonParser.parseReader(request.getReader());
      if (body.isJsonObject()) {
        JsonElement given = body.getAsJsonObject().get("name");
        if (given != null && given.isJsonPrimitive() && !given.getAsString().isEmpty()) {
          name = given.getAsString();
        }
      }
    } catch (JsonParseException e) {
      // Not JSON: the default greeting.
    }
    JsonObject out = new JsonObject();
    out.addProperty("greeting", "hello " + name);
    response.setContentType("application/json");
    response.getWriter().write(GSON.toJson(out));
  }
}
