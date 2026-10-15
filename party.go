package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// minecraftUserName is the player's Minecraft API user name (the /login/playfab "userName"); the
// game builds its SPICEWOOD net id from it and finds itself in party member lists by it.
func minecraftUserName(pid uint64) string { return fmt.Sprintf("Nextendo%d", pid) }

var (
	accountURL  = os.Getenv("NEXTENDO_ACCOUNT_URL")
	internalKey = os.Getenv("NEXTENDO_INTERNAL_KEY")
	accountHTTP = &http.Client{Timeout: 3 * time.Second}
	namesMu     sync.Mutex
	names       = map[uint64]cachedName{}
)

type cachedName struct {
	name string
	at   time.Time
}

// nextendoName is the player's Nextendo account name from nextendo-account, shown to other
// players instead of the generated Xbox gamertag.
func nextendoName(pid uint64) string {
	namesMu.Lock()
	c, ok := names[pid]
	namesMu.Unlock()
	if ok && time.Since(c.at) < 10*time.Minute {
		return c.name
	}
	name := minecraftUserName(pid)
	if accountURL != "" {
		if req, err := http.NewRequest("GET", accountURL+"/internal/resolve?pid="+strconv.FormatUint(pid, 10), nil); err == nil {
			req.Header.Set("X-Internal-Key", internalKey)
			if res, err := accountHTTP.Do(req); err == nil {
				var out struct{ Nickname string }
				if res.StatusCode == 200 && json.NewDecoder(res.Body).Decode(&out) == nil && out.Nickname != "" {
					name = out.Nickname
				}
				res.Body.Close()
			}
		}
	}
	namesMu.Lock()
	names[pid] = cachedName{name, time.Now()}
	namesMu.Unlock()
	return name
}

const (
	maxPartySize = 4
	// A member whose websocket closes is kept this long, so switching heroes (a re-login) keeps
	// them in the party.
	partyReconnectGrace = time.Minute
	codeAlphabet        = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// party is a Minecraft API party. Every player is in exactly one, starting with their own;
// Type is EUpdateSessionType (PUBLIC, FRIENDS or PRIVATE) and Version increases with every change.
type party struct {
	ID, Code, Type string
	Owner          uint64
	Members        []uint64
	Version        int
}

var (
	partiesMu sync.Mutex
	parties   = map[string]*party{}
	memberOf  = map[uint64]string{}
	// lastType keeps the type a player chose for their own party across new parties.
	lastType = map[uint64]string{}
)

func randomPartyID() string {
	var b [16]byte
	rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newPartyCodeLocked() string {
	for {
		var b [6]byte
		rand.Read(b[:])
		for i := range b {
			b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
		}
		if partyByCodeLocked(string(b[:])) == nil {
			return string(b[:])
		}
	}
}

// codeLookalikes folds characters players misread from a shown party code (a console player typed
// "2223-MG" for a code with a Z) onto one form, so either spelling finds the party.
var codeLookalikes = strings.NewReplacer("Z", "2", "B", "8", "S", "5", "V", "U", "O", "0", "I", "1", "L", "1")

func canonicalCode(code string) string {
	return codeLookalikes.Replace(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", "")))
}

func partyByCodeLocked(code string) *party {
	code = canonicalCode(code)
	for _, p := range parties {
		if canonicalCode(p.Code) == code {
			return p
		}
	}
	return nil
}

// partyOfLocked returns pid's party, giving pid a new one-player party if it has none.
func partyOfLocked(pid uint64) *party {
	if p := parties[memberOf[pid]]; p != nil && slices.Contains(p.Members, pid) {
		return p
	}
	t := lastType[pid]
	if t == "" {
		t = "PUBLIC"
	}
	p := &party{ID: randomPartyID(), Code: playerPartyCodeLocked(pid), Type: t, Owner: pid, Members: []uint64{pid}, Version: 1}
	parties[p.ID] = p
	memberOf[pid] = p.ID
	log.Printf("party: %d has new party %s, code %s", pid, p.ID, p.Code)
	return p
}

// partyCodeKey is set at startup; it keeps players' codes stable without making them guessable.
var partyCodeKey []byte

// partySessions holds each player's code seed for the current game session (guarded by partiesMu).
var partySessions = map[uint64][16]byte{}

// newPartySession starts a game session for pid: a new code seed, and a new party (so a new code)
// if pid is alone in its party. Called when the game logs in with no party connection open, i.e.
// after a launch; token refreshes during play keep the code.
func newPartySession(pid uint64) {
	var seed [16]byte
	rand.Read(seed[:])
	partiesMu.Lock()
	defer partiesMu.Unlock()
	partySessions[pid] = seed
	if p := parties[memberOf[pid]]; p != nil && len(p.Members) == 1 && p.Members[0] == pid {
		delete(parties, p.ID)
		delete(memberOf, pid)
	}
}

// playerPartyCodeLocked gives pid the same code for every party of its own during one game session:
// the game keeps showing the code it received when it started, so a new code mid-session would make
// the one on screen fail ("Unable to join Party"). Falls back to a random code if another party has it.
func playerPartyCodeLocked(pid uint64) string {
	seed, ok := partySessions[pid]
	if !ok {
		rand.Read(seed[:])
		partySessions[pid] = seed
	}
	mac := hmac.New(sha256.New, partyCodeKey)
	fmt.Fprintf(mac, "party-code:%d:", pid)
	mac.Write(seed[:])
	sum := mac.Sum(nil)
	var b [6]byte
	for i := range b {
		b[i] = codeAlphabet[int(sum[i])%len(codeAlphabet)]
	}
	if partyByCodeLocked(string(b[:])) != nil {
		return newPartyCodeLocked()
	}
	return string(b[:])
}

// removeMemberLocked takes pid out of p; the first remaining member becomes owner. It reports
// whether the owner changed.
func removeMemberLocked(p *party, pid uint64) (ownerChanged bool) {
	p.Members = slices.DeleteFunc(p.Members, func(m uint64) bool { return m == pid })
	if memberOf[pid] == p.ID {
		delete(memberOf, pid)
	}
	p.Version++
	if len(p.Members) == 0 {
		delete(parties, p.ID)
		return false
	}
	if p.Owner == pid {
		p.Owner = p.Members[0]
		return true
	}
	return false
}

// partyJSON is the Party object the Minecraft API sends; members' positions follow join order and
// are 1-based: the game maps position N to party slot N (SW.UI.Party.Slot.SlotN).
func (s server) partyJSON(p *party) map[string]any {
	members := []any{}
	for i, pid := range p.Members {
		members = append(members, s.memberJSON(pid, i+1))
	}
	return map[string]any{
		"id": p.ID, "ownerId": minecraftUserName(p.Owner), "type": p.Type, "code": p.Code,
		"partyMembers": members, "version": p.Version, "crossPlayEnabled": true,
	}
}

func (s server) memberJSON(pid uint64, position int) map[string]any {
	characterLock.Lock()
	prof, _ := readProfile(pid)
	characterLock.Unlock()
	name := minecraftUserName(pid)
	display := nextendoName(pid)
	member := map[string]any{
		"id": name, "userId": name, "profileId": strconv.FormatUint(pid, 10), "hostId": "",
		// EPlatformType without its E_PLATFORM_ prefix; anything else renders a broken icon.
		"position": position, "platform": "SWITCH", "platformId": strconv.FormatUint(pid, 10),
		"profileNames": map[string]string{"platformName": display, "publisherName": display, "msaName": display},
		"status":       "CONNECTED",
	}
	if c, ok := prof.Characters[prof.Primary]; ok {
		loadout := []any{}
		if c.Skin != "" {
			loadout = append(loadout, map[string]string{"type": "SW.Skin", "value": c.Skin})
		}
		// Other members read this hero's save from sasUrl for its campfire stats.
		member["characterData"] = map[string]any{"characterId": c.ID, "loadout": loadout, "sasUrl": s.characterReadLink(pid, c)}
	}
	return member
}

// partyFor is the party pid is in, as sent to clients. The login flow waits
// (UPartySigninLoginState) for it as Party/PartyCreated and fails with "Failed to retrieve a party
// on login" if none arrives.
func (s server) partyFor(pid uint64) map[string]any {
	partiesMu.Lock()
	p := *partyOfLocked(pid)
	p.Members = slices.Clone(p.Members)
	partiesMu.Unlock()
	return s.partyJSON(&p)
}

// notify sends a Party event about the players in about to every member of p except skip.
func (s server) notify(p party, event string, about []uint64, skip uint64) {
	ids := []string{}
	for _, pid := range about {
		ids = append(ids, minecraftUserName(pid))
	}
	body := s.partyJSON(&p)
	for _, m := range p.Members {
		if m == skip {
			continue
		}
		if sock := socketFor(m); sock != nil {
			sock.party(event, body, ids)
		}
	}
}

// partyRequest serves the Minecraft API party calls:
//   - PUT /spicewood/parties/<id>/type {"newPartyType": "FRIENDS"}: the owner changes who may join.
//   - PUT /spicewood/parties/<id>/join: join from an invite.
//   - PUT /spicewood/parties/join/code {"code": "ABC123"}: join by party code.
//   - DELETE /spicewood/parties/<id>/partymembers: leave (also sent when switching to an offline hero).
func (s server) partyRequest(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/spicewood/parties/")
	id, action, _ := strings.Cut(rest, "/")
	byCode := rest == "join/code" && (r.Method == "PUT" || r.Method == "POST")
	if !ok || !(byCode || r.Method == "PUT" && (action == "type" || action == "join") || r.Method == "DELETE" && action == "partymembers") {
		return false
	}
	pid, valid := s.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !valid {
		log.Printf("party: %s %s rejected, invalid session token", r.Method, r.URL.Path)
		failure(w, 401, "InvalidSessionTicket")
		return true
	}
	switch {
	case byCode || action == "join":
		code := ""
		if byCode {
			var q struct{ Code string }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil || q.Code == "" {
				log.Printf("party: %d join by code rejected, unreadable body", pid)
				failure(w, 400, "InvalidRequest")
				return true
			}
			code = q.Code
		}
		s.joinParty(w, pid, id, code)
	case action == "partymembers":
		partiesMu.Lock()
		p := parties[id]
		if p == nil || !slices.Contains(p.Members, pid) {
			partiesMu.Unlock()
			failure(w, 404, "PARTY_NOT_FOUND")
			return true
		}
		ownerChanged := removeMemberLocked(p, pid)
		left := *p
		left.Members = slices.Clone(p.Members)
		partiesMu.Unlock()
		w.WriteHeader(204)
		s.announceLeave(left, pid, ownerChanged)
		// The leaver is back in a party of their own, as at login.
		if sock := socketFor(pid); sock != nil {
			time.AfterFunc(500*time.Millisecond, func() {
				sock.party("PartyCreated", s.partyFor(pid), []string{minecraftUserName(pid)})
			})
		}
		log.Printf("party: %d left %s", pid, id)
	default:
		var q struct {
			NewPartyType string `json:"newPartyType"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil || (q.NewPartyType != "PUBLIC" && q.NewPartyType != "FRIENDS" && q.NewPartyType != "PRIVATE") {
			failure(w, 400, "InvalidRequest")
			return true
		}
		partiesMu.Lock()
		p := parties[id]
		if p == nil || p.Owner != pid {
			partiesMu.Unlock()
			failure(w, 403, "NOT_OWNER_OF_PARTY")
			return true
		}
		p.Type = q.NewPartyType
		p.Version++
		lastType[pid] = p.Type
		changed := *p
		changed.Members = slices.Clone(p.Members)
		partiesMu.Unlock()
		s.notify(changed, "TypeUpdated", []uint64{pid}, 0)
		log.Printf("party %d type -> %s", pid, q.NewPartyType)
		reply(w, 200, map[string]any{})
	}
	return true
}

// joinParty moves pid into the party with the given id (an invite) or code. The joiner is told
// with Party/SelfJoined, the members already there with Party/PlayerJoined.
func (s server) joinParty(w http.ResponseWriter, pid uint64, id, code string) {
	partiesMu.Lock()
	target := parties[id]
	if code != "" {
		target = partyByCodeLocked(code)
	}
	if target == nil {
		partiesMu.Unlock()
		log.Printf("party: %d join failed, no party for id %q code %q", pid, id, code)
		failure(w, 404, "PARTY_NOT_FOUND")
		return
	}
	if slices.Contains(target.Members, pid) {
		joined := *target
		joined.Members = slices.Clone(target.Members)
		partiesMu.Unlock()
		reply(w, 200, s.partyJSON(&joined))
		return
	}
	if len(target.Members) >= maxPartySize {
		partiesMu.Unlock()
		failure(w, 409, "PARTY_FULL")
		return
	}
	var left *party
	var ownerChanged bool
	if old := parties[memberOf[pid]]; old != nil && slices.Contains(old.Members, pid) {
		ownerChanged = removeMemberLocked(old, pid)
		l := *old
		l.Members = slices.Clone(old.Members)
		left = &l
	}
	target.Members = append(target.Members, pid)
	target.Version++
	memberOf[pid] = target.ID
	joined := *target
	joined.Members = slices.Clone(target.Members)
	partiesMu.Unlock()

	if left != nil {
		s.announceLeave(*left, pid, ownerChanged)
	}
	body := s.partyJSON(&joined)
	reply(w, 200, body)
	if sock := socketFor(pid); sock != nil {
		sock.party("SelfJoined", body, []string{minecraftUserName(pid)})
	}
	s.notify(joined, "PlayerJoined", []uint64{pid}, pid)
	log.Printf("party: %d joined %s (%d members)", pid, joined.ID, len(joined.Members))
	s.lateJoiner(joined.ID, pid)
}

// announceLeave tells the remaining members of p that pid left.
func (s server) announceLeave(p party, pid uint64, ownerChanged bool) {
	if len(p.Members) == 0 {
		return
	}
	s.notify(p, "PlayerLeave", []uint64{pid}, 0)
	if ownerChanged {
		s.notify(p, "OwnerUpdated", []uint64{p.Owner}, 0)
	}
}

// partyDisconnected removes pid from a shared party if it has not reconnected within the grace
// period after its websocket closed.
func (s server) partyDisconnected(pid uint64) {
	time.AfterFunc(partyReconnectGrace, func() {
		if socketFor(pid) != nil {
			return
		}
		partiesMu.Lock()
		p := parties[memberOf[pid]]
		if p == nil || len(p.Members) < 2 {
			partiesMu.Unlock()
			return
		}
		ownerChanged := removeMemberLocked(p, pid)
		left := *p
		left.Members = slices.Clone(p.Members)
		partiesMu.Unlock()
		s.announceLeave(left, pid, ownerChanged)
		log.Printf("party: %d dropped from %s after disconnect", pid, left.ID)
	})
}

// partyMessage wraps a Party topic event as a Web PubSub server message; body is a JSON string.
func partyMessage(seq uint64, event string, party map[string]any, memberIDs []string) map[string]any {
	return partyEventMessage(seq, event, map[string]any{"updatePartyMemberIds": memberIDs, "party": party})
}

func partyEventMessage(seq uint64, event string, fields map[string]any) map[string]any {
	body, _ := json.Marshal(fields)
	return map[string]any{
		"type": "message", "from": "server", "dataType": "json", "sequenceId": seq,
		"data": map[string]any{"topic": "Party", "event": event, "body": string(body)},
	}
}
