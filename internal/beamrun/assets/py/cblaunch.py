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


def rewrite_environments(p, harness):
    """Points the pipeline's Python environment at the worker pool."""
    python_env = None
    for eid, env in p.components.environments.items():
        if env.urn == common_urns.environments.DOCKER.urn:
            dp = beam_runner_api_pb2.DockerPayload()
            dp.ParseFromString(env.payload)
            if "python" not in dp.container_image:
                raise RuntimeError("The pipeline's environment %s runs the SDK container %s: only the Python SDK's harness is served by "
                                   "CloudBurrow's local runner." % (eid, dp.container_image))
            env.urn = common_urns.environments.EXTERNAL.urn
            env.payload = beam_runner_api_pb2.ExternalPayload(
                endpoint=endpoints_pb2.ApiServiceDescriptor(url=harness)).SerializeToString()
            python_env = python_env or eid
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
    if python_env is None:
        raise RuntimeError("The pipeline has no Python SDK environment.")
    return python_env


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
    o["job_name"] = args.job_name
    o["flink_master"] = "[local]"
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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--job-endpoint", required=True)
    ap.add_argument("--artifact-endpoint", required=True)
    ap.add_argument("--harness", default="localhost:50000")
    ap.add_argument("--pipeline-url", required=True)
    ap.add_argument("--options", required=True)
    ap.add_argument("--job-name", required=True)
    ap.add_argument("--parallelism", type=int, default=1)
    ap.add_argument("--streaming", action="store_true")
    ap.add_argument("--cancel-file", required=True)
    ap.add_argument("--metrics-dir", default="/cb/metrics")
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
        env = rewrite_environments(p, args.harness)
        cbio.rewrite(p, env)
    except Exception as e:
        emit(event="done", state="FAILED", message="Workflow failed. Causes: %s" % e)
        return
    emit(event="graph", transforms=graph(p))
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
    while t.is_alive():
        t.join(1)
        if not cancelled and os.path.exists(args.cancel_file):
            cancelled = True
            try:
                stub.Cancel(beam_job_api_pb2.CancelJobRequest(job_id=job_id))
            except Exception:
                pass
        if time.time() - last > 5:
            last = time.time()
            emit(event="metrics", metrics=harness_metrics(args.metrics_dir, producers))
    emit(event="metrics", metrics=harness_metrics(args.metrics_dir, producers))
    state = final.get("state") or STATE.Enum.Name(stub.GetState(beam_job_api_pb2.GetJobStateRequest(job_id=job_id)).state)
    emit(event="done", state=state, message=final.get("message", ""))


if __name__ == "__main__":
    main()
