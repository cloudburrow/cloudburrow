// The Java SDK harness's credentials on a Dataflow job's network
// (internal/beamrun, #2215): a fixed access token of the instance
// (CB_ACCESS_TOKEN), as no metadata server is reachable and the instance
// takes every caller to be its account. Compiled in the job's Java worker
// pool against the job's staged jars, and named by its pipeline options'
// credentialFactoryClass.
//
// The token is a plain Credentials, not GoogleCredentials: gRPC's
// GoogleAuthLibraryCallCredentials demands a TLS channel for
// GoogleCredentials only, and the emulators' channels (Bigtable's through
// BIGTABLE_EMULATOR_HOST, #2215 round 3) are plaintext.
import com.google.auth.Credentials;
import java.net.URI;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import org.apache.beam.sdk.extensions.gcp.auth.CredentialFactory;
import org.apache.beam.sdk.options.PipelineOptions;

public class CbCredentialFactory implements CredentialFactory {
  public static CbCredentialFactory fromOptions(PipelineOptions options) {
    return new CbCredentialFactory();
  }

  @Override
  public Credentials getCredential() {
    String token = System.getenv("CB_ACCESS_TOKEN");
    if (token == null || token.isEmpty()) {
      token = "cloudburrow-dataflow";
    }
    return new Token(token);
  }

  /** A bearer token sent as the Authorization header. */
  public static final class Token extends Credentials {
    private final Map<String, List<String>> headers;

    Token(String token) {
      headers = Collections.singletonMap("Authorization", Collections.singletonList("Bearer " + token));
    }

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
  }
}
