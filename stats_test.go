package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func resetStatsState() {
	presenceMu.Lock()
	presences = map[uint64]*presenceEntry{}
	presenceMu.Unlock()
	partiesMu.Lock()
	parties = map[string]*party{}
	memberOf = map[uint64]string{}
	partiesMu.Unlock()
	sessionsMu.Lock()
	hostedSessions = map[string]*hostedSession{}
	sessionsMu.Unlock()
}

func seePlayer(pid uint64, idle time.Duration) {
	now := time.Now()
	presenceMu.Lock()
	presences[pid] = &presenceEntry{premier: now.Add(-2 * time.Hour), dernier: now.Add(-idle)}
	presenceMu.Unlock()
}

func statsBody(t *testing.T) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/stats?key=secret", nil)
	w := httptest.NewRecorder()
	if !(server{}).statsRequest(w, r) {
		t.Fatal("statsRequest did not handle /api/stats")
	}
	if w.Code != 200 {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Sans jeton lisible l'endpoint doit refuser : la reponse contient pseudos et PID.
func TestStatsFailsClosedWithoutToken(t *testing.T) {
	t.Setenv("DASH_TOKEN", "")
	t.Setenv("CHARACTER_DATA_DIR", t.TempDir())
	w := httptest.NewRecorder()
	if !(server{}).statsRequest(w, httptest.NewRequest("GET", "/api/stats?key=", nil)) {
		t.Fatal("not handled")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 without token, got %d", w.Code)
	}
}

func TestStatsRejectsWrongToken(t *testing.T) {
	t.Setenv("DASH_TOKEN", "secret")
	w := httptest.NewRecorder()
	(server{}).statsRequest(w, httptest.NewRequest("GET", "/api/stats?key=nope", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for wrong key, got %d", w.Code)
	}
}

// Un joueur inactif au-dela du seuil ne doit pas compter comme connecte.
func TestStatsExcludesIdlePlayers(t *testing.T) {
	t.Setenv("DASH_TOKEN", "secret")
	resetStatsState()
	seePlayer(1, 5*time.Second)
	seePlayer(2, 40*time.Minute)

	d := statsBody(t)
	if c := d["connected"].(float64); c != 1 {
		t.Fatalf("connected=%v want 1", c)
	}
	ps := d["players"].([]any)
	if len(ps) != 2 {
		t.Fatalf("players=%d want 2 (idle players stay listed, just not counted)", len(ps))
	}
	for _, raw := range ps {
		p := raw.(map[string]any)
		if p["pid"].(float64) == 2 && p["idleSeconds"].(float64) < 900 {
			t.Fatalf("idle player reported as fresh: %v", p)
		}
	}
}

// Chaque joueur demarre seul dans son groupe : cela ne doit pas creer un salon par joueur.
func TestStatsSoloPartiesAreNotLobbies(t *testing.T) {
	t.Setenv("DASH_TOKEN", "secret")
	resetStatsState()
	seePlayer(1, time.Second)
	partiesMu.Lock()
	parties["p1"] = &party{ID: "p1", Owner: 1, Members: []uint64{1}}
	memberOf[1] = "p1"
	partiesMu.Unlock()

	d := statsBody(t)
	if l := d["activeLobbies"].(float64); l != 0 {
		t.Fatalf("activeLobbies=%v want 0 for a solo party", l)
	}
	if n := d["inLobby"].(float64); n != 0 {
		t.Fatalf("inLobby=%v want 0", n)
	}
}

// Un vrai groupe (deux joueurs) doit apparaitre une fois, avec ses membres vivants.
func TestStatsCountsRealParty(t *testing.T) {
	t.Setenv("DASH_TOKEN", "secret")
	resetStatsState()
	seePlayer(10, time.Second)
	seePlayer(11, 2*time.Second)
	partiesMu.Lock()
	parties["pA"] = &party{ID: "pA", Type: "Mission", Owner: 10, Members: []uint64{10, 11}}
	memberOf[10], memberOf[11] = "pA", "pA"
	partiesMu.Unlock()

	d := statsBody(t)
	if l := d["activeLobbies"].(float64); l != 1 {
		t.Fatalf("activeLobbies=%v want 1", l)
	}
	if n := d["inLobby"].(float64); n != 2 {
		t.Fatalf("inLobby=%v want 2", n)
	}
	gs := d["gatherings"].([]any)
	g := gs[0].(map[string]any)
	if g["count"].(float64) != 2 || len(g["players"].([]any)) != 2 {
		t.Fatalf("gathering members wrong: %v", g)
	}
	if g["hostPid"].(float64) != 10 {
		t.Fatalf("hostPid=%v want 10", g["hostPid"])
	}
	if g["id"].(float64) != float64(fnv32a("pA")) {
		t.Fatalf("gathering id is not FNV-32a of the party id")
	}
	if g["max"].(float64) != float64(maxPartySize) {
		t.Fatalf("max=%v want %d", g["max"], maxPartySize)
	}
}

// Le compteur et le filtre central doivent s'accorder : connected == joueurs sous le seuil.
func TestStatsConnectedMatchesLivePlayers(t *testing.T) {
	t.Setenv("DASH_TOKEN", "secret")
	resetStatsState()
	seePlayer(1, time.Second)
	seePlayer(2, 20*time.Minute)
	seePlayer(3, 10*time.Second)

	d := statsBody(t)
	live := 0
	for _, raw := range d["players"].([]any) {
		if raw.(map[string]any)["idleSeconds"].(float64) < 900 {
			live++
		}
	}
	if d["connected"].(float64) != float64(live) {
		t.Fatalf("connected=%v but %d players under the threshold", d["connected"], live)
	}
}

// verify() doit enregistrer la presence : c'est la seule source de joueurs.
func TestVerifyRecordsPresence(t *testing.T) {
	resetStatsState()
	s := server{key: []byte("0123456789abcdef")}
	tok := s.issue(4242, "session")
	if pid, ok := s.verify(tok); !ok || pid != 4242 {
		t.Fatalf("verify failed: pid=%d ok=%v", pid, ok)
	}
	presenceMu.Lock()
	_, seen := presences[4242]
	presenceMu.Unlock()
	if !seen {
		t.Fatal("verify did not record presence")
	}
}
