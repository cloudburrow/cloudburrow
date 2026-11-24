package dev.cloudburrow.dataflow;

import com.google.auth.Credentials;
import com.google.auth.oauth2.AccessToken;
import com.google.auth.oauth2.GoogleCredentials;
import org.apache.beam.sdk.extensions.gcp.auth.CredentialFactory;
import org.apache.beam.sdk.options.PipelineOptions;

/** A fixed access token of the instance (CB_ACCESS_TOKEN): no metadata server is reachable. */
public class TokenCredentialFactory implements CredentialFactory {
  public static TokenCredentialFactory fromOptions(PipelineOptions options) {
    return new TokenCredentialFactory();
  }

  @Override
  public Credentials getCredential() {
    String token = System.getenv("CB_ACCESS_TOKEN");
    return GoogleCredentials.create(new AccessToken(token == null || token.isEmpty() ? "cloudburrow-test" : token, null));
  }
}
