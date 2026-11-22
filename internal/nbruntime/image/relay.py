# The notebook runtime's one route (internal/nbruntime): on the runtime's
# --internal network as host.docker.internal:8080, relaying to the
# instance's BigQuery REST port, and :8081 to its Dataproc API port when
# one is given. It dials nothing else.
import asyncio
import socket
import sys


def resolve(hostport):
    host, port = hostport.rsplit(":", 1)
    return socket.gethostbyname(host), int(port)


# Resolved before the relay joins the internal network: BigQuery's on
# 8080, and the Dataproc API's on 8081 when given (#2156), which an Apache
# Spark notebook's DataprocSparkSession reaches its Spark Connect session
# through.
TARGETS = {8080: resolve(sys.argv[1])}
if len(sys.argv) > 2 and sys.argv[2]:
    TARGETS[8081] = resolve(sys.argv[2])


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
