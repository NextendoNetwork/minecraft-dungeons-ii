package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type partyEvent struct {
	Event   string
	Members []string
	Party   struct {
		ID, OwnerID, Type, Code string
		PartyMembers            []struct{ ID string }
	}
}

func connectClient(t *testing.T, s server, ts *httptest.Server, pid uint64) *websocket.Conn {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/websocket/client/negotiate", nil)
	req.Header.Set("Authorization", "Bearer "+s.issue(pid, "entity"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ URL string }
	json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	u, _ := url.Parse(body.URL)
	conn, _, err := (&websocket.Dialer{Subprotocols: []string{clientProtocol}}).Dial("ws"+strings.TrimPrefix(ts.URL, "http")+u.RequestURI(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var connected map[string]string
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if conn.ReadJSON(&connected) != nil || connected["event"] != "connected" {
		t.Fatal("not connected", connected)
	}
	return conn
}

// nextEvent returns the next Party event, skipping repeated PartyCreated pushes unless wanted.
func nextEvent(t *testing.T, conn *websocket.Conn, want string) partyEvent {
	t.Helper()
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var msg struct {
			Data struct{ Topic, Event, Body string }
		}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("waiting for %s: %v", want, err)
		}
		if msg.Data.Event == "PartyCreated" && want != "PartyCreated" {
			continue
		}
		var body struct {
			UpdatePartyMemberIds []string
			Party                json.RawMessage
		}
		json.Unmarshal([]byte(msg.Data.Body), &body)
		e := partyEvent{Event: msg.Data.Event, Members: body.UpdatePartyMemberIds}
		json.Unmarshal(body.Party, &e.Party)
		if e.Event != want {
			t.Fatalf("got %s, want %s", e.Event, want)
		}
		return e
	}
}

func call(t *testing.T, s server, ts *httptest.Server, pid uint64, method, path, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.issue(pid, "entity"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// The invite flow two players ran on 2026-10-01: the host invites, the guest's game calls
// PUT /spicewood/parties/<host party>/join and both games wait for the party events.
func TestPartyInviteJoinTypeAndLeave(t *testing.T) {
	t.Setenv("CHARACTER_DATA_DIR", t.TempDir())
	s := server{[]byte("party-test-key")}
	ts := httptest.NewServer(s)
	defer ts.Close()
	const host, guest = 11, 22
	hc := connectClient(t, s, ts, host)
	defer hc.Close()
	gc := connectClient(t, s, ts, guest)
	defer gc.Close()
	hostParty := nextEvent(t, hc, "PartyCreated").Party
	guestParty := nextEvent(t, gc, "PartyCreated").Party
	if hostParty.ID == guestParty.ID || hostParty.Code == "" || len(hostParty.PartyMembers) != 1 {
		t.Fatal("bad starting parties", hostParty, guestParty)
	}

	if code := call(t, s, ts, guest, "PUT", "/spicewood/parties/"+hostParty.ID+"/join", ""); code != 200 {
		t.Fatal("join failed", code)
	}
	self := nextEvent(t, gc, "SelfJoined")
	joined := nextEvent(t, hc, "PlayerJoined")
	for _, e := range []partyEvent{self, joined} {
		if e.Party.ID != hostParty.ID || len(e.Party.PartyMembers) != 2 || e.Party.OwnerID != minecraftUserName(host) || e.Members[0] != minecraftUserName(guest) {
			t.Fatalf("bad join event %+v", e)
		}
	}

	if code := call(t, s, ts, guest, "PUT", "/spicewood/parties/"+hostParty.ID+"/type", `{"newPartyType":"PRIVATE"}`); code != 403 {
		t.Fatal("guest changed the party type", code)
	}
	if code := call(t, s, ts, host, "PUT", "/spicewood/parties/"+hostParty.ID+"/type", `{"newPartyType":"FRIENDS"}`); code != 200 {
		t.Fatal("type change failed", code)
	}
	for _, c := range []*websocket.Conn{hc, gc} {
		if e := nextEvent(t, c, "TypeUpdated"); e.Party.Type != "FRIENDS" {
			t.Fatal("type not sent", e)
		}
	}

	if code := call(t, s, ts, guest, "DELETE", "/spicewood/parties/"+hostParty.ID+"/partymembers", ""); code != 204 {
		t.Fatal("leave failed", code)
	}
	if e := nextEvent(t, hc, "PlayerLeave"); len(e.Party.PartyMembers) != 1 || e.Members[0] != minecraftUserName(guest) {
		t.Fatal("bad leave event", e)
	}
	if e := nextEvent(t, gc, "PartyCreated"); e.Party.ID == hostParty.ID || len(e.Party.PartyMembers) != 1 {
		t.Fatal("leaver has no party of their own", e)
	}

	// Joining by the code shown in the party menu, in any case and with a dash.
	code := strings.ToLower(hostParty.Code[:3] + "-" + hostParty.Code[3:])
	if status := call(t, s, ts, guest, "PUT", "/spicewood/parties/join/code", `{"code":"`+code+`"}`); status != 200 {
		t.Fatal("join by code failed", status)
	}
	if e := nextEvent(t, gc, "SelfJoined"); e.Party.ID != hostParty.ID || e.Party.Type != "FRIENDS" {
		t.Fatal("bad join by code", e)
	}
	if call(t, s, ts, guest, "PUT", "/spicewood/parties/join/code", `{"code":"ZZZZZZ"}`) != 404 {
		t.Fatal("unknown code accepted")
	}
}

func TestPartyCodeLookalikes(t *testing.T) {
	partiesMu.Lock()
	defer partiesMu.Unlock()
	p := &party{ID: "code-test", Code: "Z2B8SV"}
	parties[p.ID] = p
	defer delete(parties, p.ID)
	for _, typed := range []string{"Z2B8SV", "2288-5U", "z2b8sv", " ZZ88SU "} {
		if partyByCodeLocked(typed) != p {
			t.Errorf("%q did not find the party", typed)
		}
	}
	if partyByCodeLocked("Z2B8SW") != nil {
		t.Error("a different code matched")
	}
}

func TestPlayerPartyCodeIsStable(t *testing.T) {
	partiesMu.Lock()
	defer partiesMu.Unlock()
	old := partyCodeKey
	partyCodeKey = []byte("test-key")
	defer func() { partyCodeKey = old }()
	a, b := playerPartyCodeLocked(1800000301), playerPartyCodeLocked(1800000301)
	if a != b || len(a) != 6 {
		t.Fatalf("codes %q and %q", a, b)
	}
	if playerPartyCodeLocked(1800000302) == a {
		t.Fatal("two players got the same code")
	}
	p := &party{ID: "taken", Code: a}
	parties[p.ID] = p
	defer delete(parties, p.ID)
	if playerPartyCodeLocked(1800000301) == a {
		t.Fatal("a code another party holds was reused")
	}
}

func TestNewPartySessionGivesNewCode(t *testing.T) {
	old := partyCodeKey
	partyCodeKey = []byte("test-key")
	defer func() { partyCodeKey = old }()
	const pid = 1800000077
	partiesMu.Lock()
	first := partyOfLocked(pid)
	firstCode := first.Code
	partiesMu.Unlock()

	newPartySession(pid)

	partiesMu.Lock()
	defer partiesMu.Unlock()
	if parties[first.ID] != nil {
		t.Fatal("the solo party of the previous session was kept")
	}
	second := partyOfLocked(pid)
	if second.Code == firstCode {
		t.Fatalf("new session kept code %s", firstCode)
	}
	if partyOfLocked(pid).Code != second.Code {
		t.Fatal("code changed within a session")
	}
	delete(parties, second.ID)
	delete(memberOf, pid)
	delete(partySessions, pid)
}
