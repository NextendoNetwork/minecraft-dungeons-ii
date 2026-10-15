package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestKeepGuestStory(t *testing.T) {
	prev := `{"SerializeMeta":{"HardFormat":"FCharacterSaveV1"},"CharacterSaveV1":{"MetaData":{"Level":1,"CurrentLocation":"SW.Area.Camp.A1"},"Ability":{"Attributes":[{"AttributeName":"Level","CurrentValue":1}]},"quest":{"FocusedQuestId":"CA00"},"WorldExploration":{"SavedCutsceneTags":["CS01"]}}}`
	host := `{"SerializeMeta":{"HardFormat":"FCharacterSaveV1"},"CharacterSaveV1":{"MetaData":{"Level":2,"CurrentLocation":"SW.Area.Town"},"Ability":{"Attributes":[{"AttributeName":"Level","CurrentValue":2}]},"quest":{"FocusedQuestId":"CA01"},"WorldExploration":{"SavedCutsceneTags":["CS01","CS02"]}}}`
	out, err := keepGuestStory([]byte(host), []byte(prev))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		C struct {
			MetaData struct {
				Level           int
				CurrentLocation string
			}
			Ability          json.RawMessage
			Quest            struct{ FocusedQuestId string } `json:"quest"`
			WorldExploration struct{ SavedCutsceneTags []string }
		} `json:"CharacterSaveV1"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.C.Quest.FocusedQuestId != "CA00" || len(got.C.WorldExploration.SavedCutsceneTags) != 1 || got.C.MetaData.CurrentLocation != "SW.Area.Camp.A1" {
		t.Fatalf("guest story not kept: %s", out)
	}
	if got.C.MetaData.Level != 2 || !strings.Contains(string(got.C.Ability), `"CurrentValue":2`) {
		t.Fatalf("guest progress lost: %s", out)
	}
	// The game reads the save in order: SerializeMeta first, MetaData before the rest.
	if !strings.HasPrefix(string(out), `{"SerializeMeta":`) || !strings.HasPrefix(string(out[strings.Index(string(out), `"CharacterSaveV1":`):]), `"CharacterSaveV1":{"MetaData":{"Level":2,"CurrentLocation"`) {
		t.Fatalf("field order changed: %s", out)
	}
}

func TestBlobRoundTripAndSignature(t *testing.T) {
	t.Setenv("CHARACTER_DATA_DIR", t.TempDir())
	s := server{[]byte("test-only-not-production-key")}
	id := "12345678-1234-1234-1234-123456789abc"
	c := character{ID: id, Type: "ONLINE", PlayerType: "PLAYER"}
	if e := writeProfile(1, characterProfile{Characters: map[string]character{id: c}}); e != nil {
		t.Fatal(e)
	}
	response := s.characterResponse(1, c).(map[string]any)
	sas := response["sasUrl"].(map[string]any)
	for _, k := range []string{"url", "path", "permissions", "expiry", "metadata"} {
		if _, ok := sas[k]; !ok {
			t.Fatal("missing SAS field", k)
		}
	}
	address := sas["url"].(string)
	for _, method := range []string{"PUT", "GET"} {
		r := httptest.NewRequest(method, address, strings.NewReader("exact-save-bytes"))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if method == "PUT" && w.Code != 201 {
			t.Fatal(w.Code)
		}
		if method == "GET" && (w.Code != 200 || w.Body.String() != "exact-save-bytes") {
			t.Fatal("save changed")
		}
	}
	u, _ := url.Parse(address)
	q := u.Query()
	q.Set("sig", "forged")
	u.RawQuery = q.Encode()
	r := httptest.NewRequest("GET", u.String(), nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("invalid signature accepted")
	}
}

func TestCharacterPersistenceIsolationAndRetry(t *testing.T) {
	t.Setenv("CHARACTER_DATA_DIR", t.TempDir())
	s := server{[]byte("test-only-not-production-key")}
	id := "12345678-1234-1234-1234-123456789abc"
	call := func(pid uint64, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+s.issue(pid, "entity"))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	body := `{"id":"` + id + `","playerType":"PLAYER","data":"original-save","skin":"test-skin"}`
	if w := call(1, "POST", "/spicewood/characters/online", body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call(1, "POST", "/spicewood/characters/online", strings.Replace(body, "original-save", "", 1)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p, e := readProfile(1)
	if e != nil || p.Characters[id].Data != "original-save" {
		t.Fatal("retry destroyed progress", e)
	}
	w := call(1, "GET", "/spicewood/characters", "")
	var list struct{ Characters []character }
	json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Characters) != 1 {
		t.Fatal("list failed")
	}
	w = call(2, "GET", "/spicewood/characters/id/"+id, "")
	if w.Code != 404 {
		t.Fatal("cross-account access")
	}
	w = call(1, "PUT", "/spicewood/profiles/characters/primary", `{"characterId":"`+id+`"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var primary map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &primary); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "characterId", "profileId", "createdAt", "updatedAt"} {
		if primary[field] == "" {
			t.Fatal("missing primary response field", field)
		}
	}
	if primary["characterId"] != id || primary["profileId"] != "1" {
		t.Fatal("incorrect primary response identity")
	}
	p, e = readProfile(1)
	if e != nil || p.Primary != id {
		t.Fatal("primary not persisted")
	}
	w = call(1, "POST", "/spicewood/characters/online", `{"id":"../../escape","playerType":"PLAYER"}`)
	if w.Code != 400 {
		t.Fatal("unsafe ID accepted")
	}
	r := httptest.NewRequest("GET", "/spicewood/characters", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("anonymous access")
	}
}
