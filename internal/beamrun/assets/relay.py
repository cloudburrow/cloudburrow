# A Dataflow job's one route out (internal/beamrun): on the job's
# --internal network, it listens on each LISTEN port given as
# LISTEN=HOST:PORT and relays to that address of the host (the
# instance's Cloud Storage, Pub/Sub, BigQuery and Dataflow ports, and its
# Datastore, Bigtable and Spanner emulators). A LISTEN+tls=HOST:PORT route
# sends the TLS connections of LISTEN to HOST:PORT instead, decrypted with
# /cb/tls's certificate (BigQuery's Storage API, whose Java gRPC clients
# take the REST client's endpoint and speak TLS). It dials nothing else.
# The addresses are resolved before the relay joins the internal network,
# from the default bridge. A LISTEN of IP:PORT listens on that address
# only: a private services address the job's workers dial (a Cloud SQL
# instance's private IP, which cbnet.py put on their loopback), relayed
# to the relay's port for it. A LISTEN+tls@NAME=HOST:PORT route sends a
# TLS connection whose client named NAME (its SNI) to HOST:PORT: a Google
# API host the job's containers resolve to the relay, which its
# certificate names, answered by the instance's service.
import asyncio
import socket
import ssl
import sys

ROUTES = {}
TLS_ROUTES = {}


def listen_addr(listen):
    """(host, port) of a route's LISTEN: PORT on every address, or IP:PORT."""
    if ":" in listen:
        host, port = listen.rsplit(":", 1)
        return (host, int(port))
    return ("0.0.0.0", int(listen))


for arg in sys.argv[1:]:
    listen, target = arg.split("=", 1)
    host, port = target.rsplit(":", 1)
    listen, _, sni = listen.partition("@")
    if listen.endswith("+tls"):
        by_name = TLS_ROUTES.setdefault(listen_addr(listen[:-len("+tls")]), {})
        by_name[sni or None] = (socket.gethostbyname(host), int(port))
    else:
        ROUTES[listen_addr(listen)] = (socket.gethostbyname(host), int(port))

TLS = None
if TLS_ROUTES:
    TLS = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    TLS.load_cert_chain("/cb/tls/cert.pem", "/cb/tls/key.pem")
    TLS.set_alpn_protocols(["h2"])
    # The name each connection's client asked for, by its TLS object.
    SNI = {}

    def sni_callback(obj, name, ctx):
        SNI[id(obj)] = name
        return None

    TLS.sni_callback = sni_callback


async def pipe(reader, writer, first=b""):
    try:
        if first:
            writer.write(first)
            await writer.drain()
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


async def tls_relay(first, reader, writer, targets):
    """Terminates the client's TLS (its first bytes already read) and
    relays the plaintext to the target of the name it asked for (else to
    the route's own target; else it is closed)."""
    incoming, outgoing = ssl.MemoryBIO(), ssl.MemoryBIO()
    tls = TLS.wrap_bio(incoming, outgoing, server_side=True)
    incoming.write(first)

    async def flush():
        data = outgoing.read()
        if data:
            writer.write(data)
            await writer.drain()

    async def more():
        data = await reader.read(65536)
        if not data:
            incoming.write_eof()
            return False
        incoming.write(data)
        return True

    try:
        while True:
            try:
                tls.do_handshake()
                break
            except ssl.SSLWantReadError:
                await flush()
                if not await more():
                    return
        await flush()
        name = SNI.pop(id(tls), None)
        target = targets.get(name) if name in targets else targets.get(None)
        if target is None:
            writer.close()
            return
        r, w = await asyncio.open_connection(*target)
    except Exception:
        SNI.pop(id(tls), None)
        writer.close()
        return

    async def up():
        try:
            while True:
                try:
                    data = tls.read(65536)
                except ssl.SSLWantReadError:
                    await flush()
                    if not await more():
                        break
                    continue
                if not data:
                    break
                await flush()
                w.write(data)
                await w.drain()
        except Exception:
            pass
        finally:
            try:
                w.close()
            except Exception:
                pass

    async def down():
        try:
            while True:
                data = await r.read(65536)
                if not data:
                    break
                tls.write(data)
                await flush()
        except Exception:
            pass
        finally:
            try:
                writer.close()
            except Exception:
                pass

    await asyncio.gather(up(), down())


def handler(listen, target):
    async def handle(reader, writer):
        first = b""
        if listen in TLS_ROUTES:
            # A TLS ClientHello's first byte is its record type, 22.
            first = await reader.read(65536)
            if not first:
                writer.close()
                return
            if first[0] == 0x16:
                await tls_relay(first, reader, writer, TLS_ROUTES[listen])
                return
        if target is None:
            writer.close()
            return
        try:
            r, w = await asyncio.open_connection(*target)
        except Exception:
            writer.close()
            return
        await asyncio.gather(pipe(reader, w, first), pipe(r, writer))
    return handle


async def main():
    servers = []
    for listen in sorted(set(ROUTES) | set(TLS_ROUTES)):
        servers.append(await asyncio.start_server(handler(listen, ROUTES.get(listen)), listen[0], listen[1]))
    print("ready", flush=True)
    await asyncio.gather(*(s.serve_forever() for s in servers))


asyncio.run(main())
