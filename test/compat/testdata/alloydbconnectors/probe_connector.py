# The Python client of TestAlloyDBConnectorsSendRealSQL: Google's AlloyDB
# Python connector (google-cloud-alloydb-connector, pg8000), pointed at the
# instance's admin API over TLS, writes and reads a row. Prints "OK python
# <rows>" or "FAIL python: <error>"; runs only in the no-egress container.
import os
import sys
import traceback

inst, api, tok = os.environ.get("ADB_INSTANCE", ""), os.environ.get("ADB_TLS", ""), os.environ.get("ADB_TOKEN", "")
# Never fall back to Google's endpoint.
if not inst or not api or not tok:
    print("FAIL python: ADB_INSTANCE, ADB_TLS and ADB_TOKEN must be set")
    sys.exit(1)

import datetime

from google.cloud.alloydbconnector import Connector
from google.oauth2.credentials import Credentials

try:
    # The connector logs the token's expiry, so it needs one (google-auth's
    # naive UTC).
    expiry = datetime.datetime.now(datetime.timezone.utc).replace(tzinfo=None) + datetime.timedelta(hours=1)
    with Connector(credentials=Credentials(token=tok, expiry=expiry), alloydb_api_endpoint=api) as connector:
        conn = connector.connect(inst, "pg8000", user="postgres", password=os.environ["ADB_PASSWORD"], db="postgres")
        cur = conn.cursor()
        cur.execute("CREATE TABLE IF NOT EXISTS connector_probe (client text)")
        cur.execute("INSERT INTO connector_probe VALUES ('python')")
        conn.commit()
        cur.execute("SELECT count(*) FROM connector_probe WHERE client = 'python'")
        print("OK python", cur.fetchone()[0], flush=True)
        conn.close()
except Exception as e:  # noqa: BLE001 - the probe reports every failure
    print("FAIL python:", repr(e), flush=True)
    traceback.print_exc(file=sys.stdout)
