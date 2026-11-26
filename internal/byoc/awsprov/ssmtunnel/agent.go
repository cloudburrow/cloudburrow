package ssmtunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
)

// AgentOptions configure ServeAgent, the agent's side of a port session,
// which the local AWS fake (awsprov/awsfake) runs in place of the SSM agent
// on an instance.
type AgentOptions struct {
	// Token is the session's TokenValue; the client's OpenDataChannel must
	// carry it.
	Token string
	// Version is the agent version the handshake names; it decides whether
	// the client multiplexes (MuxAfterAgentVersion).
	Version string
	// Port is the document's portNumber, echoed in the handshake.
	Port string
	// Dial connects to the port on the instance.
	Dial func() (net.Conn, error)
	// Drop, when set, swallows an outgoing frame on its attempt (tests).
	Drop func(m Message, attempt int) bool
}

// AgentDefaultVersion is a current SSM agent's version.
const AgentDefaultVersion = "3.3.1802.0"

// ServeAgent speaks the agent's side of the data channel on ws until the
// session ends.
func ServeAgent(ws *websocket.Conn, o AgentOptions) error {
	defer ws.Close()
	if o.Version == "" {
		o.Version = AgentDefaultVersion
	}
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	kind, b, err := ws.ReadMessage()
	if err != nil {
		return err
	}
	_ = ws.SetReadDeadline(time.Time{})
	var open OpenDataChannel
	if kind != websocket.TextMessage || json.Unmarshal(b, &open) != nil || open.TokenValue == "" {
		return errors.New("the client's first frame is not an OpenDataChannel message")
	}
	if open.TokenValue != o.Token {
		return errors.New("the client's token is not the session's")
	}
	e := newEndpoint(ws, false)
	e.drop = o.Drop
	e.start()
	defer e.Close()
	params, _ := json.Marshal(SessionTypeParameters{SessionType: "Port",
		Properties: map[string]any{"portNumber": o.Port, "type": "LocalPortForwarding"}})
	req, _ := json.Marshal(HandshakeRequest{AgentVersion: o.Version,
		RequestedClientActions: []RequestedAction{{ActionType: "SessionType", ActionParameters: params}}})
	start := time.Now()
	if err := e.Send(PayloadHandshakeRequest, req); err != nil {
		return err
	}
	m, err := e.next(30 * time.Second)
	if err != nil {
		return err
	}
	if m.PayloadType != PayloadHandshakeResponse {
		return fmt.Errorf("expected the handshake response, got payload type %d", m.PayloadType)
	}
	var resp HandshakeResponse
	if err := json.Unmarshal(m.Payload, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("the client refused the handshake: %v", resp.Errors)
	}
	done, _ := json.Marshal(HandshakeComplete{HandshakeTimeToComplete: time.Since(start)})
	if err := e.Send(PayloadHandshakeComplete, done); err != nil {
		return err
	}
	p := &pipe{e: e}
	if !VersionAfter(o.Version, MuxAfterAgentVersion) {
		conn, err := o.Dial()
		if err != nil {
			_ = e.Send(PayloadFlag, FlagPayload(FlagConnectToPortError))
			return err
		}
		defer conn.Close()
		end := make(chan struct{}, 2)
		p.onFlag = func(f Flag) {
			if f == FlagDisconnectToPort || f == FlagTerminateSession {
				_ = conn.Close()
			}
		}
		go func() { _, _ = io.Copy(conn, p); end <- struct{}{} }()
		go func() { _, _ = io.Copy(p, conn); end <- struct{}{} }()
		<-end
		return nil
	}
	cfg := smux.DefaultConfig()
	if VersionAfter(o.Version, KeepAliveOffAfterAgentVersion) {
		cfg.KeepAliveDisabled = true
	}
	p.onFlag = func(f Flag) {
		if f == FlagTerminateSession {
			e.Close()
		}
	}
	mux, err := smux.Server(p, cfg)
	if err != nil {
		return err
	}
	defer mux.Close()
	for {
		s, err := mux.AcceptStream()
		if err != nil {
			return nil
		}
		go func() {
			defer s.Close()
			conn, err := o.Dial()
			if err != nil {
				_ = e.Send(PayloadFlag, FlagPayload(FlagConnectToPortError))
				return
			}
			defer conn.Close()
			end := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(conn, s); end <- struct{}{} }()
			go func() { _, _ = io.Copy(s, conn); end <- struct{}{} }()
			<-end
		}()
	}
}
