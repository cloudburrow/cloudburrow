# The notebook runtime's one route (internal/nbruntime): on the runtime's
# --internal network as host.docker.internal:8080, relaying to the
# instance's BigQuery REST port, the one address given. It dials nothing
# else.
import asyncio
import socket
import sys


def resolve(hostport):
    host, port = hostport.rsplit(":", 1)
    return socket.gethostbyname(host), int(port)


# Resolved before the relay joins the internal network.
TARGET = resolve(sys.argv[1])


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


async def handle(reader, writer):
    try:
        up_reader, up_writer = await asyncio.open_connection(*TARGET)
    except Exception:
        writer.close()
        return
    await asyncio.gather(pipe(reader, up_writer), pipe(up_reader, writer))


async def main():
    server = await asyncio.start_server(handle, "0.0.0.0", 8080)
    print("ready", flush=True)
    await server.serve_forever()


asyncio.run(main())
