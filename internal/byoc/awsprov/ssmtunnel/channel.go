package ssmtunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The data channel's timings and limits, the plugin's (config.go):
// a stream message not acknowledged within ResendAfter is sent again, every
// ResendAfter, until it is or MaxResends is reached; at most MaxUnacked
// are outstanding at once; a stream message's payload is at most
// PayloadChunk bytes.
const (
	ResendAfter  = 200 * time.Millisecond
	MaxResends   = 3000
	MaxUnacked   = 10000
	PayloadChunk = 1024
	pingEvery    = 5 * time.Minute
)

// ErrChannelClosed is a data channel closed by the other side or by Close.
var ErrChannelClosed = errors.New("the Session Manager data channel is closed")

type pending struct {
	raw      []byte
	sent     time.Time
	attempts int
}

// endpoint is one side of a data channel: it numbers and sends its stream
// messages, resends each until acknowledged, acknowledges the other side's
// and delivers them in sequence order. The client sends input_stream_data,
// the agent output_stream_data.
type endpoint struct {
	ws       *websocket.Conn
	sendType string
	recvType string

	wmu sync.Mutex // gorilla allows one writer at a time

	mu       sync.Mutex
	cond     *sync.Cond
	nextSeq  int64
	unacked  map[int64]*pending
	expect   int64
	buffer   map[int64]Message
	closed   bool
	closeErr error

	deliver chan Message
	done    chan struct{}

	// drop, when set, swallows an outgoing frame (tests: a lost frame must
	// be resent).
	drop func(Message, int) bool
}

func newEndpoint(ws *websocket.Conn, client bool) *endpoint {
	e := &endpoint{ws: ws, unacked: map[int64]*pending{}, buffer: map[int64]Message{},
		deliver: make(chan Message, 256), done: make(chan struct{})}
	e.cond = sync.NewCond(&e.mu)
	if client {
		e.sendType, e.recvType = InputStreamData, OutputStreamData
	} else {
		e.sendType, e.recvType = OutputStreamData, InputStreamData
	}
	return e
}

func (e *endpoint) start() {
	go e.readLoop()
	go e.resendLoop()
}

func (e *endpoint) write(kind int, b []byte) error {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	_ = e.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return e.ws.WriteMessage(kind, b)
}

// Send sends payload as stream messages of at most PayloadChunk bytes.
func (e *endpoint) Send(pt PayloadType, payload []byte) error {
	for {
		n := min(len(payload), PayloadChunk)
		if err := e.sendOne(pt, payload[:n]); err != nil {
			return err
		}
		payload = payload[n:]
		if len(payload) == 0 {
			return nil
		}
	}
}

func (e *endpoint) sendOne(pt PayloadType, payload []byte) error {
	e.mu.Lock()
	for !e.closed && len(e.unacked) >= MaxUnacked {
		e.cond.Wait()
	}
	if e.closed {
		err := e.closeErr
		e.mu.Unlock()
		return err
	}
	m := Message{Type: e.sendType, SchemaVersion: 1, Sequence: e.nextSeq, ID: NewID(), PayloadType: pt, Payload: payload}
	raw := m.Marshal()
	e.unacked[m.Sequence] = &pending{raw: raw, sent: time.Now(), attempts: 1}
	e.nextSeq++
	e.mu.Unlock()
	if e.drop != nil && e.drop(m, 1) {
		return nil
	}
	if err := e.write(websocket.BinaryMessage, raw); err != nil {
		e.fail(fmt.Errorf("send on the data channel: %w", err))
		return err
	}
	return nil
}

func (e *endpoint) ack(m Message) error {
	content, _ := json.Marshal(AckContent{MessageType: m.Type, MessageID: UUIDString(m.ID), SequenceNumber: m.Sequence, IsSequentialMessage: true})
	a := Message{Type: Acknowledge, SchemaVersion: 1, Flags: 3, ID: NewID(), Payload: content}
	return e.write(websocket.BinaryMessage, a.Marshal())
}

func (e *endpoint) readLoop() {
	defer close(e.deliver)
	for {
		kind, b, err := e.ws.ReadMessage()
		if err != nil {
			e.fail(fmt.Errorf("%w: %v", ErrChannelClosed, err))
			return
		}
		if kind != websocket.BinaryMessage {
			continue
		}
		m, err := Unmarshal(b)
		if err != nil {
			continue // the plugin drops a frame it cannot read too
		}
		switch m.Type {
		case Acknowledge:
			var a AckContent
			if json.Unmarshal(m.Payload, &a) == nil {
				e.mu.Lock()
				delete(e.unacked, a.SequenceNumber)
				e.cond.Broadcast()
				e.mu.Unlock()
			}
		case e.recvType:
			if err := e.ack(m); err != nil {
				e.fail(err)
				return
			}
			var ready []Message
			e.mu.Lock()
			switch {
			case m.Sequence == e.expect:
				ready = append(ready, m)
				e.expect++
				for {
					next, ok := e.buffer[e.expect]
					if !ok {
						break
					}
					delete(e.buffer, e.expect)
					ready = append(ready, next)
					e.expect++
				}
			case m.Sequence > e.expect && len(e.buffer) < MaxUnacked:
				e.buffer[m.Sequence] = m
			}
			e.mu.Unlock()
			for _, r := range ready {
				select {
				case e.deliver <- r:
				case <-e.done:
					return
				}
			}
		case ChannelClosed:
			var c ChannelClosedContent
			_ = json.Unmarshal(m.Payload, &c)
			msg := "the session ended"
			if c.Output != "" {
				msg += ": " + c.Output
			}
			e.fail(fmt.Errorf("%w: %s", ErrChannelClosed, msg))
			return
		case StartPublication, PausePublication:
		}
	}
}

func (e *endpoint) resendLoop() {
	t := time.NewTicker(ResendAfter / 2)
	defer t.Stop()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-e.done:
			return
		case <-ping.C:
			e.wmu.Lock()
			_ = e.ws.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(10*time.Second))
			e.wmu.Unlock()
		case now := <-t.C:
			var due [][]byte
			var dropped []bool
			e.mu.Lock()
			for seq, p := range e.unacked {
				if now.Sub(p.sent) < ResendAfter {
					continue
				}
				if p.attempts >= MaxResends {
					e.mu.Unlock()
					e.fail(fmt.Errorf("stream message %d was not acknowledged after %d sends", seq, p.attempts))
					return
				}
				p.attempts++
				p.sent = now
				drop := false
				if e.drop != nil {
					m, _ := Unmarshal(p.raw)
					drop = e.drop(m, p.attempts)
				}
				due = append(due, p.raw)
				dropped = append(dropped, drop)
			}
			e.mu.Unlock()
			for i, raw := range due {
				if dropped[i] {
					continue
				}
				if err := e.write(websocket.BinaryMessage, raw); err != nil {
					e.fail(err)
					return
				}
			}
		}
	}
}

func (e *endpoint) fail(err error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed, e.closeErr = true, err
	e.cond.Broadcast()
	e.mu.Unlock()
	close(e.done)
	_ = e.ws.Close()
}

// Close ends the channel.
func (e *endpoint) Close() { e.fail(ErrChannelClosed) }

// Err is why the channel closed, or nil while it is open.
func (e *endpoint) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closeErr
}

// Outstanding is how many sent messages await acknowledgement.
func (e *endpoint) Outstanding() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.unacked)
}

// next is the next delivered stream message.
func (e *endpoint) next(timeout time.Duration) (Message, error) {
	var tc <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		tc = t.C
	}
	select {
	case m, ok := <-e.deliver:
		if !ok {
			if err := e.Err(); err != nil {
				return Message{}, err
			}
			return Message{}, ErrChannelClosed
		}
		return m, nil
	case <-tc:
		return Message{}, errors.New("timed out waiting for the Session Manager agent")
	}
}

// pipe carries a stream's output payloads as bytes: Read returns what the
// other side sent as PayloadOutput, Write sends PayloadOutput. Flags are
// handed to onFlag.
type pipe struct {
	e      *endpoint
	onFlag func(Flag)
	rmu    sync.Mutex
	rest   []byte
}

func (p *pipe) Read(b []byte) (int, error) {
	p.rmu.Lock()
	defer p.rmu.Unlock()
	for len(p.rest) == 0 {
		m, err := p.e.next(0)
		if err != nil {
			return 0, err
		}
		switch m.PayloadType {
		case PayloadOutput:
			p.rest = m.Payload
		case PayloadFlag:
			if len(m.Payload) >= 4 && p.onFlag != nil {
				p.onFlag(Flag(uint32(m.Payload[0])<<24 | uint32(m.Payload[1])<<16 | uint32(m.Payload[2])<<8 | uint32(m.Payload[3])))
			}
		}
	}
	n := copy(b, p.rest)
	p.rest = p.rest[n:]
	return n, nil
}

func (p *pipe) Write(b []byte) (int, error) {
	if err := p.e.Send(PayloadOutput, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *pipe) Close() error {
	p.e.Close()
	return nil
}
