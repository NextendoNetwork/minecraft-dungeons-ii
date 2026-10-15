package main

import (
	"crypto/subtle"
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// /api/stats : la meme charge utile que les serveurs NEX servent au tableau de bord
// (nexdash) et au compteur de joueurs de nextendo-account.
//
// Dungeons II n'est PAS un serveur NEX : il n'a pas de table de connexions a lire.
// La presence se deduit donc des requetes authentifiees - notePresence est appele
// depuis verify(), le seul point par ou passe tout pid authentifie.

const (
	// Au-dela, un joueur est oublie : sans cela la carte grandirait sans fin.
	statsRetention = time.Hour
	// Meme seuil que le filtre central du tableau de bord, pour que les deux
	// comptes s'accordent au lieu de se contredire.
	statsGhostIdle = 900 * time.Second
)

var (
	presenceMu sync.Mutex
	presences  = map[uint64]*presenceEntry{}
	demarrage  = time.Now()
)

type presenceEntry struct{ premier, dernier time.Time }

// notePresence enregistre qu'un joueur vient de faire une requete authentifiee.
func notePresence(pid uint64) {
	if pid == 0 {
		return
	}
	maintenant := time.Now()
	presenceMu.Lock()
	defer presenceMu.Unlock()
	if e, ok := presences[pid]; ok {
		e.dernier = maintenant
		return
	}
	presences[pid] = &presenceEntry{premier: maintenant, dernier: maintenant}
}

// statsToken : le jeton attendu en ?key=. Comme pour nextendo-account, il arrive par
// FICHIER sur le volume deja monte - ajouter une variable d'environnement imposerait
// de recreer le conteneur. Sans jeton lisible, l'endpoint refuse tout le monde.
func statsToken() string {
	if v := strings.TrimSpace(os.Getenv("DASH_TOKEN")); v != "" {
		return v
	}
	dossier := os.Getenv("CHARACTER_DATA_DIR")
	if dossier == "" {
		dossier = "/data"
	}
	b, err := os.ReadFile(filepath.Join(dossier, "dash_token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// statsRequest sert /api/stats. Rend false si la requete ne le concerne pas.
func (s server) statsRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/stats" {
		return false
	}
	if r.Method != "GET" {
		failure(w, 405, "MethodNotAllowed")
		return true
	}
	jeton := statsToken()
	fourni := r.URL.Query().Get("key")
	// Echec ferme : un jeton absent ne doit pas rendre l'endpoint public - la reponse
	// contient les pseudos et les PID des joueurs.
	if jeton == "" || subtle.ConstantTimeCompare([]byte(fourni), []byte(jeton)) != 1 {
		failure(w, 403, "Forbidden")
		return true
	}
	reply(w, 200, construireStats())
	return true
}

func nomEnCache(pid uint64) string {
	namesMu.Lock()
	c, ok := names[pid]
	namesMu.Unlock()
	// Jamais nextendoName() ici : il appelle le serveur de comptes et bloquerait
	// chaque interrogation du tableau de bord (toutes les 2 s).
	if ok && c.name != "" {
		return c.name
	}
	return minecraftUserName(pid)
}

type instantane struct {
	pid            uint64
	inactif, enjeu float64
}

func construireStats() map[string]any {
	maintenant := time.Now()

	presenceMu.Lock()
	for pid, e := range presences {
		if maintenant.Sub(e.dernier) > statsRetention {
			delete(presences, pid)
		}
	}
	vus := make([]instantane, 0, len(presences))
	for pid, e := range presences {
		vus = append(vus, instantane{pid, maintenant.Sub(e.dernier).Seconds(), maintenant.Sub(e.premier).Seconds()})
	}
	presenceMu.Unlock()
	sort.Slice(vus, func(a, b int) bool { return vus[a].pid < vus[b].pid })

	// Les verrous sont pris l'un APRES l'autre, jamais imbriques : rien ici ne peut
	// bloquer le jeu, et l'ordre avec les autres fichiers n'a donc pas d'importance.
	type copieGroupe struct {
		id, genre string
		chef      uint64
		membres   []uint64
	}
	partiesMu.Lock()
	groupeDe := make(map[uint64]string, len(memberOf))
	for pid, id := range memberOf {
		groupeDe[pid] = id
	}
	groupes := make([]copieGroupe, 0, len(parties))
	for id, p := range parties {
		if p == nil {
			continue
		}
		groupes = append(groupes, copieGroupe{id, p.Type, p.Owner, append([]uint64(nil), p.Members...)})
	}
	partiesMu.Unlock()

	sessionsMu.Lock()
	heberges := make(map[string]bool, len(hostedSessions))
	for _, hs := range hostedSessions {
		if hs != nil {
			heberges[hs.PartyID] = hs.Ready
		}
	}
	sessionsMu.Unlock()

	vivant := make(map[uint64]bool, len(vus))
	for _, v := range vus {
		if v.inactif < statsGhostIdle.Seconds() {
			vivant[v.pid] = true
		}
	}

	// Chaque joueur commence seul dans SON groupe : les compter tous ferait autant de
	// salons que de joueurs. Un groupe ne compte que s'il reunit vraiment du monde,
	// ou si une partie est hebergee pour lui.
	sort.Slice(groupes, func(a, b int) bool { return groupes[a].id < groupes[b].id })
	retenus := make([]copieGroupe, 0, len(groupes))
	salonDe := map[uint64]uint32{}
	for _, g := range groupes {
		presents := 0
		for _, m := range g.membres {
			if vivant[m] {
				presents++
			}
		}
		_, heberge := heberges[g.id]
		if presents < 2 && !heberge {
			continue
		}
		if presents == 0 {
			continue
		}
		retenus = append(retenus, g)
		for _, m := range g.membres {
			salonDe[m] = fnv32a(g.id)
		}
	}

	connectes, enSalon := 0, 0
	joueurs := make([]map[string]any, 0, len(vus))
	for _, v := range vus {
		salon := salonDe[v.pid]
		etat := "en ligne"
		if !vivant[v.pid] {
			etat = "inactif"
		} else {
			connectes++
			if salon != 0 {
				enSalon++
			}
		}
		chef := false
		if id, ok := groupeDe[v.pid]; ok {
			for _, g := range retenus {
				if g.id == id && g.chef == v.pid {
					chef = true
					break
				}
			}
		}
		joueurs = append(joueurs, map[string]any{
			"pid":           v.pid,
			"name":          nomEnCache(v.pid),
			"state":         etat,
			"gathering":     salon,
			"isHost":        chef,
			"idleSeconds":   int64(v.inactif),
			"onlineSeconds": int64(v.enjeu),
		})
	}

	salons := make([]map[string]any, 0, len(retenus))
	for _, g := range retenus {
		membres := make([]map[string]any, 0, len(g.membres))
		for _, m := range g.membres {
			if !vivant[m] {
				continue
			}
			membres = append(membres, map[string]any{
				"pid":  m,
				"name": nomEnCache(m),
				"host": m == g.chef,
			})
		}
		etat := "salon"
		if pret, heberge := heberges[g.id]; heberge {
			etat = "partie"
			if !pret {
				etat = "demarrage"
			}
		}
		genre := g.genre
		if genre == "" {
			genre = "Party"
		}
		salons = append(salons, map[string]any{
			"id":       fnv32a(g.id),
			"hostPid":  g.chef,
			"hostName": nomEnCache(g.chef),
			"type":     genre,
			"state":    etat,
			"count":    len(membres),
			"max":      maxPartySize,
			"players":  membres,
		})
	}

	return map[string]any{
		"serverTime":    maintenant.Format("15:04:05"),
		"uptimeSeconds": int64(maintenant.Sub(demarrage).Seconds()),
		"connected":     connectes,
		"inLobby":       enSalon,
		"activeLobbies": len(salons),
		"players":       joueurs,
		"gatherings":    salons,
		"events":        []any{},
		"methods":       []any{},
		"server": map[string]any{
			"accessKey":  "dungeons2",
			"stack":      "the online stack",
			"nexVersion": "",
			"sniHost":    "",
		},
	}
}
