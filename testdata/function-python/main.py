"""The Python fixture used to verify Google Functions Framework and
Buildpacks support (#1303).

It is an ordinary Functions Framework function: no CloudBurrow-specific code,
because the point is that a normal function runs unchanged. It answers as
testdata/function does.
"""

import json

import functions_framework


@functions_framework.http
def hello(request):
    """Answers an HTTP request, echoing a name when given one."""
    body = request.get_json(silent=True) or {}
    name = body.get("name") or "world"
    return {"greeting": "hello " + name}


@functions_framework.cloud_event
def event(cloud_event):
    """Receives a CloudEvent in the Google schema and logs it."""
    data = json.dumps(cloud_event.data, separators=(",", ":"), sort_keys=True)
    print(
        f"received CloudEvent type={cloud_event['type']} "
        f"subject={cloud_event['subject']} data={data}",
        flush=True,
    )
