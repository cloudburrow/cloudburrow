# The notebook runtime's one route (internal/nbruntime): on the runtime's
# --internal network as host.docker.internal, relaying each of its ports
# to one of the instance's served APIs on the host. It dials nothing else.
#
#   relay.py BIGQUERY [DATAPROC]      8080 to BigQuery's REST port, and
#                                     8081 to the Dataproc API's (#2156);
#   relay.py PORT=HOST:PORT ...       PORT to HOST:PORT, for a Workbench
#                                     instance (#2198): BigQuery, Cloud
#                                     Storage, the Dataproc API and the
#                                     instance's metadata server;
#   relay.py in PORT=IP:PORT          inbound: PORT, published on the host's
#                                     loopback alone, to the instance's
#                                     JupyterLab on the internal network,
#                                     the console's proxy its one client.
import asyncio
import socket
import sys


def resolve(hostport):
    host, port = hostport.rsplit(":", 1)
    return socket.gethostbyname(host), int(port)


# Resolved before the relay joins the internal network.
TARGETS = {}
args = sys.argv[1:]
inbound = bool(args) and args[0] == "in"
if inbound:
    args = args[1:]
if args and "=" not in args[0]:
    TARGETS[8080] = resolve(args[0])
    if len(args) > 1 and args[1]:
        TARGETS[8081] = resolve(args[1])
else:
    for a in args:
        port, target = a.split("=", 1)
        if target:
            TARGETS[int(port)] = resolve(target)


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


def handler(target):
    async def handle(reader, writer):
        try:
            up_reader, up_writer = await asyncio.open_connection(*target)
        except Exception:
            writer.close()
            return
        await asyncio.gather(pipe(reader, up_writer), pipe(up_reader, writer))
    return handle


async def main():
    servers = [await asyncio.start_server(handler(t), "0.0.0.0", port) for port, t in TARGETS.items()]
    print("ready", flush=True)
    await asyncio.gather(*(s.serve_forever() for s in servers))


asyncio.run(main())
