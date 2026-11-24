# A Dataflow job's one route out (internal/beamrun): on the job's
# --internal network, it listens on each LISTEN port given as
# LISTEN=HOST:PORT and relays to that address of the host (the
# instance's Cloud Storage, Pub/Sub, BigQuery and Dataflow ports, and its
# Datastore, Bigtable and Spanner emulators). It
# dials nothing else. The addresses are resolved before the relay joins
# the internal network, from the default bridge.
import asyncio
import socket
import sys

ROUTES = {}
for arg in sys.argv[1:]:
    listen, target = arg.split("=", 1)
    host, port = target.rsplit(":", 1)
    ROUTES[int(listen)] = (socket.gethostbyname(host), int(port))


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
            r, w = await asyncio.open_connection(*target)
        except Exception:
            writer.close()
            return
        await asyncio.gather(pipe(reader, w), pipe(r, writer))
    return handle


async def main():
    servers = []
    for listen, target in ROUTES.items():
        servers.append(await asyncio.start_server(handler(target), "0.0.0.0", listen))
    print("ready", flush=True)
    await asyncio.gather(*(s.serve_forever() for s in servers))


asyncio.run(main())
