package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The game's save reader is order-sensitive, so the starting save must match the game's own
// output byte for byte (testdata: a fresh hero the game uploaded itself).
func TestStartingSaveMatchesGameOutput(t *testing.T) {
	want, err := os.ReadFile("testdata/fresh_offline_save.json")
	if err != nil {
		t.Fatal(err)
	}
	const ticks, unixEpochTicks = 639264378732510455, 621355968000000000
	now := time.Unix(0, (ticks-unixEpochTicks)*100)
	got, _ := newHeroSave(character{ID: "39d41f5f-bd55-11f1-8049-000000000000", Type: "OFFLINE", PlayerType: "PLAYER", Skin: "SW.Skin.Valorie"}, now)
	if string(got) != strings.TrimSpace(string(want)) {
		t.Fatalf("starting save differs from the game's\n got: %s\nwant: %s", got, want)
	}
}

func TestOnlineHeroGetsStartingSave(t *testing.T) {
	t.Setenv("CHARACTER_DATA_DIR", t.TempDir())
	s := server{[]byte("test-only-not-production-key")}
	id := "59C52D5C-BD6B-11F1-807B-C92FFB5778D9"
	r := httptest.NewRequest("POST", "/spicewood/characters/online", strings.NewReader(`{"id":"`+id+`","playerType":"PLAYER","skin":"SW.Skin.Healer"}`))
	r.Header.Set("Authorization", "Bearer "+s.issue(7, "entity"))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	address := created["sasUrl"].(map[string]any)["url"].(string)
	// During creation the game must find no save.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", address, nil))
	if w.Code != 404 {
		t.Fatal("save existed during creation", w.Code)
	}
	r = httptest.NewRequest("PUT", "/spicewood/profiles/characters/primary", strings.NewReader(`{"characterId":"`+id+`"}`))
	r.Header.Set("Authorization", "Bearer "+s.issue(7, "entity"))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", address, nil))
	if w.Code != 200 || w.Header().Get("x-ms-meta-version") != "2" || w.Header().Get("x-ms-meta-level") != "1" {
		t.Fatal("starting save or metadata missing", w.Code, w.Header())
	}
	var save struct {
		CharacterSaveV1 struct {
			MetaData struct {
				CharacterId string
				IsOnline    bool
			}
			Cosmetics struct {
				Cosmetics map[string]struct{ TypeTag string }
			}
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &save) != nil || save.CharacterSaveV1.MetaData.CharacterId != id || !save.CharacterSaveV1.MetaData.IsOnline || save.CharacterSaveV1.Cosmetics.Cosmetics["SW.Skin"].TypeTag != "SW.Skin.Healer" {
		t.Fatal("bad starting save", w.Body.String())
	}

	r = httptest.NewRequest("POST", "/spicewood/characters/list/unknown", strings.NewReader(`{"characterIds":["`+id+`","00000000-0000-0000-0000-000000000000"]}`))
	r.Header.Set("Authorization", "Bearer "+s.issue(7, "entity"))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "00000000-0000-0000-0000-000000000000") || strings.Contains(w.Body.String(), id) {
		t.Fatal("unknown list wrong", w.Body.String())
	}
}
