# The Spark container's one route (internal/sparkproc): on the job's
# --internal network as bigquery.cloudburrow.internal (8080, plain) and
# bigquerystorage.cloudburrow.internal (8443, TLS with the run's
# certificate, ALPN h2), relaying to the instance's BigQuery REST port and
# its Storage Read and Write API port, the two addresses given. It dials
# nothing else. A third argument, NAME:PORT, is a Spark Connect session's
# server on the internal network: the relay listens on PORT too, published
# on the host's loopback, and relays to it (resolved at each connection,
# as the server starts after the relay).
import asyncio
import socket
import ssl
import sys


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


def relay(target):
    async def handle(reader, writer):
        try:
            if callable(target):
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
    if len(sys.argv) > 3:
        name, port = sys.argv[3].rsplit(":", 1)
        inbound = await asyncio.start_server(relay(lambda: (name, int(port))), "0.0.0.0", int(port))
        servers.append(inbound.serve_forever())
    print("ready", flush=True)
    await asyncio.gather(*servers)


asyncio.run(main())
