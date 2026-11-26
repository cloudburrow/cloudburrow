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
# certificate names, answered by the instance's service. A route whose
# target ends ";host" (LISTEN+tls@NAME=HOST:PORT;host) sends each HTTP/1.1
# request with the Host it dials, HOST:PORT: the instance's services
# answer only their own names (internal/hostguard), not a Google host's.
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
    target, _, flag = target.partition(";")
    host, port = target.rsplit(":", 1)
    listen, _, sni = listen.partition("@")
    if listen.endswith("+tls"):
        by_name = TLS_ROUTES.setdefault(listen_addr(listen[:-len("+tls")]), {})
        by_name[sni or None] = (socket.gethostbyname(host), int(port), flag == "host")
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


class HostRewriter:
    """Rewrites the Host header of each HTTP/1.1 request of a stream to
    host, passing bodies (Content-Length or chunked) through as they are;
    after an Upgrade, or a head it cannot read, it passes everything."""

    def __init__(self, host):
        self.host = host.encode()
        self.buf = b""
        self.state = "head"
        self.left = 0

    def feed(self, data):
        self.buf += data
        out = []
        while self.buf:
            if self.state == "raw":
                out.append(self.buf)
                self.buf = b""
            elif self.state == "head":
                i = self.buf.find(b"\r\n\r\n")
                if i < 0:
                    if len(self.buf) > 1 << 16:
                        self.state = "raw"
                        continue
                    break
                head, self.buf = self.buf[:i], self.buf[i + 4:]
                lines = head.split(b"\r\n")
                length, chunked, upgrade = 0, False, False
                for j in range(1, len(lines)):
                    key, _, value = lines[j].partition(b":")
                    key = key.strip().lower()
                    if key == b"host":
                        lines[j] = b"Host: " + self.host
                    elif key == b"content-length":
                        try:
                            length = int(value.strip())
                        except ValueError:
                            upgrade = True
                    elif key == b"transfer-encoding" and b"chunked" in value.lower():
                        chunked = True
                    elif key == b"upgrade":
                        upgrade = True
                out.append(b"\r\n".join(lines) + b"\r\n\r\n")
                if upgrade:
                    self.state = "raw"
                elif chunked:
                    self.state = "size"
                elif length > 0:
                    self.state, self.left = "body", length
            elif self.state in ("body", "chunk"):
                n = min(len(self.buf), self.left)
                out.append(self.buf[:n])
                self.buf, self.left = self.buf[n:], self.left - n
                if self.left == 0:
                    self.state = "head" if self.state == "body" else "size"
            else:  # "size" or "trailer": one line at a time
                i = self.buf.find(b"\r\n")
                if i < 0:
                    break
                line, self.buf = self.buf[:i + 2], self.buf[i + 2:]
                out.append(line)
                if self.state == "trailer":
                    if line == b"\r\n":
                        self.state = "head"
                    continue
                try:
                    size = int(line.split(b";")[0].strip(), 16)
                except ValueError:
                    self.state = "raw"
                    continue
                if size == 0:
                    self.state = "trailer"
                else:
                    self.state, self.left = "chunk", size + 2
        return b"".join(out)


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
        rewrite = None
        if target[2] and tls.selected_alpn_protocol() != "h2":
            rewrite = HostRewriter("%s:%d" % (target[0], target[1]))
        r, w = await asyncio.open_connection(target[0], target[1])
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
                if rewrite is not None:
                    data = rewrite.feed(data)
                    if not data:
                        continue
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
        opened = await dial(target, reader, first)
        if opened is None:
            writer.close()
            return
        r, w, to_up, to_down = opened
        await asyncio.gather(pipe(reader, w, to_up), pipe(r, writer, to_down))
    return handle


# How long a connection's target is dialled again for while nothing has
# gone either way on it.
DIAL_FOR = 15.0


async def dial(target, reader, first):
    """Connects to target for a client that has sent first (b"" if
    nothing yet): (upstream reader, writer, bytes for upstream, bytes for
    the client), or None. A host target is often a tunnel
    (internal/netfwd's kubectl port forward) that is briefly being
    re-established: a connection it refuses, or closes before a byte has
    gone either way, is dialled again, for DIAL_FOR seconds, as nothing of
    the client's is lost; a server that speaks first (MySQL) is then
    answered rather than the client cut off."""
    loop = asyncio.get_running_loop()
    deadline = loop.time() + DIAL_FOR
    while True:
        try:
            r, w = await asyncio.open_connection(*target)
        except Exception:
            r = None
        if r is not None:
            if first:
                # The client spoke first: the rest is its exchange.
                return r, w, first, b""
            up = asyncio.ensure_future(r.read(65536))
            down = asyncio.ensure_future(reader.read(65536))
            await asyncio.wait({up, down}, return_when=asyncio.FIRST_COMPLETED)
            if down.done():
                # The client spoke (or left) first. A pending read
                # cancelled takes nothing from the stream's buffer.
                greeting = b""
                if up.done():
                    greeting = b"" if up.exception() else up.result()
                else:
                    up.cancel()
                data = b"" if down.exception() else down.result()
                if not data:
                    w.close()
                    return None
                return r, w, data, greeting
            down.cancel()
            data = b"" if up.exception() else up.result()
            if data:
                return r, w, b"", data
            w.close()
        if loop.time() >= deadline:
            return None
        await asyncio.sleep(0.5)


async def main():
    servers = []
    for listen in sorted(set(ROUTES) | set(TLS_ROUTES)):
        servers.append(await asyncio.start_server(handler(listen, ROUTES.get(listen)), listen[0], listen[1]))
    print("ready", flush=True)
    await asyncio.gather(*(s.serve_forever() for s in servers))


asyncio.run(main())
