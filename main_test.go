package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func carrier(s server, expiry int64, bad bool) string {
	raw := fmt.Sprintf("1800000301.player.%d", expiry)
	sig := s.sign("nex:", raw)
	if bad {
		sig = "bad"
	}
	b, _ := json.Marshal(map[string]string{"sub": "random-unstable-subject", "nnex": "nx2." + base64.RawURLEncoding.EncodeToString([]byte(raw)) + "." + sig})
	return "header." + base64.RawURLEncoding.EncodeToString(b) + ".signature"
}
func TestIdentity(t *testing.T) {
	s := server{[]byte("test-only-not-production-key")}
	for _, c := range []struct {
		name  string
		token string
		valid bool
	}{
		{"valid", carrier(s, time.Now().Add(time.Hour).Unix(), false), true},
		{"expired", carrier(s, time.Now().Add(-time.Hour).Unix(), false), false},
		{"forged", carrier(s, time.Now().Add(time.Hour).Unix(), true), false},
		{"unsigned PID", "header.eyJucGlkIjoxODAwMDAzNDA2fQ.signature", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pid, e := s.identity(c.token)
			if (e == nil) != c.valid {
				t.Fatalf("valid=%v err=%v", c.valid, e)
			}
			if c.valid && pid != 1800000301 {
				t.Fatal("wrong identity")
			}
		})
	}
}
func TestLoginAndEntityRefresh(t *testing.T) {
	s := server{[]byte("test-only-not-production-key")}
	b, _ := json.Marshal(map[string]string{"TitleId": "83156", "IdentityToken": carrier(s, time.Now().Add(time.Hour).Unix(), false)})
	r := httptest.NewRequest("POST", "/Client/LoginWithNintendoServiceAccount", strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var result struct {
		Data struct {
			SessionTicket     string
			PlayFabId         string
			InfoResultPayload struct{ AccountInfo map[string]any }
		}
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.Data.InfoResultPayload.AccountInfo == nil {
		t.Fatal("game requires InfoResultPayload.AccountInfo")
	}
	if result.Data.PlayFabId != fmt.Sprintf("%016X", uint64(1800000301)) {
		t.Fatal("unstable identity")
	}
	r = httptest.NewRequest("POST", "/Authentication/GetEntityToken", nil)
	r.Header.Set("X-Authorization", result.Data.SessionTicket)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if _, ok := s.verify(result.Data.SessionTicket + "tampered"); ok {
		t.Fatal("accepted tampered token")
	}
	r = httptest.NewRequest("POST", "/spicewood/parties/", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 501 {
		t.Fatal("unimplemented API claimed success")
	}
}

func TestMinecraftLogin(t *testing.T) {
	s := server{[]byte("test-only-not-production-key")}
	for _, valid := range []bool{false, true} {
		token := "forged"
		if valid {
			token = s.issue(1800000301, "entity")
		}
		body, _ := json.Marshal(map[string]string{"playfabToken": token})
		r := httptest.NewRequest("POST", "/login/playfab", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if !valid {
			if w.Code != 401 {
				t.Fatal("accepted invalid credential")
			}
			continue
		}
		var response map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatal("login failed")
		}
		for _, field := range []string{"userName", "access_token", "expires_in", "user_hash"} {
			if _, ok := response[field]; !ok {
				t.Fatalf("missing game-required field %s", field)
			}
		}
	}
}

func TestInboxShape(t *testing.T) {
	s := server{[]byte("test-only-not-production-key")}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/inbox/messages", strings.NewReader(`{"languageCode":"en-US"}`)))
	var got struct {
		Messages struct {
			Takeover []any `json:"TAKEOVER"`
		} `json:"messages"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Messages.Takeover == nil {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestNintendoEntitlement(t *testing.T) {
	s := server{[]byte("test-only-not-production-key")}
	req := httptest.NewRequest("POST", "/spicewood/entitlement/nintendo", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("unauthenticated status=%d", w.Code)
	}
	req = httptest.NewRequest("POST", "/spicewood/entitlement/nintendo", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+s.issue(1800000301, "entity"))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var got struct{ Entitlements string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || strings.Count(got.Entitlements, ".") != 2 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBomSchedule(t *testing.T) {
	s := server{[]byte("test-only-not-production-key")}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/spicewood/config/bom/schedule", nil))
	var got struct{ BomSchedules []any }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.BomSchedules == nil {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestSisuAutoLink(t *testing.T) {
	s := server{[]byte("test-only-not-production-key")}
	req := httptest.NewRequest("POST", "https://sisu.xboxlive.com/authenticate", strings.NewReader(`{"AppId":"00000000497c1b94","RedirectUri":"https://login.live.com/oauth20_desktop.srf","DeviceToken":"dev","Query":{"state":"s"}}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("X-SessionId") == "" || !strings.Contains(w.Body.String(), "MsaOauthRedirect") {
		t.Fatalf("authenticate status=%d body=%s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("POST", "https://sisu.xboxlive.com/authorize", strings.NewReader(`{"AccessToken":"t=x","DeviceToken":"dev","SessionId":"abc"}`))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	var got struct {
		AuthorizationToken struct {
			DisplayClaims struct {
				Xui []struct{ Gtg, Xid string }
			}
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.AuthorizationToken.DisplayClaims.Xui) != 1 || got.AuthorizationToken.DisplayClaims.Xui[0].Xid == "" {
		t.Fatalf("authorize status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestXboxLinkPersists(t *testing.T) {
	t.Setenv("CHARACTER_DATA_DIR", t.TempDir())
	s := server{[]byte("test-only-not-production-key")}
	id := identityFor("device")
	tok := s.xblToken(id.userClaims())["Token"].(string)
	body, _ := json.Marshal(map[string]string{"mapiXblToken": "XBL3.0 x=" + id.uhs + ";" + tok})
	req := httptest.NewRequest("POST", "/account/nintendo/link", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+s.issue(1800000301, "entity"))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("link status=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/Client/LoginWithNintendoServiceAccount", strings.NewReader(`{"IdentityToken":"`+carrier(s, time.Now().Add(time.Hour).Unix(), false)+`","TitleId":"83156"}`)))
	if !strings.Contains(w.Body.String(), `"XboxUserId":"`+id.xuid+`"`) {
		t.Fatalf("login missing XboxInfo: %s", w.Body.String())
	}
}
