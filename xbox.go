package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed xbl_endpoints.json
var xblEndpoints []byte

var titleEndpoints = func() []byte {
	var doc map[string]any
	if err := json.Unmarshal(xblEndpoints, &doc); err != nil {
		panic(err)
	}
	eps, _ := doc["EndPoints"].([]any)
	for _, host := range []string{"api.minecraftservices.com", "vex.minecraftservices.com"} {
		eps = append(eps, map[string]any{"Protocol": "https", "Host": host, "HostType": "fqdn", "RelyingParty": "rp://api.minecraftservices.com/", "TokenType": "JWT"})
	}
	doc["EndPoints"] = append(eps, map[string]any{"Protocol": "https", "Host": "*.minecraftservices.com", "HostType": "wildcard", "RelyingParty": "rp://api.minecraftservices.com/", "TokenType": "JWT"})
	out, _ := json.Marshal(doc)
	return out
}()

// Dungeons II links a Microsoft account (XAL) before Play Online; Citron redirects those hosts
// here while the game runs. Nextendo plays Microsoft's part and approves every link at once, so
// each device gets a stable Xbox identity without a real Microsoft account.
func isXboxHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	return host == "login.live.com" || host == "xboxlive.com" || strings.HasSuffix(host, ".xboxlive.com")
}

// xblPrivileges is a normal adult account's privilege list (multiplayer, communication, ...).
const xblPrivileges = "184 185 186 187 188 191 192 193 195 196 198 199 200 201 203 204 205 206 207 208 211 214 215 216 217 220 224 227 228 235 238 245 247 249 252 254 255"

type xblIdentity struct {
	xuid, uhs, gamertag string
}

// identityFor derives a stable Xbox identity from whatever stays constant for one device.
func identityFor(seed string) xblIdentity {
	sum := sha256.Sum256([]byte("nextendo-xbl:" + seed))
	xuid := 2535400000000000 + binary.BigEndian.Uint64(sum[:8])%99999999999
	return xblIdentity{
		xuid:     strconv.FormatUint(xuid, 10),
		uhs:      strconv.FormatUint(binary.BigEndian.Uint64(sum[8:16])%1e19, 10),
		gamertag: "Nextendo" + strings.ToUpper(hex.EncodeToString(sum[16:19])),
	}
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// xblToken is the {IssueInstant, NotAfter, Token, DisplayClaims} shape every Xbox Live token
// service returns. XAL treats Token as opaque.
func (s server) xblToken(claims map[string]any) map[string]any {
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{"claims": claims, "iat": now.Unix()})
	body := base64.RawURLEncoding.EncodeToString(payload)
	return map[string]any{
		"IssueInstant":  now.Format(time.RFC3339Nano),
		"NotAfter":      now.Add(16 * time.Hour).Format(time.RFC3339Nano),
		"Token":         "eyJhbGciOiJIUzI1NiJ9." + body + "." + s.sign("nextendo-xbl:", body),
		"DisplayClaims": claims,
	}
}

// xboxUserFromToken reads the Xbox user out of an "XBL3.0 x=<uhs>;<token>" value whose token this
// server issued (xblToken), refusing anything it did not sign.
func (s server) xboxUserFromToken(header string) (xuid, gamertag string, ok bool) {
	if _, t, found := strings.Cut(header, ";"); found {
		header = t
	}
	parts := strings.Split(header, ".")
	if len(parts) != 3 || !hmac.Equal([]byte(s.sign("nextendo-xbl:", parts[1])), []byte(parts[2])) {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	var payload struct {
		Claims struct {
			Xui []struct{ Xid, Gtg string } `json:"xui"`
		} `json:"claims"`
	}
	if json.Unmarshal(raw, &payload) != nil || len(payload.Claims.Xui) == 0 || payload.Claims.Xui[0].Xid == "" {
		return "", "", false
	}
	return payload.Claims.Xui[0].Xid, payload.Claims.Xui[0].Gtg, true
}

func (id xblIdentity) userClaims() map[string]any {
	return map[string]any{"xui": []any{map[string]string{
		"gtg": id.gamertag, "xid": id.xuid, "uhs": id.uhs, "agg": "Adult",
		"usr": "", "utr": "", "prv": xblPrivileges, "mgt": id.gamertag, "umg": id.gamertag,
	}}}
}

func (s server) xboxRequest(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// TEMP: full request capture while the XAL sign-in sequence is being mapped.
	names := make([]string, 0, len(r.Header))
	for k := range r.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	var hdr strings.Builder
	for _, k := range names {
		v := r.Header.Get(k)
		if len(v) > 200 {
			v = v[:200] + "…"
		}
		hdr.WriteString(" " + k + "=" + v)
	}
	shown := body
	if len(shown) > 1500 {
		shown = shown[:1500]
	}
	log.Printf("xbox request %s %s%s?%s headers:%s body=%q", r.Method, host, r.URL.Path, r.URL.RawQuery, hdr.String(), shown)

	var req map[string]any
	json.Unmarshal(body, &req)
	str := func(k string) string { v, _ := req[k].(string); return v }
	// Every token for one device must name the same user, so the identity always comes from the
	// device token, wherever the request carries it.
	deviceID := func() xblIdentity {
		dt := str("DeviceToken")
		if p, ok := req["Properties"].(map[string]any); ok && dt == "" {
			dt, _ = p["DeviceToken"].(string)
		}
		return identityFor(dt)
	}

	switch {
	case strings.HasPrefix(r.URL.Path, "/titles/") && strings.HasSuffix(r.URL.Path, "/endpoints"):
		// Xbox Live's public endpoint document, without its server-certificate pins, plus the
		// title's own Minecraft services: after sign-in the game asks XAL for a token for
		// https://api.minecraftservices.com and fails the link if no endpoint names it.
		w.Header().Set("Content-Type", "application/json")
		w.Write(titleEndpoints)

	case host == "sisu.xboxlive.com" && r.URL.Path == "/authenticate":
		// Starts a sign-in: XAL then gets an MSA token for these parameters and calls /authorize.
		q, _ := req["Query"].(map[string]any)
		params := map[string]string{
			"client_id": str("AppId"), "response_type": "code", "display": "touch",
			"scope": "service::user.auth.xboxlive.com::MBI_SSL", "redirect_uri": str("RedirectUri"),
		}
		for k, v := range q {
			if sv, ok := v.(string); ok {
				params[k] = sv
			}
		}
		qs := url.Values{}
		for k, v := range params {
			qs.Set(k, v)
		}
		w.Header().Set("X-SessionId", randomToken(24))
		reply(w, 200, map[string]any{
			"MsaOauthRedirect":     "https://login.live.com/oauth20_authorize.srf?" + qs.Encode(),
			"MsaRequestParameters": params,
		})

	case host == "login.live.com" && r.URL.Path == "/oauth20_connect.srf":
		// Remote sign-in: hand out a code the player never has to enter.
		reply(w, 200, map[string]any{
			"user_code": "NEXTENDO", "device_code": randomToken(32), "verification_uri": "https://www.microsoft.com/link",
			"interval": 1, "expires_in": 900,
		})

	case host == "login.live.com" && (r.URL.Path == "/oauth20_token.srf" || r.URL.Path == "/oauth20_authorize.srf" || r.URL.Path == "/oauth20_desktop.srf"):
		// Every grant (authorization code, device code, refresh) is approved immediately.
		form, _ := url.ParseQuery(string(body))
		seed := form.Get("refresh_token")
		if seed == "" {
			seed = randomToken(16)
		}
		id := identityFor(seed)
		reply(w, 200, map[string]any{
			"token_type": "bearer", "expires_in": 86400, "scope": "service::user.auth.xboxlive.com::MBI_SSL",
			"access_token": "EwA" + randomToken(48), "refresh_token": seed, "user_id": id.xuid[len(id.xuid)-16:],
		})

	case host == "sisu.xboxlive.com" && r.URL.Path == "/authorize":
		id := deviceID()
		reply(w, 200, map[string]any{
			"DeviceToken":        str("DeviceToken"),
			"TitleToken":         s.xblToken(map[string]any{"xti": map[string]string{"tid": "1805509784"}}),
			"UserToken":          s.xblToken(map[string]any{"xui": []any{map[string]string{"uhs": id.uhs}}}),
			"AuthorizationToken": s.xblToken(id.userClaims()),
			"WebPage":            "https://sisu.xboxlive.com/client/v33/default/view/index.html",
			"Sandbox":            "RETAIL",
			"UseModernGamertag":  true,
		})

	case host == "sisu.xboxlive.com" && r.URL.Path == "/proxy":
		// Extra relying-party tokens for the signed-in user.
		reply(w, 200, s.xblToken(deviceID().userClaims()))

	case host == "userpresence.xboxlive.com":
		// Presence heartbeats ({"state":"active"/"inactive"}): nothing to store yet.
		w.WriteHeader(200)

	case host == "notify.xboxlive.com" && r.URL.Path == "/system/notifications/endpoints":
		reply(w, 200, map[string]any{"endpointId": randomToken(16)})

	case host == "peoplehub.xboxlive.com":
		// Xbox friends: Dungeons II friends come from the Switch friend list instead.
		reply(w, 200, map[string]any{"people": []any{}, "recommendationSummary": nil, "friendFinderState": nil, "accountLinkDetails": nil})

	case host == "multiplayeractivity.xboxlive.com":
		// Xbox "joinable activity": parties are tracked by the Minecraft API instead.
		if r.Method == "GET" {
			reply(w, 200, map[string]any{"activities": []any{}})
		} else {
			w.WriteHeader(204)
		}

	case host == "rta.xboxlive.com" && r.URL.Path == "/connect":
		s.rtaSocket(w, r)

	case strings.HasSuffix(host, "auth.xboxlive.com") && strings.HasSuffix(r.URL.Path, "/authenticate"):
		switch {
		case strings.HasPrefix(host, "device."):
			reply(w, 200, s.xblToken(map[string]any{"xdi": map[string]string{"did": "F" + strings.ToUpper(hex.EncodeToString([]byte(randomToken(6)))), "dcs": "0"}}))
		case strings.HasPrefix(host, "title."):
			reply(w, 200, s.xblToken(map[string]any{"xti": map[string]string{"tid": "1805509784"}}))
		default:
			reply(w, 200, s.xblToken(map[string]any{"xui": []any{map[string]string{"uhs": deviceID().uhs}}}))
		}

	case strings.HasSuffix(host, "auth.xboxlive.com") && r.URL.Path == "/xsts/authorize":
		reply(w, 200, s.xblToken(deviceID().userClaims()))

	default:
		failure(w, 404, fmt.Sprintf("NotImplemented %s", r.URL.Path))
	}
}
