# The Spark container's one route (internal/sparkproc): on the job's
# --internal network as bigquery.cloudburrow.internal (8080, plain) and
# bigquerystorage.cloudburrow.internal (8443, TLS with the run's
# certificate, ALPN h2), relaying to the instance's BigQuery REST port and
# its Storage Read and Write API port, the two addresses given. It dials
# nothing else. Each further argument, [LISTEN=]NAME:PORT, is a server on
# the internal network (a Spark Connect session's, a Dataproc cluster's
# web interfaces): the relay listens on LISTEN (PORT when none), published
# on the host's loopback, and relays to it (resolved at each connection,
# as the server starts after the relay); a target @HOST:PORT is a host
# address, resolved at the start; a target HOST1|HOST2|...:PORT is a High
# Availability cluster's ResourceManagers, relayed to the active one (the
# one whose ws/v1/cluster/info reports haState ACTIVE).
import asyncio
import inspect
import json
import socket
import ssl
import sys
import time


def resolve(hostport):
    host, port = hostport.rsplit(":", 1)
    return socket.gethostbyname(host), int(port)


# Resolved before the relay joins the internal network.
REST = resolve(sys.argv[1])
STORAGE = resolve(sys.argv[2])


async def pipe(reader, writer):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
    except Exception:
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def ha_state(host, port):
    # The ResourceManager's haState, "" when it does not answer.
    try:
        r, w = await asyncio.wait_for(asyncio.open_connection(host, port), 2)
    except Exception:
        return ""
    try:
        w.write(("GET /ws/v1/cluster/info HTTP/1.0\r\nHost: %s:%d\r\nAccept: application/json\r\n\r\n" % (host, port)).encode())
        await w.drain()
        data = await asyncio.wait_for(r.read(), 3)
        body = data.split(b"\r\n\r\n", 1)[1]
        return json.loads(body).get("clusterInfo", {}).get("haState", "")
    except Exception:
        return ""
    finally:
        try:
            w.close()
        except Exception:
            pass


def active(hosts, port):
    # The active ResourceManager of hosts, cached for two seconds; the
    # first that answers while none is active (a failover under way).
    cache = {"at": 0.0, "host": hosts[0]}

    async def pick():
        if time.monotonic() - cache["at"] < 2:
            return cache["host"], port
        states = await asyncio.gather(*(ha_state(h, port) for h in hosts))
        host = next((h for h, st in zip(hosts, states) if st == "ACTIVE"), None)
        if host is None:
            host = next((h for h, st in zip(hosts, states) if st), hosts[0])
        else:
            cache["at"] = time.monotonic()
        cache["host"] = host
        return host, port

    return pick


def relay(target):
    async def handle(reader, writer):
        try:
            if inspect.iscoroutinefunction(target):
                up_reader, up_writer = await asyncio.open_connection(*(await target()))
            elif callable(target):
                up_reader, up_writer = await asyncio.open_connection(*target())
            else:
                up_reader, up_writer = await asyncio.open_connection(*target)
        except Exception:
            writer.close()
            return
        await asyncio.gather(pipe(reader, up_writer), pipe(up_reader, writer))

    return handle


async def main():
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain("/cb/tls/cert.pem", "/cb/tls/key.pem")
    tls.set_alpn_protocols(["h2"])
    rest = await asyncio.start_server(relay(REST), "0.0.0.0", 8080)
    storage = await asyncio.start_server(relay(STORAGE), "0.0.0.0", 8443, ssl=tls)
    servers = [rest.serve_forever(), storage.serve_forever()]
    for arg in sys.argv[3:]:
        listen, _, target = arg.rpartition("=")
        if target.startswith("@"):
            # A host address (the instance's Cloud Storage for a
            # cluster's Hadoop), resolved now, as REST and STORAGE are.
            fixed = resolve(target[1:])
            inbound = await asyncio.start_server(relay(fixed), "0.0.0.0", int(listen or fixed[1]))
            servers.append(inbound.serve_forever())
            continue
        name, port = target.rsplit(":", 1)
        if "|" in name:
            inbound = await asyncio.start_server(relay(active(name.split("|"), int(port))), "0.0.0.0", int(listen or port))
            servers.append(inbound.serve_forever())
            continue
        inbound = await asyncio.start_server(relay(lambda name=name, port=port: (name, int(port))), "0.0.0.0", int(listen or port))
        servers.append(inbound.serve_forever())
    print("ready", flush=True)
    await asyncio.gather(*servers)


asyncio.run(main())
