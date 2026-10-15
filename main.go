package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type server struct{ key []byte }
type claims struct {
	PID  uint64
	Exp  int64
	Kind string
}

func (s server) sign(prefix, payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(prefix + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// The unsigned outer JWT only carries the independently signed Nextendo credential.
func (s server) identity(jwt string) (uint64, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("invalid carrier")
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return 0, e
	}
	var carrier struct {
		NNEX string `json:"nnex"`
	}
	if e = json.Unmarshal(raw, &carrier); e != nil {
		return 0, e
	}
	token := strings.Split(carrier.NNEX, ".")
	if len(token) != 3 || token[0] != "nx2" {
		return 0, fmt.Errorf("signed identity required")
	}
	raw, e = base64.RawURLEncoding.DecodeString(token[1])
	if e != nil {
		return 0, e
	}
	if !hmac.Equal([]byte(s.sign("nex:", string(raw))), []byte(token[2])) {
		return 0, fmt.Errorf("invalid signature")
	}
	fields := strings.SplitN(string(raw), ".", 3)
	if len(fields) != 3 {
		return 0, fmt.Errorf("invalid identity")
	}
	pid, e := strconv.ParseUint(fields[0], 10, 64)
	if e != nil || pid == 0 {
		return 0, fmt.Errorf("invalid player")
	}
	exp, e := strconv.ParseInt(fields[2], 10, 64)
	if e != nil || exp <= time.Now().Unix() {
		return 0, fmt.Errorf("expired identity")
	}
	return pid, nil
}
func (s server) issue(pid uint64, kind string) string {
	b, _ := json.Marshal(claims{pid, time.Now().Add(time.Hour).Unix(), kind})
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + s.sign("dungeons-ii:", p)
}
func (s server) entitlementToken(pid uint64) string {
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"HS256","typ":"JWT"}`))
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{"sub": strconv.FormatUint(pid, 10), "iat": now.Unix(), "exp": now.Add(30 * 24 * time.Hour).Unix(), "entitlements": []string{}})
	body := header + "." + enc(payload)
	return body + "." + s.sign("dungeons-ii-entitlements:", body)
}
func (s server) verify(t string) (uint64, bool) {
	c, ok := s.claimsOf(t)
	if !ok || (c.Kind != "session" && c.Kind != "entity") {
		return 0, false
	}
	notePresence(c.PID) // seul point par ou passe tout pid authentifie : voir stats.go
	return c.PID, true
}

// claimsOf checks a token's signature and expiry; callers check its kind.
func (s server) claimsOf(t string) (claims, bool) {
	var c claims
	p := strings.Split(t, ".")
	if len(p) != 2 || !hmac.Equal([]byte(s.sign("dungeons-ii:", p[0])), []byte(p[1])) {
		return c, false
	}
	b, e := base64.RawURLEncoding.DecodeString(p[0])
	if e != nil || json.Unmarshal(b, &c) != nil || c.PID == 0 || c.Exp <= time.Now().Unix() {
		return c, false
	}
	return c, true
}

func encodeClaims(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func (s server) entity(pid uint64) any {
	return map[string]any{"EntityToken": s.issue(pid, "entity"), "TokenExpiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "Entity": map[string]string{"Id": fmt.Sprintf("%016X", pid), "Type": "title_player_account"}}
}
func reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func failure(w http.ResponseWriter, status int, name string) {
	reply(w, status, map[string]any{"code": status, "status": http.StatusText(status), "error": name, "errorCode": status, "errorMessage": name})
}
func (s server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if i := strings.Index(path, "/id/"); i >= 0 {
		path = path[:i] + "/id/<redacted>"
	}
	log.Printf("request method=%s path=%q", r.Method, path)
	if isXboxHost(r.Host) {
		s.xboxRequest(w, r)
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		reply(w, 200, map[string]string{"status": "ok", "phase": "authentication"})
		return
	}
	if s.statsRequest(w, r) {
		return
	}
	if s.hostingRequest(w, r) || s.websocketRequest(w, r) || s.blobRequest(w, r) || s.characterRequest(w, r) || s.gameSessionRequest(w, r) || s.partyRequest(w, r) {
		return
	}
	// /spicewoodconfig/... is the hosting game server's path for the same schedule.
	if r.Method == "GET" && (r.URL.Path == "/spicewood/config/bom/schedule" || r.URL.Path == "/spicewoodconfig/bom/schedule") {
		// Online Data Tables schedule: no server-side config overrides, so the game keeps its
		// built-in tables. Each entry would be {id, bomId, name, namespace, clientVersion, activeFrom}.
		reply(w, 200, map[string]any{"bomSchedules": []any{}})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/spicewood/profiles/inventory" {
		// Both are string arrays of owned item ids; nothing is granted server-side yet.
		reply(w, 200, map[string]any{"defaultInventory": []string{}, "premiumInventory": []string{}})
		return
	}
	if r.Method != "POST" {
		b, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<14))
		log.Printf("unimplemented %s request body=%q", r.Method, b) // TEMP
		failure(w, 405, "MethodNotAllowed")
		return
	}
	switch r.URL.Path {
	case "/login/playfab":
		var q struct {
			PlayfabToken string `json:"playfabToken"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q) != nil {
			failure(w, 400, "InvalidRequest")
			return
		}
		pid, ok := s.verify(q.PlayfabToken)
		if !ok {
			log.Print("Minecraft login rejected: invalid PlayFab credential")
			failure(w, 401, "InvalidSessionTicket")
			return
		}
		log.Print("Minecraft login accepted")
		if socketFor(pid) == nil {
			newPartySession(pid)
		}
		reply(w, 200, map[string]any{"userName": minecraftUserName(pid), "access_token": s.issue(pid, "entity"), "expires_in": 3600, "user_hash": fmt.Sprintf("%d", pid)})
	case "/Client/LoginWithNintendoServiceAccount":
		log.Print("Nintendo login request received")
		var q struct {
			IdentityToken string
			TitleId       string
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q) != nil {
			log.Print("Nintendo login rejected: invalid JSON")
			failure(w, 400, "InvalidRequest")
			return
		}
		if q.TitleId != "83156" {
			log.Print("Nintendo login rejected: title mismatch")
			failure(w, 400, "InvalidTitleId")
			return
		}
		pid, e := s.identity(q.IdentityToken)
		if e != nil {
			log.Printf("Nintendo login rejected: %v", e)
			failure(w, 401, "InvalidIdentityToken")
			return
		}
		account := map[string]any{"PlayFabId": fmt.Sprintf("%016X", pid)}
		// The game compares this with its signed-in Xbox user and relinks after every login
		// when it is missing.
		characterLock.Lock()
		profile, _ := readProfile(pid)
		characterLock.Unlock()
		if profile.XboxUserID != "" {
			account["XboxInfo"] = map[string]any{"XboxUserId": profile.XboxUserID, "XboxUserSandbox": "RETAIL"}
		}
		reply(w, 200, map[string]any{"code": 200, "status": "OK", "data": map[string]any{"PlayFabId": fmt.Sprintf("%016X", pid), "SessionTicket": s.issue(pid, "session"), "EntityToken": s.entity(pid), "NewlyCreated": false, "InfoResultPayload": map[string]any{"AccountInfo": account}, "SettingsForUser": map[string]bool{"NeedsAttribution": false, "GatherDeviceInfo": false, "GatherFocusInfo": false}}})
	case "/spicewood/entitlement/nintendo":
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		pid, valid := s.verify(token)
		if !ok || !valid {
			failure(w, 401, "InvalidSessionTicket")
			return
		}
		// The client stores this as entitlements.jwt.bin and never reads it back, so it carries
		// only what our own server would need to trust it later.
		reply(w, 200, map[string]any{"entitlements": s.entitlementToken(pid)})
	case "/account/nintendo/link", "/account/nintendo/unlink":
		// Links the Nextendo-issued Xbox identity (playfabXblToken, mapiXblToken, nsaIdToken) to
		// this player. Both identities are ours, so the link always succeeds.
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		pid, valid := s.verify(token)
		if !ok || !valid {
			failure(w, 401, "InvalidSessionTicket")
			return
		}
		var q struct {
			PlayfabXblToken string `json:"playfabXblToken"`
			MapiXblToken    string `json:"mapiXblToken"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q)
		xuid, gamertag := "", ""
		if r.URL.Path == "/account/nintendo/link" {
			var ok bool
			if xuid, gamertag, ok = s.xboxUserFromToken(q.MapiXblToken); !ok {
				if xuid, gamertag, ok = s.xboxUserFromToken(q.PlayfabXblToken); !ok {
					failure(w, 400, "InvalidMapiXblToken")
					return
				}
			}
		}
		characterLock.Lock()
		p, e := readProfile(pid)
		if e == nil {
			p.XboxUserID, p.XboxGamertag = xuid, gamertag
			e = writeProfile(pid, p)
		}
		characterLock.Unlock()
		if e != nil {
			failure(w, 500, "CharacterStorageError")
			return
		}
		reply(w, 200, map[string]any{})
	case "/inbox/messages":
		// ULobbyMessageSubsystem reads messages.TAKEOVER as a list of {messageId, languageCode,
		// content}; anything else is "Received error response" / "Could not parse response body".
		reply(w, 200, map[string]any{"messages": map[string]any{"TAKEOVER": []any{}}})
	case "/Client/LinkXboxAccount", "/Client/UnlinkXboxAccount":
		// The Xbox identity is Nextendo's own (see xbox.go), so linking it always succeeds.
		reply(w, 200, map[string]any{"code": 200, "status": "OK", "data": map[string]any{}})
	case "/MultiplayerServer/ListQosServersForTitle":
		reply(w, 200, map[string]any{"code": 200, "status": "OK", "data": map[string]any{"PageSize": 1, "QosServers": []any{map[string]string{"Region": "NorthEurope", "ServerUrl": "vex.minecraftservices.com"}}}})
	case "/Authentication/GetEntityToken":
		token := r.Header.Get("X-EntityToken")
		if token == "" {
			token = r.Header.Get("X-Authorization")
		}
		pid, ok := s.verify(token)
		if !ok {
			failure(w, 401, "InvalidSessionTicket")
			return
		}
		reply(w, 200, map[string]any{"code": 200, "status": "OK", "data": s.entity(pid)})
	default:
		// Do not record paths with player identifiers, query strings, headers or bodies.
		if strings.HasPrefix(r.URL.Path, "/Client/") || strings.HasPrefix(r.URL.Path, "/Authentication/") || strings.HasPrefix(r.URL.Path, "/MultiplayerServer/") {
			log.Printf("unimplemented PlayFab API: %q", r.URL.Path)
		} else {
			b, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<14))
			log.Printf("unimplemented Minecraft API request body=%q", b) // TEMP
		}
		failure(w, 501, "NotImplemented")
	}
}
func main() {
	b, e := os.ReadFile(os.Getenv("NEXTENDO_SECRET_FILE"))
	if e != nil {
		log.Fatal("cannot read identity key file")
	}
	key, e := hex.DecodeString(strings.TrimSpace(string(b)))
	if e != nil || len(key) < 16 {
		log.Fatal("invalid identity key file")
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8186"
	}
	partyCodeKey = key
	startRelay()
	h := &http.Server{Addr: addr, Handler: server{key}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("Dungeons II authentication service listening on %s", addr)
	log.Fatal(h.ListenAndServe())
}
