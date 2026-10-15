package main

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClientWebsocket(t *testing.T) {
	s := server{[]byte("websocket-test-key")}
	ts := httptest.NewServer(s)
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/websocket/client/negotiate", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("anonymous negotiation accepted")
	}
	req.Header.Set("Authorization", "Bearer "+s.issue(1, "entity"))
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		URL string `json:"url"`
	}
	err = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 {
		t.Fatal("negotiation failed", err)
	}
	u, err := url.Parse(body.URL)
	if err != nil || u.Scheme != "wss" || u.Host != "vex.minecraftservices.com" {
		t.Fatal("invalid websocket URL")
	}
	endpoint := "ws" + strings.TrimPrefix(ts.URL, "http") + u.RequestURI()
	dialer := websocket.Dialer{Subprotocols: []string{clientProtocol}}
	conn, _, err := dialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var connected map[string]string
	if err = conn.ReadJSON(&connected); err != nil {
		t.Fatal(err)
	}
	if connected["type"] != "system" || connected["event"] != "connected" || connected["userId"] != "1" || conn.Subprotocol() != clientProtocol {
		t.Fatal(connected)
	}
	if err = conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		t.Fatal(err)
	}
	var pong map[string]string
	if err = conn.ReadJSON(&pong); err != nil || pong["type"] != "pong" {
		t.Fatal("heartbeat failed", err)
	}
	var pushed struct {
		Type       string
		SequenceID uint64 `json:"sequenceId"`
		Data       struct{ Topic, Event, Body string }
	}
	if err = conn.ReadJSON(&pushed); err != nil || pushed.Type != "message" || pushed.SequenceID != 1 || pushed.Data.Topic != "Party" || pushed.Data.Event != "PartyCreated" {
		t.Fatal("party push missing", err, pushed)
	}
	var partyBody struct {
		UpdatePartyMemberIDs []string `json:"updatePartyMemberIds"`
		Party                struct {
			ID, OwnerID, Type, Code string
			PartyMembers            []map[string]any
		}
	}
	if err = json.Unmarshal([]byte(pushed.Data.Body), &partyBody); err != nil || partyBody.Party.OwnerID != minecraftUserName(1) || len(partyBody.Party.PartyMembers) != 1 || partyBody.Party.PartyMembers[0]["status"] != "CONNECTED" || len(partyBody.UpdatePartyMemberIDs) != 1 {
		t.Fatal("bad party body", err, pushed.Data.Body)
	}
	reconnect := "ws" + strings.TrimPrefix(ts.URL, "http") + u.Path + "?awps_connection_id=" + connected["connectionId"] + "&awps_reconnection_token=" + url.QueryEscape(connected["reconnectionToken"])
	recovered, _, err := dialer.Dial(reconnect, nil)
	if err != nil {
		t.Fatal(err)
	}
	recovered.Close()
	for _, ticket := range []string{s.issue(1, "entity"), s.socketTicket(1, connected["connectionId"], time.Now().Add(-time.Minute).Unix()), u.Query().Get("access_token") + "x"} {
		if _, _, ok := s.socketIdentity(ticket); ok {
			t.Fatal("invalid socket ticket accepted")
		}
	}
}
