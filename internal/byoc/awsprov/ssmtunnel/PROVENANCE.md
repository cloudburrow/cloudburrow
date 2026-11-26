# internal/byoc/awsprov/ssmtunnel provenance

A Go implementation of AWS Systems Manager Session Manager's data-channel
protocol, for port forwarding (the `AWS-StartPortForwardingSession`
document), written for #2209 so the user installs no plugin. The protocol
was read from AWS's session-manager-plugin,
<https://github.com/aws/session-manager-plugin>, tag `1.2.896.0` (commit
`6430675a44032a551be1937fb27a2d3fdfeb27f7`), under the Apache License,
Version 2.0. No plugin code is copied: the files below are written anew
from what the plugin's sources show the wire to be.

| File | Upstream source read | What it implements |
| --- | --- | --- |
| `message.go` | `src/message/clientmessage.go`, `messageparser.go`, `src/message/messageconstants.go` | The 116-byte AgentMessage header, its field order and widths, the UUID's halves order, the SHA-256 payload digest; message, payload and flag types; the acknowledge, handshake and channel_closed JSON; the agent versions after which smux is used and keep-alive is off. |
| `channel.go` | `src/datachannel/streaming.go`, `src/communicator/websocketchannel.go` | Sequence numbers, acknowledgements, in-order delivery with a buffer of early messages, resending unacknowledged messages (200 ms, at most 3000 times), the outgoing window, 1024-byte chunks, the ping. |
| `tunnel.go` | `src/sessionmanagerplugin/session/portsession/`, `src/sessionmanagerplugin/session/sessionhandler.go` | OpenDataChannel, the client side of the handshake (SessionType Port, KMSEncryption refused), the local listener, smux (`muxportforwarding.go`) or one session per connection (`basicportforwarding.go`), the TerminateSession flag. |
| `agent.go` | the same files, from the agent's side | A stand-in agent for tests and awsfake: token check, handshake, smux server or basic mode, the ConnectToPortError flag. |

Multiplexing uses github.com/xtaci/smux (MIT) and the WebSocket
github.com/gorilla/websocket (BSD-2-Clause), as the plugin does.
