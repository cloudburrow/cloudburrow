# Loaded by every Python process of a Dataflow job's containers
# (internal/beamrun), on their PYTHONPATH: the launcher, the SDK harness
# and its workers. On the job's network the instance's APIs are the
# relay's: Cloud Storage and Pub/Sub through their clients' documented
# emulator variables (STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST),
# BigQuery through BIGQUERY_EMULATOR_HOST for google-cloud-bigquery and
# the base URL of Beam's own BigQuery client, which reads no variable.
# Credentials are a fixed access token of the instance, as no metadata
# server is reachable: the instance takes every caller to be its account.
import os

if os.environ.get("CB_DATAFLOW"):
    try:
        import google.auth
        import google.auth._default
        from google.oauth2 import credentials as _credentials

        def _default(scopes=None, request=None, quota_project_id=None, default_scopes=None):
            token = os.environ.get("CB_ACCESS_TOKEN", "cloudburrow-dataflow")
            return _credentials.Credentials(token=token), os.environ.get("CB_PROJECT")

        google.auth.default = _default
        google.auth._default.default = _default
    except Exception:  # pragma: no cover - google-auth is in every SDK image
        pass
    _bq = os.environ.get("CB_BIGQUERY_URL")
    if _bq:
        try:
            from apache_beam.io.gcp.internal.clients.bigquery import bigquery_v2_client as _bqc

            _bqc.BigqueryV2.BASE_URL = _bq
            _bqc.BigqueryV2.MTLS_BASE_URL = _bq
        except Exception:
            pass

    _metrics = os.environ.get("CB_METRICS_DIR")
    if _metrics:
        # The job's metrics (#2215): Flink's job server answers no
        # metrics while a job runs, so each SDK worker keeps the totals of
        # what its bundles report (each bundle's MonitoringInfos: the
        # PCollections' element counts, the pipeline's own counters and
        # distributions) in a file of its own, which the launcher sums.
        # As Dataflow defines them, a metric's committed value is that of
        # the bundles that finished successfully and its tentative
        # (attempted) value that of every bundle attempted, a failed one
        # too: a bundle that raises adds what it counted to the tentative
        # totals only. Progress reports (process_bundle_progress, which
        # also calls monitoring_infos) are not totals, so they are not
        # added.
        try:
            import json as _json
            import threading as _threading

            from apache_beam.metrics import monitoring_infos as _mi
            from apache_beam.runners.worker import bundle_processor as _bp

            _lock = _threading.Lock()
            _totals = {}  # (tentative, key): value
            _orig = _bp.BundleProcessor.process_bundle

            def _add(infos, tentative):
                for info in infos:
                    labels = dict(info.labels)
                    if info.urn == _mi.ELEMENT_COUNT_URN:
                        key = (tentative, "ElementCount", "dataflow/v1b3", "", labels.get(_mi.PCOLLECTION_LABEL, ""), "Sum")
                        _totals[key] = _totals.get(key, 0) + _mi.extract_counter_value(info)
                    elif info.urn == _mi.USER_COUNTER_URN:
                        key = (tentative, labels.get(_mi.NAME_LABEL, ""), "user", labels.get(_mi.PTRANSFORM_LABEL, ""),
                               labels.get(_mi.NAMESPACE_LABEL, ""), "Sum")
                        _totals[key] = _totals.get(key, 0) + _mi.extract_counter_value(info)
                    elif info.urn == _mi.USER_DISTRIBUTION_URN:
                        key = (tentative, labels.get(_mi.NAME_LABEL, ""), "user", labels.get(_mi.PTRANSFORM_LABEL, ""),
                               labels.get(_mi.NAMESPACE_LABEL, ""), "Distribution")
                        c, s, lo, hi = _mi.extract_distribution(info)
                        p = _totals.get(key, (0, 0, lo, hi))
                        _totals[key] = (p[0] + c, p[1] + s, min(p[2], lo), max(p[3], hi))

            def _record(processor, committed):
                try:
                    infos = processor.monitoring_infos()
                    with _lock:
                        _add(infos, True)
                        if committed:
                            _add(infos, False)
                        rows = []
                        for (tentative, name, origin, step, ns, kind), v in _totals.items():
                            row = {"name": name, "origin": origin, "step": step, "kind": kind, "tentative": tentative}
                            if origin == "user":
                                row["namespace"] = ns
                            else:
                                row["pcollection"] = ns
                            if kind == "Distribution":
                                row.update(count=v[0], total=v[1], min=v[2], max=v[3])
                            else:
                                row["value"] = v
                            rows.append(row)
                        path = os.path.join(_metrics, "%d.json" % os.getpid())
                        with open(path + ".tmp", "w") as f:
                            _json.dump(rows, f)
                        os.replace(path + ".tmp", path)
                except Exception:
                    pass

            def _process_bundle(self, *args, **kwargs):
                try:
                    out = _orig(self, *args, **kwargs)
                except BaseException:
                    _record(self, False)
                    raise
                _record(self, True)
                return out

            _bp.BundleProcessor.process_bundle = _process_bundle
        except Exception:
            pass
