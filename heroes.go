package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"
)

// heroSaveTemplate is a fresh FCharacterSaveV1 exactly as the game writes it. The game's save reader
// is order-sensitive ("Expected struct key '%s', got '%s'"): keys must stay in this order, so the
// save is filled in as text rather than marshalled from a map (Go sorts map keys).
const heroSaveTemplate = `{"SerializeMeta":{"InternalVersion":0,"HardFormat":"FCharacterSaveV1","SoftVersion":5,"FormatHash":1764133460},"CharacterSaveV1":{"MetaData":{"CharacterId":%s,"Created":0,"GameDataUpdated":%d,"IsOnline":%t,"IsGuest":%t,"Level":1,"PowerLevel":1,"CurrentLocation":"None","ReleaseToggles":["R1","R2"],"CurrentDifficulty":"None"},"Ability":{"Attributes":[],"ProgressionTags":[]},"Achievements":{"QuestAchievements":{},"BoolAchievements":{},"CollectionAchievements":{},"CountAchievements":{}},"CollectionsStats":{"CollectedWeaponsUnique":[],"CollectedWeaponsSpecial":[],"CollectedWeaponsRare":[],"CollectedWeaponsCommon":[],"CollectedArmorUnique":[],"CollectedArmorSpecial":[],"CollectedArmorRare":[],"CollectedArmorCommon":[],"CollectedArtifactsUnique":[],"CollectedArtifactsSpecial":[],"CollectedArtifactsRare":[],"CollectedArtifactsCommon":[],"CollectedTalismans":[],"CollectedEnchantmentBooksOffensive":[],"CollectedEnchantmentBooksDefensive":[],"CollectedEnchantmentBooksUtility":[],"CollectedEnchantmentBooksUncategorised":[],"ShownHints":[],"SeenExpandableInfo":[]},"Cosmetics":{"Cosmetics":{"SW.Skin":{"TypeTag":%s}}},"Inventory":{"Entries":[]},"LootProgression":{"DiscoveredLoot":[]},"quest":{"Quests":[],"FocusedQuestId":""},"WorldExploration":{"DiscoveredMinecartStationTags":[],"ActivatedGimmickTags":[],"SavedCutsceneTags":[],"SavedFogOfWarExploration":{"Items":[]},"DiscoveredDungeonDoors":[],"LastMinecartStation":"None","SavedActorStates":[]}}}`

var skinTag = regexp.MustCompile(`^SW\.Skin\.[A-Za-z0-9_.]+$`)

// An online hero's save normally comes from the game server the first time it is played; until
// then the game cannot load the hero and creates a default one at the next login. So the hero
// gets its starting save as soon as creation finishes: the save the game writes for a fresh hero,
// plus the Azure blob metadata the game sends with every save upload.
func newHeroSave(c character, now time.Time) (save []byte, meta map[string]string) {
	skin := c.Skin
	if !skinTag.MatchString(skin) {
		skin = "SW.Skin.Valorie"
	}
	id, _ := json.Marshal(c.ID)
	tag, _ := json.Marshal(skin)
	const unixEpochTicks = 621355968000000000
	save = []byte(fmt.Sprintf(heroSaveTemplate, id, unixEpochTicks+now.UTC().UnixNano()/100, c.Type == "ONLINE", c.PlayerType == "GUEST", tag))
	meta = map[string]string{
		"version": "2", "game_data_updated": strconv.FormatInt(now.Unix(), 10), "level": "1", "power_level": "1.000000",
		"current_location": "None", "release_toggles": "R1,R2", "current_difficulty": "None",
	}
	return save, meta
}

// writeNewHeroSave stores the starting save unless the hero already has one.
func writeNewHeroSave(file string, c character) error {
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	save, meta := newHeroSave(c, time.Now())
	b, _ := json.Marshal(meta)
	if err := os.WriteFile(file+".meta", b, 0600); err != nil {
		return err
	}
	return os.WriteFile(file, save, 0600)
}
