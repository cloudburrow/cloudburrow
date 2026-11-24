"""The metrics of a Dataflow job's Java workers (internal/beamrun, #2215).

Flink's job server answers no metrics, so the job's metrics are what its
SDK workers report to the runner at the end of each bundle (each
ProcessBundleResponse's MonitoringInfos: the PCollections' element counts,
the pipeline's own counters and distributions). The Python workers keep
theirs in sitecustomize; the Java workers' are read here, on the Fn API:

this process is the job's Java worker pool as the runner sees it (Beam's
BeamFnExternalWorkerPool, on --listen). Each worker it is asked to start
it starts on the Java SDK's own pool (--target), its control endpoint
pointed at a proxy of the runner's: the proxy passes every instruction and
response through unchanged, keeps the totals of the bundles' metrics, and
writes them, as sitecustomize does, to a file of --metrics, which the
launcher sums. A bundle's metrics that are reported by short ID are
resolved by asking the worker for their metadata (an instruction of the
proxy's own, whose response the runner never sees).

The Java SDK's pool takes a worker's control endpoint from the runner's
ProvisionInfo whenever the request names a provision endpoint, not from
the request (ExternalWorkerService.startWorker: "if
(provisionInfo.hasControlEndpoint()) controlEndpoint = ..."), so this
proxy answers the worker's provisioning too: the runner's ProvisionInfo,
its control endpoint made the proxy's.
"""
import argparse
import itertools
import json
import os
import queue
import threading
from concurrent import futures

import grpc
from apache_beam.metrics import monitoring_infos as mi
from apache_beam.portability.api import beam_fn_api_pb2
from apache_beam.portability.api import beam_fn_api_pb2_grpc
from apache_beam.portability.api import beam_provision_api_pb2_grpc
from apache_beam.portability.api import metrics_pb2

OPTS = [("grpc.max_receive_message_length", -1), ("grpc.max_send_message_length", -1)]
_ids = itertools.count(1)


class Totals(object):
    def __init__(self, path):
        self.path = path
        self.lock = threading.Lock()
        self.totals = {}

    def add(self, infos):
        with self.lock:
            for info in infos:
                labels = dict(info.labels)
                try:
                    if info.urn == mi.ELEMENT_COUNT_URN:
                        key = ("ElementCount", "dataflow/v1b3", "", labels.get(mi.PCOLLECTION_LABEL, ""), "Sum")
                        self.totals[key] = self.totals.get(key, 0) + mi.extract_counter_value(info)
                    elif info.urn == mi.USER_COUNTER_URN:
                        key = (labels.get(mi.NAME_LABEL, ""), "user", labels.get(mi.PTRANSFORM_LABEL, ""),
                               labels.get(mi.NAMESPACE_LABEL, ""), "Sum")
                        self.totals[key] = self.totals.get(key, 0) + mi.extract_counter_value(info)
                    elif info.urn == mi.USER_DISTRIBUTION_URN:
                        key = (labels.get(mi.NAME_LABEL, ""), "user", labels.get(mi.PTRANSFORM_LABEL, ""),
                               labels.get(mi.NAMESPACE_LABEL, ""), "Distribution")
                        c, s, lo, hi = mi.extract_distribution(info)
                        p = self.totals.get(key, (0, 0, lo, hi))
                        self.totals[key] = (p[0] + c, p[1] + s, min(p[2], lo), max(p[3], hi))
                except Exception:
                    continue
            rows = []
            for (name, origin, step, ns, kind), v in self.totals.items():
                row = {"name": name, "origin": origin, "step": step, "kind": kind}
                if origin == "user":
                    row["namespace"] = ns
                else:
                    row["pcollection"] = ns
                if kind == "Distribution":
                    row.update(count=v[0], total=v[1], min=v[2], max=v[3])
                else:
                    row["value"] = v
                rows.append(row)
            try:
                with open(self.path + ".tmp", "w") as f:
                    json.dump(rows, f)
                os.replace(self.path + ".tmp", self.path)
            except OSError:
                pass


class ControlProxy(beam_fn_api_pb2_grpc.BeamFnControlServicer):
    """The runner's control service, as one worker sees it."""

    def __init__(self, upstream, totals):
        self.channel = grpc.insecure_channel(upstream, options=OPTS)
        self.stub = beam_fn_api_pb2_grpc.BeamFnControlStub(self.channel)
        self.totals = totals
        self.meta = {}  # short ID: MonitoringInfo (no payload)
        self.pending = {}  # proxy instruction ID: monitoring_data awaiting its metadata

    def _record(self, data, to_worker):
        missing = [sid for sid in data if sid not in self.meta]
        if missing:
            iid = "cb-metrics-%d" % next(_ids)
            self.pending[iid] = data
            to_worker.put(beam_fn_api_pb2.InstructionRequest(
                instruction_id=iid,
                monitoring_infos=beam_fn_api_pb2.MonitoringInfosMetadataRequest(monitoring_info_id=missing)))
            return
        infos = []
        for sid, payload in data.items():
            info = metrics_pb2.MonitoringInfo()
            info.CopyFrom(self.meta[sid])
            info.payload = payload
            infos.append(info)
        self.totals.add(infos)

    def Control(self, request_iterator, context):
        md = [(k, v) for k, v in context.invocation_metadata() if not k.startswith(":") and k not in ("user-agent",)]
        to_runner = queue.Queue()
        to_worker = queue.Queue()
        done = object()

        def runner_requests():
            while True:
                r = to_runner.get()
                if r is done:
                    return
                yield r

        upstream = self.stub.Control(runner_requests(), metadata=md)

        def from_worker():
            try:
                for resp in request_iterator:
                    iid = resp.instruction_id
                    if iid in self.pending:
                        data = self.pending.pop(iid)
                        for sid, info in resp.monitoring_infos.monitoring_info.items():
                            self.meta[sid] = info
                        self._record({k: v for k, v in data.items() if k in self.meta}, to_worker)
                        continue
                    if resp.HasField("process_bundle"):
                        pb = resp.process_bundle
                        if pb.monitoring_infos:
                            self.totals.add(pb.monitoring_infos)
                        elif pb.monitoring_data:
                            self._record(dict(pb.monitoring_data), to_worker)
                    elif resp.HasField("monitoring_infos"):
                        for sid, info in resp.monitoring_infos.monitoring_info.items():
                            self.meta[sid] = info
                    to_runner.put(resp)
            finally:
                to_runner.put(done)

        def from_runner():
            try:
                for req in upstream:
                    to_worker.put(req)
            except grpc.RpcError:
                pass
            finally:
                to_worker.put(done)

        threading.Thread(target=from_worker, daemon=True).start()
        threading.Thread(target=from_runner, daemon=True).start()
        while True:
            r = to_worker.get()
            if r is done:
                return
            yield r

    def GetProcessBundleDescriptor(self, request, context):
        return self.stub.GetProcessBundleDescriptor(request)


class ProvisionProxy(beam_provision_api_pb2_grpc.ProvisionServiceServicer):
    """The runner's provision service, as one worker sees it: its
    ProvisionInfo with the control endpoint made control's (the proxy's)."""

    def __init__(self, upstream, control):
        self.channel = grpc.insecure_channel(upstream, options=OPTS)
        self.stub = beam_provision_api_pb2_grpc.ProvisionServiceStub(self.channel)
        self.control = control

    def GetProvisionInfo(self, request, context):
        md = [(k, v) for k, v in context.invocation_metadata() if not k.startswith(":") and k not in ("user-agent",)]
        resp = self.stub.GetProvisionInfo(request, metadata=md)
        if resp.info.HasField("control_endpoint"):
            resp.info.control_endpoint.url = self.control
        return resp


class Pool(beam_fn_api_pb2_grpc.BeamFnExternalWorkerPoolServicer):
    def __init__(self, target, metrics):
        self.channel = grpc.insecure_channel(target, options=OPTS)
        self.stub = beam_fn_api_pb2_grpc.BeamFnExternalWorkerPoolStub(self.channel)
        self.metrics = metrics
        self.servers = {}

    def StartWorker(self, request, context):
        # The Java SDK's pool starts beside this one (its JVM, slower).
        grpc.channel_ready_future(self.channel).result(timeout=300)
        totals = Totals(os.path.join(self.metrics, "java-%s.json" % (request.worker_id or next(_ids))))
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=16), options=OPTS)
        beam_fn_api_pb2_grpc.add_BeamFnControlServicer_to_server(ControlProxy(request.control_endpoint.url, totals), server)
        port = server.add_insecure_port("localhost:0")
        fwd = beam_fn_api_pb2.StartWorkerRequest()
        fwd.CopyFrom(request)
        fwd.control_endpoint.url = "localhost:%d" % port
        if request.HasField("provision_endpoint") and request.provision_endpoint.url:
            beam_provision_api_pb2_grpc.add_ProvisionServiceServicer_to_server(
                ProvisionProxy(request.provision_endpoint.url, fwd.control_endpoint.url), server)
            fwd.provision_endpoint.url = fwd.control_endpoint.url
        server.start()
        self.servers[request.worker_id] = server
        return self.stub.StartWorker(fwd)

    def StopWorker(self, request, context):
        resp = self.stub.StopWorker(request)
        server = self.servers.pop(request.worker_id, None)
        if server is not None:
            server.stop(5)
        return resp


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--listen", required=True)
    ap.add_argument("--target", required=True)
    ap.add_argument("--metrics", required=True)
    args = ap.parse_args()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8), options=OPTS)
    beam_fn_api_pb2_grpc.add_BeamFnExternalWorkerPoolServicer_to_server(Pool(args.target, args.metrics), server)
    server.add_insecure_port(args.listen)
    server.start()
    print("ready", flush=True)
    server.wait_for_termination()


if __name__ == "__main__":
    main()
