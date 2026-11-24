package dev.cloudburrow.dataflow;

import com.google.auth.Credentials;
import java.net.URI;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import org.apache.beam.sdk.extensions.gcp.auth.CredentialFactory;
import org.apache.beam.sdk.options.PipelineOptions;

/**
 * A fixed access token of the instance (CB_ACCESS_TOKEN): no metadata server is reachable. A
 * plain Credentials, as gRPC demands TLS of GoogleCredentials only and the emulators' channels
 * are plaintext.
 */
public class TokenCredentialFactory implements CredentialFactory {
  public static TokenCredentialFactory fromOptions(PipelineOptions options) {
    return new TokenCredentialFactory();
  }

  @Override
  public Credentials getCredential() {
    String token = System.getenv("CB_ACCESS_TOKEN");
    final Map<String, List<String>> headers =
        Collections.singletonMap(
            "Authorization",
            Collections.singletonList(
                "Bearer " + (token == null || token.isEmpty() ? "cloudburrow-test" : token)));
    return new Credentials() {
      @Override
      public String getAuthenticationType() {
        return "OAuth2";
      }

      @Override
      public Map<String, List<String>> getRequestMetadata(URI uri) {
        return headers;
      }

      @Override
      public boolean hasRequestMetadata() {
        return true;
      }

      @Override
      public boolean hasRequestMetadataOnly() {
        return true;
      }

      @Override
      public void refresh() {}
    };
  }
}
