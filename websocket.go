package main

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const clientProtocol = "json.reliable.webpubsub.azure.v1"

// clientSocket is one player's live websocket; HTTP handlers push party events through it.
type clientSocket struct {
	write func(any) error
	seq   atomic.Uint64
}

func (c *clientSocket) party(event string, party map[string]any, memberIDs []string) error {
	return c.write(partyMessage(c.seq.Add(1), event, party, memberIDs))
}

var (
	socketsMu sync.Mutex
	sockets   = map[uint64]*clientSocket{}
)

func registerSocket(pid uint64, c *clientSocket) {
	socketsMu.Lock()
	sockets[pid] = c
	socketsMu.Unlock()
}

// socketAddress is the public address a player's websocket last connected from.
func socketAddress(pid uint64) string {
	socketsMu.Lock()
	defer socketsMu.Unlock()
	return socketAddrs[pid]
}

var socketAddrs = map[uint64]string{}

func unregisterSocket(pid uint64, c *clientSocket) {
	socketsMu.Lock()
	if sockets[pid] == c {
		delete(sockets, pid)
	}
	socketsMu.Unlock()
}

func socketFor(pid uint64) *clientSocket {
	socketsMu.Lock()
	defer socketsMu.Unlock()
	return sockets[pid]
}

// Tickets are purpose-bound; they cannot be used as login or character credentials.
func (s server) socketTicket(pid uint64, id string, exp int64) string {
	payload := fmt.Sprintf("%d.%s.%d", pid, id, exp)
	return payload + "." + s.sign("client-websocket:", payload)
}
func (s server) socketIdentity(ticket string) (uint64, string, bool) {
	p := strings.Split(ticket, ".")
	if len(p) != 4 {
		return 0, "", false
	}
	pid, err := strconv.ParseUint(p[0], 10, 64)
	exp, e := strconv.ParseInt(p[2], 10, 64)
	id, idErr := hex.DecodeString(p[1])
	valid := err == nil && e == nil && idErr == nil && len(id) == 16 && pid != 0 && exp > time.Now().Unix() && hmac.Equal([]byte(p[3]), []byte(s.sign("client-websocket:", strings.Join(p[:3], "."))))
	return pid, p[1], valid
}
func (s server) websocketRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/websocket/client/negotiate" && r.URL.Path != "/nextendo/websocket/client" {
		return false
	}
	if r.Method != http.MethodGet {
		failure(w, 405, "MethodNotAllowed")
		return true
	}
	if r.URL.Path == "/websocket/client/negotiate" {
		pid, ok := s.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if !ok {
			failure(w, 401, "InvalidSessionTicket")
			return true
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			failure(w, 500, "RandomSourceFailed")
			return true
		}
		ticket := s.socketTicket(pid, hex.EncodeToString(random[:]), time.Now().Add(time.Hour).Unix())
		reply(w, 200, map[string]string{"url": "wss://vex.minecraftservices.com/nextendo/websocket/client?access_token=" + url.QueryEscape(ticket)})
		return true
	}
	ticket := r.URL.Query().Get("access_token")
	reconnect := r.URL.Query().Get("awps_reconnection_token")
	if reconnect != "" {
		ticket = reconnect
	}
	pid, id, ok := s.socketIdentity(ticket)
	if !ok || (reconnect != "" && r.URL.Query().Get("awps_connection_id") != id) {
		failure(w, 401, "InvalidSocketTicket")
		return true
	}
	supported := false
	for _, protocol := range websocket.Subprotocols(r) {
		if protocol == clientProtocol {
			supported = true
		}
	}
	if !supported {
		failure(w, 400, "UnsupportedSubprotocol")
		return true
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{clientProtocol}, HandshakeTimeout: 10 * time.Second}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return true
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	var writeLock sync.Mutex
	write := func(value any) error {
		writeLock.Lock()
		defer writeLock.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(value)
	}
	if write(map[string]string{"type": "system", "event": "connected", "userId": strconv.FormatUint(pid, 10), "connectionId": id, "reconnectionToken": ticket}) != nil {
		return true
	}
	log.Print("client websocket connected")
	defer log.Print("client websocket disconnected")
	sock := &clientSocket{write: write}
	registerSocket(pid, sock)
	socketsMu.Lock()
	socketAddrs[pid] = clientAddress(r)
	socketsMu.Unlock()
	defer unregisterSocket(pid, sock)
	defer s.partyDisconnected(pid)
	// The game never sends anything while idle, so keep the socket alive with protocol pings
	// (answered by the client's websocket library) instead of timing it out after 3 minutes.
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(3 * time.Minute)) })
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(30 * time.Second):
			}
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)) != nil {
				return
			}
		}
	}()
	// The party system subscribes shortly after CONNECTED, so the party is pushed after a delay
	// and repeated once in case the first copy arrived before the subscription.
	go func() {
		name := minecraftUserName(pid)
		for _, delay := range []time.Duration{time.Second, 3 * time.Second} {
			select {
			case <-done:
				return
			case <-time.After(delay):
			}
			if sock.party("PartyCreated", s.partyFor(pid), []string{name}) != nil {
				return
			}
			log.Print("pushed Party/PartyCreated")
		}
	}()
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		var msg struct {
			Type  string  `json:"type"`
			AckID *uint64 `json:"ackId"`
		}
		if conn.ReadJSON(&msg) != nil {
			return true
		}
		switch msg.Type {
		case "ping":
			if write(map[string]string{"type": "pong"}) != nil {
				return true
			}
		case "sequenceAck":
			// Party pushes are idempotent and never resent, so acknowledgements need no bookkeeping.
		default:
			// Do not acknowledge unimplemented game operations as successful.
			log.Printf("unsupported websocket message type=%q", msg.Type)
			if msg.AckID != nil {
				if write(map[string]any{"type": "ack", "ackId": *msg.AckID, "success": false, "error": map[string]string{"name": "Forbidden", "message": "Operation not supported"}}) != nil {
					return true
				}
			}
		}
	}
}
