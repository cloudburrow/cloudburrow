# The Python client of TestServedClientsSendNothingOffTheMachine
# (test/compat/egresscontainer_test.go): Cloud KMS over REST and gRPC,
# pointed at the instance, with the ADC fixture GOOGLE_APPLICATION_CREDENTIALS
# names as its only credential (google.auth.default, as a client finds it).
# It prints "OK python <transport>" or "FAIL python <transport>: <error>",
# and runs only inside the no-egress container.
import os
import sys
import time

addr, parent = os.environ.get("EGRESS_KMS", ""), os.environ.get("EGRESS_PARENT", "")
# Never fall back to Google's endpoint.
if not addr or not parent or not os.environ.get("GOOGLE_APPLICATION_CREDENTIALS"):
    print("FAIL python: EGRESS_KMS, EGRESS_PARENT and GOOGLE_APPLICATION_CREDENTIALS must be set")
    sys.exit(1)

import google.auth
import google.auth.transport.grpc
import google.auth.transport.requests
import grpc
from google.cloud import kms_v1
from google.cloud.kms_v1.services.key_management_service.transports import KeyManagementServiceGrpcTransport


def run(transport, make):
    try:
        client = make()
        list(client.list_key_rings(request={"parent": parent}))
        print("OK python", transport, flush=True)
    except Exception as e:  # noqa: BLE001 - the probe reports every failure
        print("FAIL python", transport + ":", repr(e)[:400], flush=True)


def rest():
    return kms_v1.KeyManagementServiceClient(
        transport="rest", client_options={"api_endpoint": "http://" + addr})


def grpc_client():
    creds, _ = google.auth.default(scopes=["https://www.googleapis.com/auth/cloud-platform"])
    # Local channel credentials carry call credentials over plaintext
    # loopback, so the fixture's token is sent as it is to Google.
    plugin = google.auth.transport.grpc.AuthMetadataPlugin(creds, google.auth.transport.requests.Request())
    channel = grpc.secure_channel(addr, grpc.composite_channel_credentials(
        grpc.local_channel_credentials(), grpc.metadata_call_credentials(plugin)))
    return kms_v1.KeyManagementServiceClient(transport=KeyManagementServiceGrpcTransport(channel=channel))


run("rest", rest)
run("grpc", grpc_client)
# Room for the background lookups google-auth starts (#1981).
time.sleep(5)
