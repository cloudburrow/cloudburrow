// The Java client of TestAlloyDBConnectorsSendRealSQL: Google's AlloyDB
// Java connector (com.google.cloud:alloydb-jdbc-connector) and the
// PostgreSQL JDBC driver, pointed at the instance's admin API over TLS,
// write and read a row. Prints "OK java <rows>" or "FAIL java: <error>";
// runs only in the no-egress container, with java's source launcher.
import com.google.auth.oauth2.AccessToken;
import com.google.auth.oauth2.GoogleCredentials;
import com.google.cloud.alloydb.ConnectorConfig;
import com.google.cloud.alloydb.ConnectorRegistry;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.ResultSet;
import java.sql.Statement;
import java.util.Date;
import java.util.Properties;

public class Probe {
  public static void main(String[] args) {
    String inst = System.getenv("ADB_INSTANCE"), api = System.getenv("ADB_TLS"), tok = System.getenv("ADB_TOKEN");
    // Never fall back to Google's endpoint.
    if (inst == null || api == null || tok == null || inst.isEmpty() || api.isEmpty() || tok.isEmpty()) {
      System.out.println("FAIL java: ADB_INSTANCE, ADB_TLS and ADB_TOKEN must be set");
      System.exit(1);
    }
    try {
      ConnectorRegistry.register("cloudburrow", new ConnectorConfig.Builder()
          .withAdminServiceEndpoint(api)
          .withGoogleCredentials(GoogleCredentials.create(new AccessToken(tok, new Date(System.currentTimeMillis() + 3600_000L))))
          .build());
      Properties p = new Properties();
      p.setProperty("user", "postgres");
      p.setProperty("password", System.getenv("ADB_PASSWORD"));
      p.setProperty("socketFactory", "com.google.cloud.alloydb.SocketFactory");
      p.setProperty("alloydbInstanceName", inst);
      p.setProperty("alloydbNamedConnector", "cloudburrow");
      p.setProperty("sslmode", "disable");
      try (Connection c = DriverManager.getConnection("jdbc:postgresql:///postgres", p); Statement s = c.createStatement()) {
        s.execute("CREATE TABLE IF NOT EXISTS connector_probe (client text)");
        s.execute("INSERT INTO connector_probe VALUES ('java')");
        try (ResultSet r = s.executeQuery("SELECT count(*) FROM connector_probe WHERE client = 'java'")) {
          r.next();
          System.out.println("OK java " + r.getLong(1));
        }
      }
    } catch (Throwable e) {
      System.out.println("FAIL java: " + e);
      for (Throwable c = e.getCause(); c != null; c = c.getCause()) {
        System.out.println("  caused by " + c);
      }
    } finally {
      ConnectorRegistry.shutdown();
    }
    System.exit(0);
  }
}
