package main

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var characterLock sync.Mutex
var characterID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type character struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	PlayerType string `json:"playerType"`
	Skin       string `json:"skin,omitempty"`
	Data       string `json:"data,omitempty"`
}
type characterProfile struct {
	Characters map[string]character `json:"characters"`
	Primary    string               `json:"primary,omitempty"`
	// The Xbox identity linked through /account/nintendo/link (see xbox.go).
	XboxUserID   string `json:"xboxUserId,omitempty"`
	XboxGamertag string `json:"xboxGamertag,omitempty"`
	CreatedAt    string `json:"createdAt,omitempty"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
}

func (s server) characterResponse(pid uint64, c character) any {
	return s.characterResponseFor(pid, c, false)
}

// guestCharacterResponse is a party member's hero as served to the host's server: its save link
// keeps the hero's own story (see keepGuestStory), so playing in someone else's world does not
// move the guest's quests.
func (s server) guestCharacterResponse(pid uint64, c character) any {
	return s.characterResponseFor(pid, c, true)
}

func (s server) characterResponseFor(pid uint64, c character, guest bool) any {
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	resource := fmt.Sprintf("%d/%s", pid, c.ID)
	url := "https://vex.minecraftservices.com/nextendo/blobs/" + resource + "?exp=" + exp + "&sig=" + s.sign("character-blob:", resource+":"+exp)
	if guest {
		url = "https://vex.minecraftservices.com/nextendo/blobs/" + resource + "?exp=" + exp + "&guest=1&sig=" + s.sign("character-blob-guest:", resource+":"+exp)
	}
	// The hero list shows a hero's look from its loadout: [{"type":"SW.Skin","value":<skin tag>}].
	loadout := []any{}
	if c.Skin != "" {
		loadout = append(loadout, map[string]string{"type": "SW.Skin", "value": c.Skin})
	}
	return map[string]any{"id": c.ID, "type": c.Type, "playerType": c.PlayerType, "loadout": loadout, "sasUrl": map[string]any{"url": url, "path": resource, "permissions": "rw", "expiry": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "metadata": blobMetadata(pid, c.ID)}}
}

// keepGuestStory returns save with the story sections (quests, world exploration, location) taken
// from previous: a guest's hero earns levels and loot in the host's world but keeps its own story.
// The game reads saves in field order (SerializeMeta first), so the merge keeps every object's
// field order and only replaces values.
func keepGuestStory(save, previous []byte) ([]byte, error) {
	top, err := objectFields(save)
	if err != nil {
		return nil, err
	}
	prevTop, err := objectFields(previous)
	if err != nil {
		return nil, err
	}
	char, err := objectFields(fieldValue(top, "CharacterSaveV1"))
	if err != nil {
		return nil, err
	}
	prevChar, err := objectFields(fieldValue(prevTop, "CharacterSaveV1"))
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"quest", "WorldExploration"} {
		char = setField(char, k, fieldValue(prevChar, k))
	}
	meta, err1 := objectFields(fieldValue(char, "MetaData"))
	prevMeta, err2 := objectFields(fieldValue(prevChar, "MetaData"))
	if err1 == nil && err2 == nil {
		if loc := fieldValue(prevMeta, "CurrentLocation"); loc != nil {
			char = setField(char, "MetaData", encodeFields(setField(meta, "CurrentLocation", loc)))
		}
	}
	return encodeFields(setField(top, "CharacterSaveV1", encodeFields(char))), nil
}

type jsonField struct {
	key   string
	value json.RawMessage
}

// objectFields decodes a JSON object into its fields, in document order.
func objectFields(b []byte) ([]jsonField, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("not a JSON object")
	}
	var fields []jsonField
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		fields = append(fields, jsonField{key, v})
	}
	return fields, nil
}

func fieldValue(fields []jsonField, key string) json.RawMessage {
	for _, f := range fields {
		if f.key == key {
			return f.value
		}
	}
	return nil
}

// setField replaces key's value in place (appending if absent); a nil value removes the field.
func setField(fields []jsonField, key string, value json.RawMessage) []jsonField {
	for i, f := range fields {
		if f.key == key {
			if value == nil {
				return append(fields[:i:i], fields[i+1:]...)
			}
			fields[i].value = value
			return fields
		}
	}
	if value != nil {
		fields = append(fields, jsonField{key, value})
	}
	return fields
}

func encodeFields(fields []jsonField) []byte {
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(f.value)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// blobMetadata is the summary the game stored with a hero's save (level, power_level,
// current_location, ...); hero cards at the campfire read it from sasUrl.metadata.
func blobMetadata(pid uint64, id string) map[string]string {
	m := map[string]string{}
	if b, e := os.ReadFile(blobPath(pid, id) + ".meta"); e == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// characterReadLink is a read-only signed link to a hero's save: party members read each other's
// heroes from it for the campfire stats.
func (s server) characterReadLink(pid uint64, c character) map[string]any {
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	resource := fmt.Sprintf("%d/%s", pid, c.ID)
	url := "https://vex.minecraftservices.com/nextendo/blobs/" + resource + "?exp=" + exp + "&ro=1&sig=" + s.sign("character-blob-read:", resource+":"+exp)
	return map[string]any{"url": url, "path": resource, "permissions": "r", "expiry": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "metadata": blobMetadata(pid, c.ID)}
}

func (s server) blobRequest(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/nextendo/blobs/") {
		return false
	}
	resource := strings.TrimPrefix(r.URL.Path, "/nextendo/blobs/")
	parts := strings.Split(resource, "/")
	if len(parts) != 2 || !characterID.MatchString(parts[1]) {
		failure(w, 400, "InvalidBlob")
		return true
	}
	pid, e := strconv.ParseUint(parts[0], 10, 64)
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	readOnly := r.URL.Query().Get("ro") == "1"
	guest := r.URL.Query().Get("guest") == "1"
	purpose := "character-blob:"
	if readOnly {
		purpose = "character-blob-read:"
	} else if guest {
		purpose = "character-blob-guest:"
	}
	if e != nil || err != nil || exp <= time.Now().Unix() || !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(s.sign(purpose, resource+":"+r.URL.Query().Get("exp")))) {
		failure(w, 403, "InvalidBlobSignature")
		return true
	}
	if readOnly && r.Method != "GET" && r.Method != "HEAD" {
		failure(w, 403, "ReadOnlyBlob")
		return true
	}
	characterLock.Lock()
	defer characterLock.Unlock()
	p, e := readProfile(pid)
	if e != nil {
		failure(w, 500, "CharacterStorageError")
		return true
	}
	if _, ok := p.Characters[parts[1]]; !ok {
		failure(w, 404, "CharacterNotFound")
		return true
	}
	file := blobPath(pid, parts[1])
	switch r.Method {
	case "GET", "HEAD":
		if _, e := os.Stat(file); os.IsNotExist(e) {
			// A new hero has no save until it is first played; the game builds it from defaults.
			failure(w, 404, "BlobNotFound")
			return true
		}
		// Azure Blob Storage semantics: the game stores hero summary fields (level, power_level,
		// game_data_updated, ...) as x-ms-meta-* headers next to the save and reads them back.
		if meta, e := os.ReadFile(file + ".meta"); e == nil {
			var m map[string]string
			if json.Unmarshal(meta, &m) == nil {
				for k, v := range m {
					w.Header().Set("x-ms-meta-"+k, v)
				}
			}
		}
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, file)
	case "PUT":
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if e != nil {
			failure(w, 413, "BlobTooLarge")
			return true
		}
		storyKept := false
		if guest {
			if old, err := os.ReadFile(file); err == nil {
				if merged, err := keepGuestStory(body, old); err == nil {
					body, storyKept = merged, true
				}
			}
		}
		tmp, e := os.CreateTemp(filepath.Dir(file), ".blob-*")
		if e != nil {
			failure(w, 500, "CharacterStorageError")
			return true
		}
		name := tmp.Name()
		defer os.Remove(name)
		_, e = tmp.Write(body)
		if e == nil {
			e = tmp.Sync()
		}
		closeErr := tmp.Close()
		if e == nil {
			e = closeErr
		}
		if e == nil {
			e = os.Rename(name, file)
		}
		if e != nil {
			failure(w, 500, "CharacterStorageError")
			return true
		}
		meta := map[string]string{}
		for k, v := range r.Header {
			if key, ok := strings.CutPrefix(strings.ToLower(k), "x-ms-meta-"); ok && len(v) > 0 {
				meta[key] = v[0]
			}
		}
		if storyKept {
			if loc, ok := blobMetadata(pid, parts[1])["current_location"]; ok {
				meta["current_location"] = loc
			}
		}
		if b, err := json.Marshal(meta); err == nil {
			os.WriteFile(file+".meta", b, 0600)
		}
		log.Printf("blob metadata for %s: %v", parts[1], meta) // TEMP: confirm field formats
		w.Header().Set("x-ms-request-server-encrypted", "true")
		w.WriteHeader(201)
	default:
		failure(w, 405, "MethodNotAllowed")
	}
	return true
}

func profilePath(pid uint64) string {
	root := os.Getenv("CHARACTER_DATA_DIR")
	if root == "" {
		root = "data"
	}
	return filepath.Join(root, fmt.Sprintf("%d.json", pid))
}
func blobPath(pid uint64, id string) string {
	return filepath.Join(filepath.Dir(profilePath(pid)), fmt.Sprintf("%d-%s.blob", pid, id))
}
func readProfile(pid uint64) (characterProfile, error) {
	p := characterProfile{Characters: map[string]character{}}
	b, e := os.ReadFile(profilePath(pid))
	if os.IsNotExist(e) {
		return p, nil
	}
	if e != nil {
		return p, e
	}
	if e = json.Unmarshal(b, &p); e != nil {
		return p, e
	}
	if p.Characters == nil {
		p.Characters = map[string]character{}
	}
	return p, nil
}
func writeProfile(pid uint64, p characterProfile) error {
	path := profilePath(pid)
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".character-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(name, path)
}

// Route before the PlayFab POST-only dispatcher: character reads are GETs.
func (s server) characterRequest(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if path != "/spicewood/characters" && path != "/spicewood/characters/online" && path != "/spicewood/characters/offline" && path != "/spicewood/characters/loadout" && path != "/spicewood/characters/list/unknown" && path != "/spicewood/profiles/characters/primary" && !strings.HasPrefix(path, "/spicewood/characters/id/") {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	pid, valid := s.verify(token)
	if !ok || !valid {
		failure(w, 401, "InvalidSessionTicket")
		return true
	}
	characterLock.Lock()
	defer characterLock.Unlock()
	p, e := readProfile(pid)
	if e != nil {
		failure(w, 500, "CharacterStorageError")
		return true
	}
	switch {
	case path == "/spicewood/characters" && r.Method == "GET":
		list := make([]any, 0, len(p.Characters))
		for _, c := range p.Characters {
			list = append(list, s.characterResponse(pid, c))
		}
		reply(w, 200, map[string]any{"characters": list})
	case (path == "/spicewood/characters/online" || path == "/spicewood/characters/offline") && r.Method == "POST":
		var c character
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&c) != nil || !characterID.MatchString(c.ID) || (c.PlayerType != "PLAYER" && c.PlayerType != "GUEST") {
			failure(w, 400, "InvalidCharacter")
			return true
		}
		// Repeated creation of the same UUID must not erase existing progress.
		if existing, ok := p.Characters[c.ID]; ok {
			reply(w, 200, s.characterResponse(pid, existing))
			return true
		}
		c.Type = "ONLINE"
		if path == "/spicewood/characters/offline" {
			c.Type = "OFFLINE"
		}
		p.Characters[c.ID] = c
		if e = writeProfile(pid, p); e != nil {
			failure(w, 500, "CharacterStorageError")
			return true
		}
		reply(w, 200, s.characterResponse(pid, c))
	case path == "/spicewood/characters/list/unknown" && r.Method == "POST":
		// The game lists its local heroes; reply with those this server has never seen so it
		// uploads them (POST /characters/offline + save PUT).
		var q struct {
			CharacterIDs []string `json:"characterIds"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&q) != nil {
			failure(w, 400, "InvalidRequest")
			return true
		}
		unknown := []string{}
		for _, id := range q.CharacterIDs {
			if _, ok := p.Characters[id]; !ok {
				unknown = append(unknown, id)
			}
		}
		reply(w, 200, map[string]any{"characterIds": unknown})
	case path == "/spicewood/profiles/characters/primary" && r.Method == "PUT":
		var q struct {
			CharacterID string `json:"characterId"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil {
			failure(w, 400, "InvalidRequest")
			return true
		}
		c, ok := p.Characters[q.CharacterID]
		if !ok {
			failure(w, 404, "CharacterNotFound")
			return true
		}
		// Creation must see no save (the game builds the new hero itself and fails creation if one
		// exists), but every later login needs one or the hero is discarded. The game selects the
		// new hero right after creating it, so that is when its starting save is written.
		if c.Type == "ONLINE" && writeNewHeroSave(blobPath(pid, c.ID), c) != nil {
			failure(w, 500, "CharacterStorageError")
			return true
		}
		p.Primary = q.CharacterID
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if p.CreatedAt == "" {
			p.CreatedAt = now
		}
		p.UpdatedAt = now
		if writeProfile(pid, p) != nil {
			failure(w, 500, "CharacterStorageError")
			return true
		}
		profileID := fmt.Sprintf("%d", pid)
		reply(w, 200, map[string]string{"id": profileID + "-MAIN", "characterId": p.Primary, "profileId": profileID, "createdAt": p.CreatedAt, "updatedAt": p.UpdatedAt})
	case path == "/spicewood/characters/loadout" && r.Method == "PUT":
		// {"characterId": ..., "loadout": {"type": "SW.Skin", "value": ...}}: one item per request.
		type loadoutItem struct{ Type, Value string }
		var q struct {
			CharacterID string          `json:"characterId"`
			Loadout     json.RawMessage `json:"loadout"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&q) != nil {
			failure(w, 400, "InvalidRequest")
			return true
		}
		var items []loadoutItem
		var one loadoutItem
		if json.Unmarshal(q.Loadout, &one) == nil && one.Type != "" {
			items = append(items, one)
		} else if json.Unmarshal(q.Loadout, &items) != nil {
			failure(w, 400, "InvalidRequest")
			return true
		}
		c, ok := p.Characters[q.CharacterID]
		if !ok {
			failure(w, 404, "CharacterNotFound")
			return true
		}
		for _, item := range items {
			if item.Type == "SW.Skin" {
				c.Skin = item.Value
			}
		}
		p.Characters[c.ID] = c
		if writeProfile(pid, p) != nil {
			failure(w, 500, "CharacterStorageError")
			return true
		}
		reply(w, 200, s.characterResponse(pid, c))
	case strings.HasPrefix(path, "/spicewood/characters/id/") && r.Method == "DELETE":
		id := strings.TrimPrefix(path, "/spicewood/characters/id/")
		if _, ok := p.Characters[id]; !ok {
			failure(w, 404, "CharacterNotFound")
			return true
		}
		delete(p.Characters, id)
		if p.Primary == id {
			p.Primary = ""
		}
		if writeProfile(pid, p) != nil {
			failure(w, 500, "CharacterStorageError")
			return true
		}
		os.Remove(blobPath(pid, id))
		os.Remove(blobPath(pid, id) + ".meta")
		w.WriteHeader(204)
	case strings.HasPrefix(path, "/spicewood/characters/id/") && r.Method == "GET":
		id := strings.TrimPrefix(path, "/spicewood/characters/id/")
		c, ok := p.Characters[id]
		if !ok {
			failure(w, 404, "CharacterNotFound")
			return true
		}
		reply(w, 200, s.characterResponse(pid, c))
	default:
		failure(w, 405, "MethodNotAllowed")
	}
	return true
}
