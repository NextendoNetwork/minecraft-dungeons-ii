package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// rtaSocket serves Xbox Live real-time activity (wss://rta.xboxlive.com/connect), which XSAPI's
// social manager opens after sign-in. Messages are JSON arrays: subscribe [1, seq, uri] is
// answered [1, seq, 0, subscriptionId, data]; unsubscribe [2, seq, subscriptionId] is answered
// [2, seq, 0]. No events are pushed yet.
func (s server) rtaSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{Subprotocols: []string{"rta.xboxlive.com.V2"}, HandshakeTimeout: 10 * time.Second}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 16)
	nextID := 1
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg []json.RawMessage
		if json.Unmarshal(data, &msg) != nil || len(msg) < 2 {
			continue
		}
		var kind int
		json.Unmarshal(msg[0], &kind)
		var out []any
		switch kind {
		case 1:
			out = []any{1, msg[1], 0, nextID, map[string]any{}}
			nextID++
		case 2:
			out = []any{2, msg[1], 0}
		default:
			continue
		}
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if conn.WriteJSON(out) != nil {
			return
		}
	}
}
