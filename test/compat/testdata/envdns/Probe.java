// The Java client of TestEnvResolvesNoGoogleName (test/compat/envdns_test.go):
// google-auth-library-oauth2-http under `cloudburrow env`. Application
// Default Credentials (the fixture GOOGLE_APPLICATION_CREDENTIALS names,
// refreshed at its token_uri) and Compute Engine credentials (the metadata
// server GCE_METADATA_HOST names). It prints "OK java <step>" or
// "FAIL java <step>: <error>", and runs only inside the no-egress container.
import com.google.auth.oauth2.ComputeEngineCredentials;
import com.google.auth.oauth2.GoogleCredentials;
import java.util.List;

public class Probe {
  interface Step {
    void run() throws Exception;
  }

  static void step(String name, Step s) {
    try {
      s.run();
      System.out.println("OK java " + name);
    } catch (Exception e) {
      String m = String.valueOf(e);
      System.out.println("FAIL java " + name + ": " + m.substring(0, Math.min(400, m.length())));
    }
  }

  public static void main(String[] args) {
    // Never fall back to Google's defaults.
    if (System.getenv("GOOGLE_APPLICATION_CREDENTIALS") == null || System.getenv("GCE_METADATA_HOST") == null) {
      System.out.println("FAIL java: GOOGLE_APPLICATION_CREDENTIALS and GCE_METADATA_HOST must be set");
      System.exit(1);
    }
    List<String> scopes = List.of("https://www.googleapis.com/auth/cloud-platform");
    step("adc", () -> GoogleCredentials.getApplicationDefault().createScoped(scopes).refresh());
    step("metadata", () -> {
      ComputeEngineCredentials c = ComputeEngineCredentials.create();
      c.refresh();
      if (c.getAccount() == null || c.getAccount().isEmpty()) {
        throw new IllegalStateException("no account from the metadata server");
      }
    });
  }
}
