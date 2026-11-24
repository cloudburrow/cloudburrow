# The Auth Proxy's client in TestAlloyDBConnectorsSendRealSQL: pg8000 at the
# proxy's local listener writes and reads a row. Prints "OK proxy <rows>" or
# "FAIL proxy: <error>".
import os

import pg8000.dbapi

try:
    conn = pg8000.dbapi.connect(host="127.0.0.1", port=5000, user="postgres", password=os.environ["ADB_PASSWORD"], database="postgres")
    cur = conn.cursor()
    cur.execute("CREATE TABLE IF NOT EXISTS connector_probe (client text)")
    cur.execute("INSERT INTO connector_probe VALUES ('proxy')")
    conn.commit()
    cur.execute("SELECT count(*) FROM connector_probe WHERE client = 'proxy'")
    print("OK proxy", cur.fetchone()[0], flush=True)
    conn.close()
except Exception as e:  # noqa: BLE001 - the probe reports every failure
    print("FAIL proxy:", repr(e), flush=True)
