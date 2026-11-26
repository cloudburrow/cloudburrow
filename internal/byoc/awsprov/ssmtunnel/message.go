// Package ssmtunnel is AWS Systems Manager Session Manager port forwarding
// in Go (#2209): the data channel a StartSession call opens, spoken over its
// WebSocket stream URL, carrying a local TCP listener's connections to a
// port on the instance with no inbound port open and no plugin installed.
//
// The wire protocol is the one AWS's session-manager-plugin speaks
// (https://github.com/aws/session-manager-plugin, Apache-2.0; read, not
// copied): the OpenDataChannel token message, then binary AgentMessages
// (message.go), each stream message acknowledged and resent until it is,
// delivered in sequence order; a handshake naming the session type; and,
// for an agent newer than 3.0.196.0, the port session's TCP connections
// multiplexed over the stream with smux, as the plugin's
// MuxPortForwarding does.
package ssmtunnel

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Message types (the 32-byte MessageType field).
const (
	InputStreamData   = "input_stream_data"
	OutputStreamData  = "output_stream_data"
	Acknowledge       = "acknowledge"
	ChannelClosed     = "channel_closed"
	StartPublication  = "start_publication"
	PausePublication  = "pause_publication"
	messageTypeLength = 32
)

// PayloadType is a stream message's payload kind.
type PayloadType uint32

// The payload types.
const (
	PayloadOutput            PayloadType = 1
	PayloadError             PayloadType = 2
	PayloadSize              PayloadType = 3
	PayloadParameter         PayloadType = 4
	PayloadHandshakeRequest  PayloadType = 5
	PayloadHandshakeResponse PayloadType = 6
	PayloadHandshakeComplete PayloadType = 7
	PayloadEncChallengeReq   PayloadType = 8
	PayloadEncChallengeResp  PayloadType = 9
	PayloadFlag              PayloadType = 10
	PayloadStdErr            PayloadType = 11
	PayloadExitCode          PayloadType = 12
)

// Flag is a PayloadFlag message's one value.
type Flag uint32

// The flags a port session sends.
const (
	FlagDisconnectToPort   Flag = 1
	FlagTerminateSession   Flag = 2
	FlagConnectToPortError Flag = 3
)

// The header's layout: HeaderLength (4), MessageType (32), SchemaVersion
// (4), CreatedDate (8, epoch ms), SequenceNumber (8), Flags (8), MessageId
// (16), PayloadDigest (32, SHA-256), PayloadType (4), then PayloadLength (4)
// and the payload. HeaderLength counts up to PayloadLength, so 116.
const (
	offType     = 4
	offSchema   = offType + messageTypeLength
	offCreated  = offSchema + 4
	offSeq      = offCreated + 8
	offFlags    = offSeq + 8
	offID       = offFlags + 8
	offDigest   = offID + 16
	offPayType  = offDigest + 32
	offPayLen   = offPayType + 4
	headerLen   = offPayLen
	minimumSize = offPayLen + 4
)

// Message is one AgentMessage.
type Message struct {
	Type          string
	SchemaVersion uint32
	CreatedDate   time.Time
	Sequence      int64
	Flags         uint64
	ID            [16]byte // the UUID's bytes, most significant first
	PayloadType   PayloadType
	Payload       []byte
}

// NewID is a random (version 4) UUID.
func NewID() [16]byte {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return b
}

// UUIDString is id in the 8-4-4-4-12 form.
func UUIDString(id [16]byte) string {
	h := hex.EncodeToString(id[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Marshal is the message on the wire. The UUID goes least significant half
// first, as the plugin's putUuid writes it.
func (m Message) Marshal() []byte {
	b := make([]byte, minimumSize+len(m.Payload))
	binary.BigEndian.PutUint32(b[0:], headerLen)
	copy(b[offType:offSchema], bytes.Repeat([]byte{' '}, messageTypeLength))
	copy(b[offType:offSchema], m.Type)
	binary.BigEndian.PutUint32(b[offSchema:], m.SchemaVersion)
	created := m.CreatedDate
	if created.IsZero() {
		created = time.Now()
	}
	binary.BigEndian.PutUint64(b[offCreated:], uint64(created.UnixMilli()))
	binary.BigEndian.PutUint64(b[offSeq:], uint64(m.Sequence))
	binary.BigEndian.PutUint64(b[offFlags:], m.Flags)
	copy(b[offID:], m.ID[8:16])
	copy(b[offID+8:], m.ID[0:8])
	sum := sha256.Sum256(m.Payload)
	copy(b[offDigest:], sum[:])
	binary.BigEndian.PutUint32(b[offPayType:], uint32(m.PayloadType))
	binary.BigEndian.PutUint32(b[offPayLen:], uint32(len(m.Payload)))
	copy(b[minimumSize:], m.Payload)
	return b
}

// ErrMalformed is a frame that is not an AgentMessage.
var ErrMalformed = errors.New("malformed Session Manager message")

// Unmarshal reads one message, checking its length and payload digest.
func Unmarshal(b []byte) (Message, error) {
	if len(b) < minimumSize {
		return Message{}, fmt.Errorf("%w: %d bytes", ErrMalformed, len(b))
	}
	hl := binary.BigEndian.Uint32(b[0:])
	if hl < headerLen || int(hl)+4 > len(b) {
		return Message{}, fmt.Errorf("%w: header length %d", ErrMalformed, hl)
	}
	var m Message
	m.Type = strings.TrimSpace(string(bytes.Trim(b[offType:offSchema], "\x00")))
	m.SchemaVersion = binary.BigEndian.Uint32(b[offSchema:])
	m.CreatedDate = time.UnixMilli(int64(binary.BigEndian.Uint64(b[offCreated:])))
	m.Sequence = int64(binary.BigEndian.Uint64(b[offSeq:]))
	m.Flags = binary.BigEndian.Uint64(b[offFlags:])
	copy(m.ID[8:16], b[offID:offID+8])
	copy(m.ID[0:8], b[offID+8:offID+16])
	m.PayloadType = PayloadType(binary.BigEndian.Uint32(b[offPayType:]))
	n := binary.BigEndian.Uint32(b[offPayLen:])
	payload := b[hl+4:]
	if int(n) != len(payload) {
		return Message{}, fmt.Errorf("%w: payload length %d, %d bytes follow", ErrMalformed, n, len(payload))
	}
	if sum := sha256.Sum256(payload); !bytes.Equal(sum[:], b[offDigest:offDigest+32]) && len(payload) > 0 {
		return Message{}, fmt.Errorf("%w: payload digest does not match", ErrMalformed)
	}
	m.Payload = append([]byte(nil), payload...)
	return m, nil
}

// AckContent is an acknowledge message's JSON payload.
type AckContent struct {
	MessageType         string `json:"AcknowledgedMessageType"`
	MessageID           string `json:"AcknowledgedMessageId"`
	SequenceNumber      int64  `json:"AcknowledgedMessageSequenceNumber"`
	IsSequentialMessage bool   `json:"IsSequentialMessage"`
}

// OpenDataChannel is the first (text) frame: the session's token.
type OpenDataChannel struct {
	MessageSchemaVersion string `json:"MessageSchemaVersion"`
	RequestID            string `json:"RequestId"`
	TokenValue           string `json:"TokenValue"`
	ClientID             string `json:"ClientId"`
	ClientVersion        string `json:"ClientVersion"`
}

// HandshakeRequest is the agent's first stream message.
type HandshakeRequest struct {
	AgentVersion           string            `json:"AgentVersion"`
	RequestedClientActions []RequestedAction `json:"RequestedClientActions"`
}

// RequestedAction is one action the agent asks the client to take.
type RequestedAction struct {
	ActionType       string          `json:"ActionType"`
	ActionParameters json.RawMessage `json:"ActionParameters"`
}

// SessionTypeParameters is a SessionType action's parameters.
type SessionTypeParameters struct {
	SessionType string         `json:"SessionType"`
	Properties  map[string]any `json:"Properties"`
}

// HandshakeResponse is the client's answer.
type HandshakeResponse struct {
	ClientVersion          string            `json:"ClientVersion"`
	ProcessedClientActions []ProcessedAction `json:"ProcessedClientActions"`
	Errors                 []string          `json:"Errors"`
}

// Action statuses.
const (
	ActionSuccess     = 1
	ActionFailed      = 2
	ActionUnsupported = 3
)

// ProcessedAction is one action's outcome.
type ProcessedAction struct {
	ActionType   string `json:"ActionType"`
	ActionStatus int    `json:"ActionStatus"`
	ActionResult any    `json:"ActionResult"`
	Error        string `json:"Error"`
}

// HandshakeComplete ends the handshake.
type HandshakeComplete struct {
	HandshakeTimeToComplete time.Duration `json:"HandshakeTimeToComplete"`
	CustomerMessage         string        `json:"CustomerMessage"`
}

// ChannelClosedContent is a channel_closed message's payload.
type ChannelClosedContent struct {
	MessageID     string `json:"MessageId"`
	CreatedDate   string `json:"CreatedDate"`
	DestinationID string `json:"DestinationId"`
	SessionID     string `json:"SessionId"`
	MessageType   string `json:"MessageType"`
	SchemaVersion int    `json:"SchemaVersion"`
	Output        string `json:"Output"`
}

// FlagPayload is a flag's four big-endian bytes.
func FlagPayload(f Flag) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(f))
	return b
}

// MuxAfterAgentVersion is the newest agent that does not multiplex a port
// session's connections; later agents expect smux on the stream.
const MuxAfterAgentVersion = "3.0.196.0"

// KeepAliveOffAfterAgentVersion is the newest agent that expects smux's
// keep-alive; later ones expect it off (it would defeat the idle timeout).
const KeepAliveOffAfterAgentVersion = "3.1.1511.0"

// VersionAfter reports whether dotted version v is later than base; an
// unreadable v is not.
func VersionAfter(v, base string) bool {
	a, b := strings.Split(v, "."), strings.Split(base, ".")
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			if _, err := fmt.Sscanf(a[i], "%d", &x); err != nil {
				return false
			}
		}
		if i < len(b) {
			_, _ = fmt.Sscanf(b[i], "%d", &y)
		}
		if x != y {
			return x > y
		}
	}
	return false
}
