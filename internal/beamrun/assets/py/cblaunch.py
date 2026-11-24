"""Runs one Dataflow job's staged pipeline on Flink's Beam job server.

internal/beamrun runs it in the job's launcher container: it reads the
pipeline the SDK's DataflowRunner staged (pipeline.pb) from the
instance's Cloud Storage, points its SDK environment at the job's SDK
harness (an EXTERNAL worker pool beside the job server), replaces the
Pub/Sub transforms Dataflow's service implements itself (cbio), and
submits it through the Beam Job API with the job's pipeline options,
staging its files (the SDK's staged packages, from Cloud Storage) to the
job server. It prints one JSON line per event on stdout: graph, state,
metrics, message and, last, done.
"""
import argparse
import json
import os
import re
import sys
import threading
import time

import grpc
from apache_beam.io.filesystems import FileSystems
from apache_beam.portability import common_urns
from apache_beam.portability.api import beam_artifact_api_pb2_grpc
from apache_beam.portability.api import beam_job_api_pb2
from apache_beam.portability.api import beam_job_api_pb2_grpc
from apache_beam.portability.api import beam_runner_api_pb2
from apache_beam.portability.api import endpoints_pb2
from apache_beam.runners.job import utils as job_utils
from apache_beam.runners.portability import artifact_service

import cbio

STATE = beam_job_api_pb2.JobState


def emit(**kw):
    sys.stdout.write(json.dumps(kw) + "\n")
    sys.stdout.flush()


KIND = {
    common_urns.primitives.PAR_DO.urn: "ParDo",
    common_urns.primitives.GROUP_BY_KEY.urn: "GroupByKey",
    common_urns.primitives.FLATTEN.urn: "Flatten",
    common_urns.primitives.IMPULSE.urn: "Impulse",
    common_urns.composites.COMBINE_PER_KEY.urn: "Combine",
    common_urns.deprecated_primitives.READ.urn: "Read",
}


def graph(p):
    """The pipeline's user transforms: the top level and their parts, in order."""
    ts = p.components.transforms
    out = []

    def walk(tid, parent, depth):
        t = ts[tid]
        kind = KIND.get(t.spec.urn, "Composite" if t.subtransforms else "ParDo")
        if "/Write" in t.unique_name and t.subtransforms and depth == 0:
            kind = "Write"
        if depth == 0 and ("Read" in t.unique_name.split("/")[0]):
            kind = "Read"
        out.append({"id": tid, "name": t.unique_name, "kind": kind, "parent": parent,
                    "inputs": list(t.inputs.values()), "outputs": list(t.outputs.values()),
                    "stage": t.unique_name.split("/")[0]})
        if depth < 1:
            for s in t.subtransforms:
                walk(s, tid, depth + 1)
    roots = list(p.root_transform_ids)
    # The SDK wraps the user's transforms in one nameless root.
    while len(roots) == 1 and not ts[roots[0]].unique_name.strip() or (len(roots) == 1 and ts[roots[0]].unique_name.startswith("ref_")):
        roots = list(ts[roots[0]].subtransforms)
    for r in roots:
        walk(r, "", 0)
    return out


def _java_major(image):
    m = re.search(r"beam_java(\d+)_sdk", image)
    return m.group(1) if m else ""


def _fetch(url, dest):
    """Copies url (gs://, through Beam's file systems: the instance's Cloud
    Storage) to dest."""
    with FileSystems.open(url, compression_type="uncompressed") as src, open(dest, "wb") as out:
        while True:
            b = src.read(1 << 20)
            if not b:
                break
            out.write(b)


def _staged_name(dep, fallback):
    if dep.role_urn == common_urns.artifact_roles.STAGING_TO.urn:
        rp = beam_runner_api_pb2.ArtifactStagingToRolePayload()
        rp.ParseFromString(dep.role_payload)
        if rp.staged_name:
            return os.path.basename(rp.staged_name)
    return os.path.basename(fallback)


def stage_java(env, java_dir):
    """The Java environment's staged jars, copied to java_dir/jars: the
    job's Java worker pool (Beam's ExternalWorkerService, which runs each
    worker in its own JVM's threads, so takes the pipeline's classes on its
    class path) starts on them."""
    jars = os.path.join(java_dir, "jars")
    os.makedirs(jars, exist_ok=True)
    n = 0
    for dep in env.dependencies:
        if dep.type_urn == common_urns.artifact_types.URL.urn:
            pl = beam_runner_api_pb2.ArtifactUrlPayload()
            pl.ParseFromString(dep.type_payload)
            src = pl.url
        elif dep.type_urn == common_urns.artifact_types.FILE.urn:
            pl = beam_runner_api_pb2.ArtifactFilePayload()
            pl.ParseFromString(dep.type_payload)
            src = pl.path
        else:
            continue
        name = _staged_name(dep, src)
        if not name.endswith(".jar"):
            continue
        n += 1
        _fetch(src, os.path.join(jars, "%04d-%s" % (n, name)))
    del env.dependencies[:]
    return n


def rewrite_environments(p, harness, java_harness=None, java_dir=None):
    """Points the pipeline's SDK environments at the job's worker pools:
    Python's at the Python SDK's, Java's at the Java SDK's. A Java pipeline
    is given a Python environment too, for the service-side transforms the
    launcher runs in Python (cbio). Answers (the Python environment, the
    Java SDK's major Java version or "")."""
    python_env, java_version = None, ""
    for eid, env in p.components.environments.items():
        if env.urn == common_urns.environments.DOCKER.urn:
            dp = beam_runner_api_pb2.DockerPayload()
            dp.ParseFromString(env.payload)
            if "python" in dp.container_image:
                env.urn = common_urns.environments.EXTERNAL.urn
                env.payload = beam_runner_api_pb2.ExternalPayload(
                    endpoint=endpoints_pb2.ApiServiceDescriptor(url=harness)).SerializeToString()
                python_env = python_env or eid
            elif "java" in dp.container_image and java_harness and java_dir:
                v = _java_major(dp.container_image) or "17"
                if java_version and v != java_version:
                    raise RuntimeError("The pipeline's Java environments run Java %s and %s: one Java version per job is served." % (java_version, v))
                java_version = v
                stage_java(env, java_dir)
                env.urn = common_urns.environments.EXTERNAL.urn
                env.payload = beam_runner_api_pb2.ExternalPayload(
                    endpoint=endpoints_pb2.ApiServiceDescriptor(url=java_harness)).SerializeToString()
                continue
            else:
                raise RuntimeError("The pipeline's environment %s runs the SDK container %s: CloudBurrow's local runner serves the "
                                   "Python and Java SDKs' harnesses." % (eid, dp.container_image))
        # The SDK's staged files are URL artifacts in Cloud Storage
        # (gs://), which the artifact service reads as files: through
        # Beam's file systems, from the instance's Cloud Storage.
        for dep in env.dependencies:
            if dep.type_urn == common_urns.artifact_types.URL.urn:
                up = beam_runner_api_pb2.ArtifactUrlPayload()
                up.ParseFromString(dep.type_payload)
                if up.url.startswith("gs://"):
                    dep.type_urn = common_urns.artifact_types.FILE.urn
                    dep.type_payload = beam_runner_api_pb2.ArtifactFilePayload(path=up.url, sha256=up.sha256).SerializeToString()
    if python_env is None and java_version:
        python_env = "cb_python"
        env = p.components.environments[python_env]
        env.urn = common_urns.environments.EXTERNAL.urn
        env.payload = beam_runner_api_pb2.ExternalPayload(
            endpoint=endpoints_pb2.ApiServiceDescriptor(url=harness)).SerializeToString()
        from apache_beam.transforms import environments as _envs
        env.capabilities.extend(_envs.python_sdk_capabilities())
    if python_env is None:
        raise RuntimeError("The pipeline has no Python or Java SDK environment.")
    return python_env, java_version


def java_options(opts, args):
    """The Java worker pool's pipeline options (PIPELINE_OPTIONS): the
    job's, with the pool's address and the instance's credentials."""
    o = {k: v for k, v in opts.items() if k not in (
        "runner", "dataflowEndpoint", "apiRootUrl", "pipelineUrl", "templateLocation", "sdkContainerImage",
        "workerHarnessContainerImage", "dataflowJobFile", "update", "transformNameMapping", "filesToStage",
        "jobName", "credentialFactoryClass", "experiments")}
    # The Java SDK's own pool, behind cbfnproxy's (on --java-harness),
    # which reads its workers' metrics.
    o["environmentOptions"] = ["external_service_address=" + args.java_sdk_pool]
    o["credentialFactoryClass"] = "CbCredentialFactory"
    o["gcsEndpoint"] = os.environ.get("STORAGE_EMULATOR_HOST", "").rstrip("/") + "/storage/v1/"
    # BigQueryIO's clients (BigQueryOptions.bigQueryEndpoint, the API's
    # root URL): the instance's BigQuery through the relay.
    if os.environ.get("BIGQUERY_EMULATOR_HOST"):
        o["bigQueryEndpoint"] = os.environ["BIGQUERY_EMULATOR_HOST"].rstrip("/") + "/"
    o["parallelism"] = args.parallelism
    o["jobName"] = args.job_name
    if args.streaming:
        o["streaming"] = True
    return {"options": o}


def options_struct(opts, args):
    o = dict(opts)
    for k in ("runner", "dataflow_endpoint", "pipelineUrl", "pipelineProtoHash", "template_location",
              "sdk_container_image", "worker_harness_container_image", "dataflow_job_file", "update",
              "transform_name_mapping"):
        o.pop(k, None)
    o["parallelism"] = args.parallelism
    o["max_parallelism"] = max(args.parallelism, 1)
    if args.streaming:
        o["streaming"] = True
        o["checkpointing_interval"] = 10000
        # The sources keep running until the job is drained (a source that
        # shut down when idle would leave its reads' operators finishing,
        # which no checkpoint or savepoint completes through): a drain
        # stops the Flink job with --drain (drain()).
    o["job_name"] = args.job_name
    o["flink_master"] = "[local]"
    if args.savepoint_path:
        # Created from a snapshot: the Flink job starts from its savepoint.
        o["savepoint_path"] = args.savepoint_path
    return job_utils.dict_to_struct({"beam:option:%s:v1" % k: v for k, v in o.items() if v is not None})


def harness_metrics(directory, producers):
    """The SDK workers' metrics (sitecustomize's files), summed; each
    ElementCount's step is the transform that produced its PCollection."""
    import glob
    totals = {}
    for path in glob.glob(os.path.join(directory, "*.json")):
        try:
            with open(path) as f:
                rows = json.load(f)
        except Exception:
            continue
        for r in rows:
            key = (r["name"], r["origin"], r.get("step", ""), r.get("namespace", ""), r.get("pcollection", ""), r["kind"])
            if r["kind"] == "Distribution":
                p = totals.get(key)
                totals[key] = r if p is None else dict(p, count=p["count"] + r["count"], total=p["total"] + r["total"],
                                                         min=min(p["min"], r["min"]), max=max(p["max"], r["max"]))
            else:
                p = totals.get(key)
                totals[key] = r if p is None else dict(p, value=p["value"] + r["value"])
    out = list(totals.values())
    for r in out:
        if r["name"] == "ElementCount" and not r.get("step"):
            r["step"] = producers.get(r.get("pcollection", ""), "")
    return out


def producers_of(p):
    """Each PCollection's producing leaf transform's user name."""
    out = {}
    for t in p.components.transforms.values():
        if t.subtransforms:
            continue
        for pc in t.outputs.values():
            out[pc] = t.unique_name
    return out


def opaque_custom_windows(p):
    """Makes each windowed value coder whose window coder is the SDK's own
    (pickled) one opaque to the runner, as a whole.

    Python's Reshuffle carries a custom window through its shuffle in a
    windowed value of the window's own coder (streaming WriteToBigQuery's
    CommitInsertIds, under FixedWindows). Flink's runner knows the windowed
    value coder but not the pickled window one, so it would decode the
    windows as bytes and fail to use them as windows; made one pickled
    coder, it length-prefixes the value and leaves it to Python."""
    from google.protobuf import wrappers_pb2

    from apache_beam.coders.coders import serialize_coder
    from apache_beam.portability import python_urns
    from apache_beam.runners import pipeline_context

    cs = p.components.coders
    known_windows = {common_urns.coders.GLOBAL_WINDOW.urn, common_urns.coders.INTERVAL_WINDOW.urn}
    targets = [cid for cid, c in cs.items()
               if c.spec.urn in (common_urns.coders.WINDOWED_VALUE.urn, common_urns.coders.PARAM_WINDOWED_VALUE.urn)
               and len(c.component_coder_ids) == 2 and cs[c.component_coder_ids[1]].spec.urn not in known_windows]
    if not targets:
        return
    ctx = pipeline_context.PipelineContext(p.components)
    for cid in targets:
        obj = ctx.coders.get_by_id(cid)
        c = cs[cid]
        c.spec.urn = python_urns.PICKLED_CODER
        c.spec.payload = wrappers_pb2.BytesValue(value=serialize_coder(obj)).SerializeToString()
        del c.component_coder_ids[:]


def _rest(base, method, path, body=None):
    import urllib.request
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read() or b"{}")


def savepoint(rest, snapshot_id, out):
    """Takes a savepoint of the job server's running Flink job (the
    job's), into /cb-snapshots/<snapshot_id>, through Flink's REST API,
    and writes its location (or error) to out."""
    res = {}
    try:
        base = "http://" + rest
        running = [j["id"] for j in _rest(base, "GET", "/jobs").get("jobs", []) if j.get("status") == "RUNNING"]
        if not running:
            raise RuntimeError("the job's Flink job is not running")
        fid = running[0]
        trig = _rest(base, "POST", "/jobs/%s/savepoints" % fid,
                     {"target-directory": "file:///cb-snapshots/%s" % snapshot_id, "cancel-job": False})
        rid = trig["request-id"]
        deadline = time.time() + 540
        while True:
            st = _rest(base, "GET", "/jobs/%s/savepoints/%s" % (fid, rid))
            if st.get("status", {}).get("id") == "COMPLETED":
                op = st.get("operation", {})
                if op.get("location"):
                    res["location"] = op["location"]
                else:
                    res["error"] = str(op.get("failure-cause", {}).get("stack-trace", "no location"))[:2000]
                break
            if time.time() > deadline:
                res["error"] = "the savepoint did not complete"
                break
            time.sleep(0.5)
    except Exception as e:
        res["error"] = str(e)
    tmp = out + ".tmp"
    with open(tmp, "w") as f:
        json.dump(res, f)
    os.rename(tmp, out)


def drain(rest):
    """Drains the job server's Flink job: Flink's stop with --drain, which
    takes a savepoint, stops the sources and advances the watermark to the
    end of time, so every window fires before the job finishes (the reads,
    which watch the drain file, stop with it). A stop is retried until it
    completes: one asked for before every task runs (a drain just after
    the job started, or a launched template's) fails its savepoint."""
    base = "http://" + rest
    deadline = time.time() + 900
    last_error = ""
    while time.time() < deadline:
        try:
            jobs = _rest(base, "GET", "/jobs").get("jobs", [])
            live = [j for j in jobs if j.get("status") in ("CREATED", "INITIALIZING", "RUNNING", "RESTARTING")]
            if jobs and not live:
                return
            for j in live:
                if j.get("status") != "RUNNING":
                    continue
                trig = _rest(base, "POST", "/jobs/%s/stop" % j["id"], {"drain": True, "targetDirectory": "file:///tmp/cb-drain"})
                rid = trig.get("request-id")
                while rid and time.time() < deadline:
                    st = _rest(base, "GET", "/jobs/%s/savepoints/%s" % (j["id"], rid))
                    if st.get("status", {}).get("id") == "COMPLETED":
                        cause = st.get("operation", {}).get("failure-cause")
                        if cause:
                            last_error = str(cause.get("class", "")) + ": " + str(cause.get("stack-trace", ""))[:300]
                        break
                    time.sleep(0.5)
        except Exception as e:
            last_error = str(e)
        time.sleep(2)
    emit(event="message", importance="JOB_MESSAGE_WARNING", text="Draining the Flink job: %s" % (last_error or "it did not stop"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--job-endpoint", required=True)
    ap.add_argument("--artifact-endpoint", required=True)
    ap.add_argument("--harness", default="localhost:50000")
    ap.add_argument("--java-harness", default="localhost:50001")
    ap.add_argument("--java-sdk-pool", default="localhost:50002")
    ap.add_argument("--java-dir", default="/cb/java")
    ap.add_argument("--pipeline-url", required=True)
    ap.add_argument("--options", required=True)
    ap.add_argument("--job-name", required=True)
    ap.add_argument("--parallelism", type=int, default=1)
    ap.add_argument("--streaming", action="store_true")
    ap.add_argument("--cancel-file", required=True)
    ap.add_argument("--metrics-dir", default="/cb/metrics")
    ap.add_argument("--flink-rest", default="")
    ap.add_argument("--savepoint-path", default="")
    args = ap.parse_args()

    with open(args.options) as f:
        opts = json.load(f)
    try:
        with FileSystems.open(args.pipeline_url) as f:
            data = f.read()
    except Exception as e:
        emit(event="done", state="FAILED", message="Workflow failed. Causes: Unable to read the staged pipeline %s: %s" % (args.pipeline_url, e))
        return
    p = beam_runner_api_pb2.Pipeline()
    p.ParseFromString(data)
    try:
        env, java_version = rewrite_environments(p, args.harness, args.java_harness, args.java_dir)
        cbio.rewrite(p, env)
        opaque_custom_windows(p)
    except Exception as e:
        emit(event="done", state="FAILED", message="Workflow failed. Causes: %s" % e)
        return
    emit(event="graph", transforms=graph(p))
    if java_version:
        # internal/beamrun starts the job's Java worker pool, in the job
        # server's network namespace, once its jars and options are here.
        with open(os.path.join(args.java_dir, "options.json"), "w") as f:
            json.dump(java_options(opts, args), f)
        with open(os.path.join(args.java_dir, "version"), "w") as f:
            f.write(java_version)
        open(os.path.join(args.java_dir, "ready"), "w").close()
        pool = args.job_endpoint.rsplit(":", 1)[0] + ":" + args.java_harness.rsplit(":", 1)[1]
        try:
            grpc.channel_ready_future(grpc.insecure_channel(pool)).result(timeout=300)
        except Exception:
            emit(event="done", state="FAILED", message="Workflow failed. Causes: The job's Java SDK harness did not start.")
            return
    producers = producers_of(p)

    deadline = time.time() + 120
    while True:
        try:
            ch = grpc.insecure_channel(args.job_endpoint)
            grpc.channel_ready_future(ch).result(timeout=5)
            break
        except Exception:
            if time.time() > deadline:
                emit(event="done", state="FAILED", message="Workflow failed. Causes: The Flink job server did not start.")
                return
    stub = beam_job_api_pb2_grpc.JobServiceStub(ch)
    prep = stub.Prepare(beam_job_api_pb2.PrepareJobRequest(job_name=args.job_name, pipeline=p,
                                                           pipeline_options=options_struct(opts, args)))
    if prep.artifact_staging_endpoint.url:
        staging = beam_artifact_api_pb2_grpc.ArtifactStagingServiceStub(grpc.insecure_channel(args.artifact_endpoint))
        artifact_service.offer_artifacts(staging, artifact_service.ArtifactRetrievalService(
            artifact_service.BeamFilesystemHandler(None).file_reader), prep.staging_session_token)
    run = stub.Run(beam_job_api_pb2.RunJobRequest(preparation_id=prep.preparation_id))
    job_id = run.job_id
    emit(event="state", state="STARTING")

    final = {}

    def messages():
        try:
            for m in stub.GetMessageStream(beam_job_api_pb2.JobMessagesRequest(job_id=job_id)):
                if m.HasField("state_response"):
                    st = STATE.Enum.Name(m.state_response.state)
                    emit(event="state", state=st)
                    if st in ("DONE", "FAILED", "CANCELLED"):
                        final["state"] = st
                        return
                else:
                    r = m.message_response
                    emit(event="message", importance=beam_job_api_pb2.JobMessage.MessageImportance.Name(r.importance),
                         text=r.message_text)
                    if r.importance >= beam_job_api_pb2.JobMessage.JOB_MESSAGE_ERROR:
                        final["message"] = r.message_text
        except Exception as e:
            final.setdefault("message", str(e))
    t = threading.Thread(target=messages, daemon=True)
    t.start()
    cancelled = False
    last = 0
    taken = set()
    drained = False
    while t.is_alive():
        t.join(1)
        if not cancelled and os.path.exists(args.cancel_file):
            cancelled = True
            try:
                stub.Cancel(beam_job_api_pb2.CancelJobRequest(job_id=job_id))
            except Exception:
                pass
        if args.flink_rest and args.streaming and not drained and os.path.exists(os.environ.get("CB_DRAIN", "/cb/ctl/drain")):
            drained = True
            threading.Thread(target=drain, args=(args.flink_rest,), daemon=True).start()
        if args.flink_rest:
            ctl = os.path.dirname(args.cancel_file)
            for name in os.listdir(ctl):
                if name.startswith("savepoint-") and "." not in name and name not in taken:
                    taken.add(name)
                    threading.Thread(target=savepoint, args=(args.flink_rest, name[len("savepoint-"):],
                                                             os.path.join(ctl, name + ".json")), daemon=True).start()
        if time.time() - last > 5:
            last = time.time()
            emit(event="metrics", metrics=harness_metrics(args.metrics_dir, producers))
    emit(event="metrics", metrics=harness_metrics(args.metrics_dir, producers))
    state = final.get("state") or STATE.Enum.Name(stub.GetState(beam_job_api_pb2.GetJobStateRequest(job_id=job_id)).state)
    emit(event="done", state=state, message=final.get("message", ""))


if __name__ == "__main__":
    main()
