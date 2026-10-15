package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Hosting uses the game's own attachable-server flow. The party leader's game runs the game
// server (host patch: IsAttachable, Overworld?listen on Start Game) and attaches its party with
// POST /spicewood/dev/parties/<id>/attach; the reply's cookie logs that server into this API with a
// token scoped to one session. Members are sent ServerReady once that server has its allowed
// players (it then fetches its tables), since its PreLogin rejects everyone before that.

const (
	gamePort      = 7777
	defaultRegion = "WestEurope"
	// hostMarker tells the leader's (patched) game to host instead of connecting.
	hostMarker      = "0.0.0.0"
	gameServerToken = "gameserver:"
	sessionTTL      = 12 * time.Hour
)

type hostedSession struct {
	ID, PartyID string
	Host        uint64
	HostIP      string
	Ready       bool
	Started     time.Time
	Allowed     map[string]bool // Minecraft user ids the server admitted
	Socket      *clientSocket   // the host server's websocket (wssUrl), once connected
}

// allowedPlayers is the GameServer/AllowedPlayers event: the server admits players only from it
// (ServerAllowClients dispatches on topic_event like the client party socket).
func allowedPlayers(sock *clientSocket, added, all []string) error {
	body, _ := json.Marshal(map[string]any{"newUserIds": added, "allUserIds": all})
	return sock.write(map[string]any{
		"type": "message", "from": "server", "dataType": "json", "sequenceId": sock.seq.Add(1),
		"data": map[string]any{"topic": "GameServer", "event": "AllowedPlayers", "body": string(body)},
	})
}

var (
	sessionsMu     sync.Mutex
	hostedSessions = map[string]*hostedSession{} // by party id
)

type sessionConfig struct {
	// LANAddresses maps a public address to the host's LAN address, for members behind the same NAT.
	LANAddresses map[string]string `json:"lanAddresses"`
}

func loadSessionConfig() sessionConfig {
	var c sessionConfig
	b, err := os.ReadFile(filepath.Join(dataDir(), "gameserver.json"))
	if err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func dataDir() string {
	if d := os.Getenv("CHARACTER_DATA_DIR"); d != "" {
		return d
	}
	return "data"
}

func clientAddress(r *http.Request) string {
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-Ip"))); ip != nil && ip.To4() != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return ""
}

func gameSessionJSON(address string, port int) map[string]any {
	return map[string]any{
		"ipV4Address": address, "region": defaultRegion,
		"ports": []any{map[string]any{"name": "GamePort", "port": port, "protocol": "UDP"}},
	}
}

// issueServer signs a token for the host's game server, valid only for one session.
func (s server) issueServer(host uint64, sessionID string) string {
	b, _ := json.Marshal(claims{host, time.Now().Add(sessionTTL).Unix(), gameServerToken + sessionID})
	p := encodeClaims(b)
	return p + "." + s.sign("dungeons-ii:", p)
}

// verifyServer returns the session a game-server token belongs to.
func (s server) verifyServer(t string) (*hostedSession, bool) {
	c, ok := s.claimsOf(t)
	if !ok || !strings.HasPrefix(c.Kind, gameServerToken) {
		return nil, false
	}
	id := strings.TrimPrefix(c.Kind, gameServerToken)
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	// A host that starts again without restarting its game keeps the earlier session's token for
	// the new session's first calls (its hero load), so a token of the host's maps to its newest
	// session.
	var found, newest *hostedSession
	for _, hs := range hostedSessions {
		if hs.Host != c.PID {
			continue
		}
		if hs.ID == id {
			found = hs
		}
		if newest == nil || hs.Started.After(newest.Started) {
			newest = hs
		}
	}
	if found == nil {
		return nil, false
	}
	return newest, true
}

// gameSessionRequest serves POST /spicewood/parties/<id>/gamesession (Start Game). The leader is
// told to host; members follow when the host's server is up.
func (s server) gameSessionRequest(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	isSession := len(parts) == 4 && parts[0] == "spicewood" && parts[1] == "parties" && parts[2] != "" && parts[3] == "gamesession"
	isMatchmaking := strings.TrimSuffix(r.URL.Path, "/") == "/spicewood/matchmaking"
	if !isSession && !isMatchmaking {
		return false
	}
	if r.Method != http.MethodPost {
		failure(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
		return true
	}
	pid, ok := s.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !ok {
		failure(w, http.StatusUnauthorized, "InvalidSessionTicket")
		return true
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if isMatchmaking {
		log.Printf("matchmaking request from %d body=%s (not yet implemented)", pid, body)
		failure(w, http.StatusServiceUnavailable, "GameServerUnavailable")
		return true
	}
	partiesMu.Lock()
	p := parties[parts[2]]
	allowed := p != nil && p.Owner == pid && memberOf[pid] == p.ID
	var snapshot party
	if allowed {
		snapshot = *p
		snapshot.Members = slices.Clone(p.Members)
	}
	partiesMu.Unlock()
	if !allowed {
		failure(w, http.StatusForbidden, "NotPartyOwner")
		return true
	}
	hs := &hostedSession{ID: randomPartyID(), PartyID: snapshot.ID, Host: pid, HostIP: clientAddress(r), Started: time.Now()}
	sessionsMu.Lock()
	hostedSessions[snapshot.ID] = hs
	sessionsMu.Unlock()
	log.Printf("session %s: Start Game by %d (public %s) body=%s", hs.ID, pid, hs.HostIP, body)
	reply(w, 200, map[string]any{})

	body2 := s.partyJSON(&snapshot)
	body2["gameSession"] = gameSessionJSON(hostMarker, gamePort)
	time.AfterFunc(200*time.Millisecond, func() {
		if sock := socketFor(pid); sock != nil && sock.party("ServerReady", body2, []string{minecraftUserName(pid)}) == nil {
			log.Printf("session %s: told host %d to start hosting", hs.ID, pid)
		}
	})
	return true
}

// hostingRequest serves the attach call from the host's game and the game-server API its server
// then uses. Unknown server calls are logged in full while the protocol is being mapped.
func (s server) hostingRequest(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	isAttach := (strings.HasPrefix(path, "/spicewood/dev/parties/") || strings.HasPrefix(path, "/dev/spicewoodpartymanager/internal/parties/")) && strings.HasSuffix(path, "/attach")
	isServerAPI := strings.HasPrefix(path, "/spicewood/multiplayerapi/") || strings.HasPrefix(path, "/spicewood/gameserver/") || path == "/spicewood/server/event" || strings.HasPrefix(path, "/nextendo/websocket/server") || path == "/spicewoodconfig/bom/schedule"
	if !isAttach && !isServerAPI {
		return false
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	hdr := []string{}
	for k, v := range r.Header {
		if k == "Authorization" {
			v = []string{fmt.Sprintf("<%d chars>", len(strings.Join(v, "")))}
		}
		hdr = append(hdr, k+"="+strings.Join(v, ","))
	}
	slices.Sort(hdr)
	log.Printf("hosting %s %s from %s headers=%s body=%s", r.Method, path, clientAddress(r), strings.Join(hdr, " "), body)
	if isAttach {
		s.attachRequest(w, r, path)
		return true
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if strings.HasPrefix(path, "/nextendo/websocket/server") {
		// The server opens the cookie's wssUrl verbatim, so its token rides in the query.
		token = r.URL.Query().Get("access_token")
	}
	hs, ok := s.verifyServer(token)
	if !ok && path == "/spicewoodconfig/bom/schedule" {
		reply(w, 200, map[string]any{"bomSchedules": []any{}})
		return true
	}
	if !ok {
		failure(w, http.StatusUnauthorized, "InvalidServerToken")
		return true
	}
	switch path {
	case "/nextendo/websocket/server":
		s.serverSocket(w, r, hs)
	case "/spicewood/multiplayerapi/gamesessions/server/init":
		// The server reads {"party": ...} (same shape as party events) into its session state.
		party, ok := s.sessionParty(hs)
		if !ok {
			failure(w, http.StatusNotFound, "PartyNotFound")
			return true
		}
		reply(w, 200, map[string]any{"party": party})
		// A host that hosted before in the same game run logs in without fetching the BOM
		// schedule again, so nothing below would send its members: admit them over the server's
		// socket instead, as for a late joiner.
		time.AfterFunc(5*time.Second, func() { s.readyWithoutSchedule(hs) })
	case "/spicewoodconfig/bom/schedule":
		// ServerFetchTablesState follows ServerSetAllowedPlayersState, so the host now admits the
		// party (its PreLogin rejects everyone until then). Members travel once the login completes.
		reply(w, 200, map[string]any{"bomSchedules": []any{}})
		time.AfterFunc(time.Second, func() { s.hostReady(hs) })
	case "/spicewood/multiplayerapi/gamesessions/partymembers/allowed":
		s.allowRequest(w, hs, body)
	default:
		if id, ok := strings.CutPrefix(path, "/spicewood/gameserver/characters/id/"); ok {
			s.serverCharacter(w, hs, id)
			return true
		}
		reply(w, 200, map[string]any{})
	}
	return true
}

// serverCharacter serves a party member's hero to the host's server, in the client character
// shape (its sasUrl carries the save). Only heroes of the session's party are visible.
func (s server) serverCharacter(w http.ResponseWriter, hs *hostedSession, id string) {
	partiesMu.Lock()
	var members []uint64
	if p := parties[hs.PartyID]; p != nil {
		members = slices.Clone(p.Members)
	}
	partiesMu.Unlock()
	characterLock.Lock()
	defer characterLock.Unlock()
	for _, pid := range members {
		if prof, err := readProfile(pid); err == nil {
			if c, ok := prof.Characters[id]; ok {
				if pid == hs.Host {
					reply(w, 200, s.characterResponse(pid, c))
				} else {
					reply(w, 200, s.guestCharacterResponse(pid, c))
				}
				return
			}
		}
	}
	failure(w, http.StatusNotFound, "CharacterNotFound")
}

// sessionParty is the session's party as the host's server sees it, with its game session.
func (s server) sessionParty(hs *hostedSession) (map[string]any, bool) {
	partiesMu.Lock()
	p := parties[hs.PartyID]
	var snapshot party
	if p != nil {
		snapshot = *p
		snapshot.Members = slices.Clone(p.Members)
	}
	partiesMu.Unlock()
	if p == nil {
		return nil, false
	}
	body := s.partyJSON(&snapshot)
	body["gameSession"] = gameSessionJSON(hs.HostIP, gamePort)
	return body, true
}

// allowRequest serves POST partymembers/allowed {userIds, sessionId}: the server may admit only
// the session's party members. Reply {newUserIds, allUserIds}.
func (s server) allowRequest(w http.ResponseWriter, hs *hostedSession, body []byte) {
	var req struct {
		UserIDs []string `json:"userIds"`
	}
	if json.Unmarshal(body, &req) != nil {
		failure(w, http.StatusBadRequest, "InvalidBody")
		return
	}
	members := map[string]bool{}
	partiesMu.Lock()
	if p := parties[hs.PartyID]; p != nil {
		for _, m := range p.Members {
			members[minecraftUserName(m)] = true
		}
	}
	partiesMu.Unlock()
	sessionsMu.Lock()
	if hs.Allowed == nil {
		hs.Allowed = map[string]bool{}
	}
	added := []string{}
	for _, id := range req.UserIDs {
		if members[id] && !hs.Allowed[id] {
			hs.Allowed[id] = true
			added = append(added, id)
		}
	}
	all := []string{}
	for id := range hs.Allowed {
		all = append(all, id)
	}
	sock := hs.Socket
	sessionsMu.Unlock()
	slices.Sort(all)
	log.Printf("session %s: allowed %v (all %v)", hs.ID, added, all)
	// The HTTP reply body is ignored by the game; the list goes over the server websocket (or
	// is sent when it connects).
	reply(w, 200, map[string]any{"newUserIds": added, "allUserIds": all})
	if sock != nil && allowedPlayers(sock, added, all) == nil {
		log.Printf("session %s: sent GameServer/AllowedPlayers", hs.ID)
	}
}

// serverSocket is the host server's Web PubSub connection (the cookie's wssUrl). It only needs to
// stay connected for now; anything the server sends is logged.
func (s server) serverSocket(w http.ResponseWriter, r *http.Request, hs *hostedSession) {
	upgrader := websocket.Upgrader{Subprotocols: []string{clientProtocol}, HandshakeTimeout: 10 * time.Second}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("session %s: server websocket upgrade failed: %v", hs.ID, err)
		return
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
	if write(map[string]string{"type": "system", "event": "connected", "userId": hs.ID, "connectionId": randomPartyID()}) != nil {
		return
	}
	log.Printf("session %s: server websocket connected", hs.ID)
	defer log.Printf("session %s: server websocket disconnected", hs.ID)
	sock := &clientSocket{write: write}
	sessionsMu.Lock()
	hs.Socket = sock
	all := []string{}
	for id := range hs.Allowed {
		all = append(all, id)
	}
	sessionsMu.Unlock()
	defer func() {
		sessionsMu.Lock()
		if hs.Socket == sock {
			hs.Socket = nil
		}
		sessionsMu.Unlock()
	}()
	if len(all) > 0 {
		slices.Sort(all)
		allowedPlayers(sock, all, all)
	}
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
	for {
		conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		log.Printf("session %s: server websocket message %s", hs.ID, data)
		var msg struct {
			Type  string  `json:"type"`
			AckID *uint64 `json:"ackId"`
		}
		json.Unmarshal(data, &msg)
		switch {
		case msg.Type == "ping":
			write(map[string]string{"type": "pong"})
		case msg.AckID != nil:
			write(map[string]any{"type": "ack", "ackId": *msg.AckID, "success": true})
		}
	}
}

func (s server) attachRequest(w http.ResponseWriter, r *http.Request, path string) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	partyID := segs[len(segs)-2]
	from := clientAddress(r)
	sessionsMu.Lock()
	hs := hostedSessions[partyID]
	sessionsMu.Unlock()
	// The host's own game makes this call: its player token if present, else its address.
	pid, hasToken := s.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if hs == nil || (hasToken && pid != hs.Host) || (!hasToken && from != hs.HostIP) {
		log.Printf("attach refused: party=%s session=%v token=%t pid=%d from=%s", partyID, hs != nil, hasToken, pid, from)
		failure(w, http.StatusForbidden, "NotPartyHost")
		return
	}
	partiesMu.Lock()
	p := parties[partyID]
	var snapshot party
	if p != nil {
		snapshot = *p
		snapshot.Members = slices.Clone(p.Members)
	}
	partiesMu.Unlock()
	players := []string{}
	for _, m := range snapshot.Members {
		players = append(players, minecraftUserName(m))
	}
	token := s.issueServer(hs.Host, hs.ID)
	cookie := map[string]any{
		"authentication": map[string]any{"access_token": token, "refresh_token": token, "serverId": hs.ID, "expires_in": int(sessionTTL.Seconds())},
		"config":         map[string]any{"initialPlayers": players},
		"wssUrl":         "wss://vex.minecraftservices.com/nextendo/websocket/server?access_token=" + url.QueryEscape(token),
		// Required by the cookie validator; sent back as the server's s2s gateway key.
		"s2sApiKey": hs.ID,
	}
	resp := map[string]any{"cookie": cookie}
	if p != nil {
		resp["party"] = s.partyJSON(&snapshot)
	}
	log.Printf("session %s: host %d attached, initialPlayers=%v", hs.ID, hs.Host, players)
	reply(w, 200, resp)
}

// hostReady sends the other members to the host once its server has logged in.
func (s server) hostReady(hs *hostedSession) {
	sessionsMu.Lock()
	already := hs.Ready
	hs.Ready = true
	sessionsMu.Unlock()
	if already {
		return
	}
	partiesMu.Lock()
	p := parties[hs.PartyID]
	var snapshot party
	if p != nil {
		snapshot = *p
		snapshot.Members = slices.Clone(p.Members)
	}
	partiesMu.Unlock()
	if p == nil {
		return
	}
	for _, m := range snapshot.Members {
		if m != hs.Host {
			s.sendToHost(hs, &snapshot, m)
		}
	}
}

// readyWithoutSchedule admits the session's members and sends them to the host when the host's
// server logged in but never reached the BOM schedule (see server/init).
func (s server) readyWithoutSchedule(hs *hostedSession) {
	partiesMu.Lock()
	p := parties[hs.PartyID]
	var members []uint64
	if p != nil {
		members = slices.Clone(p.Members)
	}
	partiesMu.Unlock()
	all, guests := []string{}, []string{}
	for _, m := range members {
		all = append(all, minecraftUserName(m))
		if m != hs.Host {
			guests = append(guests, minecraftUserName(m))
		}
	}
	slices.Sort(all)
	sessionsMu.Lock()
	ready, sock := hs.Ready, hs.Socket
	if !ready && sock != nil {
		if hs.Allowed == nil {
			hs.Allowed = map[string]bool{}
		}
		for _, id := range all {
			hs.Allowed[id] = true
		}
	}
	sessionsMu.Unlock()
	if ready || sock == nil || len(guests) == 0 {
		return
	}
	if err := allowedPlayers(sock, guests, all); err != nil {
		log.Printf("session %s: allowing members without a BOM fetch failed: %v", hs.ID, err)
		return
	}
	log.Printf("session %s: host logged in without a BOM fetch; members allowed (all %v)", hs.ID, all)
	time.AfterFunc(time.Second, func() { s.hostReady(hs) })
}

// sendToHost sends member m ServerReady with the address to connect to: the relay whenever the
// host is pinging it (see relay.go), otherwise the host's own address (its LAN address for members
// on the same network).
func (s server) sendToHost(hs *hostedSession, p *party, m uint64) {
	if relay == nil {
		s.sendToHostNow(hs, p, m)
		return
	}
	// The host's server logs in (and members are sent) before its game port is open and pinging,
	// so wait for the pings rather than falling back to an address the host's router blocks.
	go func() {
		for deadline := time.Now().Add(relayWaitForHost); !relay.hostReachable(hs.HostIP) && time.Now().Before(deadline); {
			time.Sleep(500 * time.Millisecond)
		}
		s.sendToHostNow(hs, p, m)
	}()
}

func (s server) sendToHostNow(hs *hostedSession, p *party, m uint64) {
	sock := socketFor(m)
	if sock == nil {
		return
	}
	address, port := hs.HostIP, gamePort
	relayed := false
	if relay != nil && relay.hostReachable(hs.HostIP) {
		if rp, ok := relay.open(hs.ID, hs.HostIP); ok {
			address, port, relayed = relay.publicIP, rp, true
		}
	}
	if lan := loadSessionConfig().LANAddresses; !relayed && socketAddress(m) == hs.HostIP && lan[hs.HostIP] != "" {
		address = lan[hs.HostIP]
	}
	body := s.partyJSON(p)
	body["gameSession"] = gameSessionJSON(address, port)
	if sock.party("ServerReady", body, []string{minecraftUserName(m)}) == nil {
		log.Printf("session %s: member %d sent to host at %s:%d", hs.ID, m, address, port)
	}
}

// lateJoiner admits a member who joined the party while its host is already hosting: the host's
// server is told to allow them (it admits only its allowed list), then they are sent to the host.
func (s server) lateJoiner(partyID string, pid uint64) {
	partiesMu.Lock()
	p := parties[partyID]
	var snapshot party
	if p != nil {
		snapshot = *p
		snapshot.Members = slices.Clone(p.Members)
	}
	partiesMu.Unlock()
	if p == nil {
		return
	}
	user := minecraftUserName(pid)
	all := []string{}
	for _, m := range snapshot.Members {
		all = append(all, minecraftUserName(m))
	}
	slices.Sort(all)
	sessionsMu.Lock()
	hs := hostedSessions[partyID]
	var sock *clientSocket
	if hs != nil && hs.Ready && hs.Host != pid {
		sock = hs.Socket
		if hs.Allowed == nil {
			hs.Allowed = map[string]bool{}
		}
		for _, id := range all {
			hs.Allowed[id] = true
		}
	}
	sessionsMu.Unlock()
	if sock == nil {
		return
	}
	if err := allowedPlayers(sock, []string{user}, all); err != nil {
		log.Printf("session %s: allowing late joiner %d failed: %v", hs.ID, pid, err)
		return
	}
	log.Printf("session %s: late joiner %d allowed (all %v)", hs.ID, pid, all)
	time.AfterFunc(time.Second, func() { s.sendToHost(hs, &snapshot, pid) })
}
