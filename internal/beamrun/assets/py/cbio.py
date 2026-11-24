"""Pub/Sub for a Dataflow job on the local Flink runner (internal/beamrun).

A Python pipeline that the DataflowRunner submits reads and writes Pub/Sub
through two transforms Dataflow's service implements itself,
beam:transform:pubsub_read:v1 and beam:transform:pubsub_write:v1 (the
SDK's iobase.Read and Write of _PubSubSource and _PubSubSink). Apache
Flink's runner implements neither, so rewrite() replaces each with
transforms of the SDK that do the same through google-cloud-pubsub,
against the instance's Pub/Sub (PUBSUB_EMULATOR_HOST): an unbounded
splittable DoFn that pulls the subscription (one of its own on the topic
when the read names a topic, as Dataflow makes), emitting each message's
data, or its serialized PubsubMessage proto when the read is
with_attributes, timestamped by its publish time or its timestamp
attribute; and a DoFn that publishes each serialized message. A drain
(the file CB_DRAIN) ends the reads: what they read is still processed and
written, the watermark advances to the end of time and the job finishes.
"""
import os
import time
import uuid

import apache_beam as beam
from apache_beam.io.iobase import RestrictionTracker
from apache_beam.io.watermark_estimators import ManualWatermarkEstimator
from apache_beam.portability import common_urns
from apache_beam.portability.api import beam_runner_api_pb2
from apache_beam.transforms.core import RestrictionProvider
from apache_beam.utils.timestamp import Duration, Timestamp


def drained():
    path = os.environ.get("CB_DRAIN", "")
    return bool(path) and os.path.exists(path)


class _Restriction(object):
    def __init__(self, done=False):
        self.done = done


class _Tracker(RestrictionTracker):
    """An unbounded restriction: the subscription, until drained."""

    def __init__(self, restriction):
        self._r = restriction

    def current_restriction(self):
        return self._r

    def try_claim(self, position):
        return not self._r.done

    def try_split(self, fraction_of_remainder):
        if self._r.done:
            return None
        # A checkpoint: this attempt is done, the subscription resumes.
        self._r = _Restriction(True)
        return _Restriction(True), _Restriction(False)

    def check_done(self):
        return

    def current_progress(self):
        from apache_beam.io.iobase import RestrictionProgress
        return RestrictionProgress(fraction=0.0)

    def is_bounded(self):
        return False


class _Provider(RestrictionProvider):
    def initial_restriction(self, element):
        return _Restriction(False)

    def create_tracker(self, restriction):
        return _Tracker(restriction)

    def restriction_size(self, element, restriction):
        return 1

    def restriction_coder(self):
        return beam.coders.PickleCoder()

    def truncate(self, element, restriction):
        # A drain truncates the read to nothing more.
        return None


class ReadPubSubFn(beam.DoFn):
    def __init__(self, topic, subscription, with_attributes, timestamp_attribute, id_attribute):
        self.topic = topic
        self.subscription = subscription
        self.with_attributes = with_attributes
        self.timestamp_attribute = timestamp_attribute
        self.id_attribute = id_attribute

    def setup(self):
        from google.cloud import pubsub_v1
        self._sub = pubsub_v1.SubscriberClient()
        if not self.subscription:
            # A topic read: a subscription of the job's own, as Dataflow
            # makes one.
            project = self.topic.split("/")[1]
            name = "projects/%s/subscriptions/%s_cloudburrow_dataflow_%s" % (
                project, self.topic.split("/")[-1], uuid.uuid4().hex[:12])
            self._sub.create_subscription(request={"name": name, "topic": self.topic})
            self.subscription = name

    @beam.DoFn.unbounded_per_element()
    def process(self, element, tracker=beam.DoFn.RestrictionParam(_Provider()),
                watermark=beam.DoFn.WatermarkEstimatorParam(ManualWatermarkEstimator.default_provider())):
        if drained():
            # Done: the restriction ends, and with it the watermark hold.
            return
        if not tracker.try_claim(0):
            return
        from google.cloud.pubsub_v1.types import PubsubMessage
        resp = self._sub.pull(request={"subscription": self.subscription, "max_messages": 500,
                                       "return_immediately": True}, timeout=10)
        acks = []
        oldest = None
        for rm in resp.received_messages:
            m = rm.message
            ts = Timestamp.from_utc_datetime(m.publish_time) if m.publish_time else Timestamp.now()
            if self.timestamp_attribute and self.timestamp_attribute in m.attributes:
                v = m.attributes[self.timestamp_attribute]
                try:
                    ts = Timestamp(micros=int(v) * 1000)
                except ValueError:
                    ts = Timestamp.from_rfc3339(v)
            oldest = ts if oldest is None or ts < oldest else oldest
            if self.with_attributes:
                out = PubsubMessage.serialize(m)
            else:
                out = bytes(m.data)
            acks.append(rm.ack_id)
            yield beam.window.TimestampedValue(out, ts)
        if acks:
            self._sub.acknowledge(request={"subscription": self.subscription, "ack_ids": acks})
        # The watermark: the oldest message read, or, idle, a second ago.
        now = Timestamp.now() - Duration(seconds=1)
        wm = oldest if oldest is not None else now
        cur = watermark.current_watermark()
        if cur is None or wm > cur:
            watermark.set_watermark(wm)
        tracker.defer_remainder(Duration(seconds=0 if acks else 1))


class WritePubSubFn(beam.DoFn):
    def __init__(self, topic):
        self.topic = topic

    def setup(self):
        from google.cloud import pubsub_v1
        self._pub = pubsub_v1.PublisherClient()

    def start_bundle(self):
        self._futures = []

    def process(self, element):
        from google.cloud.pubsub_v1.types import PubsubMessage
        m = PubsubMessage.deserialize(element)
        self._futures.append(self._pub.publish(self.topic, m.data, **dict(m.attributes)))

    def finish_bundle(self):
        for f in self._futures:
            f.result(timeout=60)
        self._futures = []


def _splice(pipeline, mini, prefix, env_id):
    """Copies mini's components into pipeline, their IDs prefixed."""
    c, mc = pipeline.components, mini.components
    ren = lambda i: prefix + i
    # What mini's transforms need of the runner (a splittable DoFn's
    # requirement), which its validator checks the pipeline declares.
    for req in mini.requirements:
        if req not in pipeline.requirements:
            pipeline.requirements.append(req)
    for cid, coder in mc.coders.items():
        n = beam_runner_api_pb2.Coder()
        n.CopyFrom(coder)
        del n.component_coder_ids[:]
        n.component_coder_ids.extend(ren(x) for x in coder.component_coder_ids)
        c.coders[ren(cid)].CopyFrom(n)
    for wid, ws in mc.windowing_strategies.items():
        n = beam_runner_api_pb2.WindowingStrategy()
        n.CopyFrom(ws)
        n.window_coder_id = ren(ws.window_coder_id)
        n.environment_id = env_id
        c.windowing_strategies[ren(wid)].CopyFrom(n)
    for pid, pc in mc.pcollections.items():
        n = beam_runner_api_pb2.PCollection()
        n.CopyFrom(pc)
        n.coder_id = ren(pc.coder_id)
        n.windowing_strategy_id = ren(pc.windowing_strategy_id)
        c.pcollections[ren(pid)].CopyFrom(n)
    out = {}
    for tid, t in mc.transforms.items():
        n = beam_runner_api_pb2.PTransform()
        n.CopyFrom(t)
        n.unique_name = prefix + t.unique_name
        if n.environment_id:
            n.environment_id = env_id
        for k in list(n.inputs):
            n.inputs[k] = ren(t.inputs[k])
        for k in list(n.outputs):
            n.outputs[k] = ren(t.outputs[k])
        del n.subtransforms[:]
        n.subtransforms.extend(ren(x) for x in t.subtransforms)
        if t.spec.urn == common_urns.primitives.PAR_DO.urn:
            p = beam_runner_api_pb2.ParDoPayload()
            p.ParseFromString(t.spec.payload)
            if p.restriction_coder_id:
                p.restriction_coder_id = ren(p.restriction_coder_id)
            n.spec.payload = p.SerializeToString()
        out[tid] = n
    return out


def _leaves(mini):
    """mini's leaf transforms in order: Impulse, then the ParDo."""
    ts = mini.components.transforms
    order = []

    def walk(i):
        t = ts[i]
        if t.subtransforms:
            for s in t.subtransforms:
                walk(s)
        else:
            order.append(i)
    for r in mini.root_transform_ids:
        walk(r)
    return order


def resource_name(path):
    """A Pub/Sub topic or subscription's resource name. The Java SDK's
    payloads name them in PubsubIO's own path form (PubsubClient's
    TopicPath and SubscriptionPath: /topics/PROJECT/TOPIC,
    /subscriptions/PROJECT/SUBSCRIPTION), which the Pub/Sub API does not
    take; Python's are already projects/PROJECT/topics/TOPIC."""
    parts = path.split("/")
    if len(parts) == 4 and parts[0] == "" and parts[1] in ("topics", "subscriptions"):
        return "projects/%s/%s/%s" % (parts[2], parts[1], parts[3])
    return path


def _drop(pipeline, tid, keep):
    """Removes transform tid and its parts from the pipeline, with the
    PCollections they made that are not in keep."""
    ts = pipeline.components.transforms
    if tid not in ts:
        return
    t = ts[tid]
    for s in list(t.subtransforms):
        _drop(pipeline, s, keep)
    for pc in t.outputs.values():
        if pc not in keep:
            pipeline.components.pcollections.pop(pc, None)
    del ts[tid]


def rewrite(pipeline, env_id):
    """Replaces the pipeline's Pub/Sub reads and writes in place."""
    n = 0
    for tid in list(pipeline.components.transforms):
        if tid not in pipeline.components.transforms:
            # A part of a replaced read or write (indexing a protobuf map
            # would make it again).
            continue
        t = pipeline.components.transforms[tid]
        urn = t.spec.urn
        if urn not in (common_urns.composites.PUBSUB_READ.urn, common_urns.composites.PUBSUB_WRITE.urn):
            continue
        n += 1
        prefix = "cb_pubsub%d_" % n
        mp = beam.Pipeline()
        if urn == common_urns.composites.PUBSUB_READ.urn:
            pl = beam_runner_api_pb2.PubSubReadPayload()
            pl.ParseFromString(t.spec.payload)
            _ = (mp | "Impulse" >> beam.Impulse()
                 | "Read" >> beam.ParDo(ReadPubSubFn(resource_name(pl.topic), resource_name(pl.subscription), pl.with_attributes,
                                                     pl.timestamp_attribute, pl.id_attribute)).with_output_types(bytes))
        else:
            pl = beam_runner_api_pb2.PubSubWritePayload()
            pl.ParseFromString(t.spec.payload)
            _ = (mp | "Impulse" >> beam.Impulse()
                 | "Write" >> beam.ParDo(WritePubSubFn(resource_name(pl.topic))))
        mini = mp.to_runner_api()
        leaves = _leaves(mini)
        added = _splice(pipeline, mini, prefix, env_id)
        impulse, pardo = added[leaves[0]], added[leaves[-1]]
        if urn == common_urns.composites.PUBSUB_READ.urn:
            # The read's output is the ParDo's: downstream is unchanged.
            out_key = list(pardo.outputs)[0]
            dead = pardo.outputs[out_key]
            pardo.outputs[out_key] = list(t.outputs.values())[0]
            del pipeline.components.pcollections[dead]
            keep = [leaves[0], leaves[-1]]
        else:
            in_key = list(pardo.inputs)[0]
            dead = pardo.inputs[in_key]
            pardo.inputs[in_key] = list(t.inputs.values())[0]
            del pipeline.components.pcollections[dead]
            keep = [leaves[-1]]
        t.ClearField("spec")
        # The SDK's own expansion of the read or write (the Java SDK's
        # PubsubUnboundedSource and Sink are composites) is replaced: its
        # parts go, as their names are the replacement's.
        boundary = set(t.inputs.values()) | set(t.outputs.values())
        for s in list(t.subtransforms):
            _drop(pipeline, s, boundary)
        del t.subtransforms[:]
        t.environment_id = ""
        for k in keep:
            nid = prefix + k
            added[k].unique_name = t.unique_name + "/" + mini.components.transforms[k].unique_name
            pipeline.components.transforms[nid].CopyFrom(added[k])
            t.subtransforms.append(nid)
    return n
