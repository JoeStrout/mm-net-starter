// Command server is a minimal relay server for networked Mini Micro games.
//
// Each game keeps an append-only event log in memory. Clients poll
// POST /games/{id}/sync about once per second, sending any new actions and
// receiving every event (visible to them) after the last sequence number
// they have seen.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"maps"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	maxPlayers    = 6
	moveSpeed     = 200.0 // world units per second; sent to clients on join
	worldW        = 960.0
	worldH        = 640.0
	maxNameLen    = 20
	maxChatLen    = 200
	maxBodyBytes  = 16 << 10
	gameIdleTTL   = 10 * time.Minute
	maxActionLag  = 0.5 // how far back (seconds) a client may timestamp an action
	playerTimeout = 15 * time.Second
	recentChatLen = 8 // chat messages sent to a newly joined player
)

// shapes are the player sprite shapes (from /sys/pics/shapes in Mini Micro).
// There are more shapes than maxPlayers, so every player gets a unique one.
var shapes = []string{"Circle", "Heart", "Hexagon", "Octagon", "Pentagon", "Septagon", "Square", "Star", "Triangle"}

// ClientEvent is an action submitted by a client.
type ClientEvent struct {
	Type string  `json:"type"` // "move" or "chat"
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Text string  `json:"text"`
	T    float64 `json:"t"` // client's estimate of server time when the action happened
}

// Event is one entry in a game's log, as sent to clients.
type Event struct {
	Seq   int     `json:"seq"`
	T     float64 `json:"t"`    // server time, seconds since game start
	From  int     `json:"from"` // player ID
	Type  string  `json:"type"` // "join", "move", "chat", or "leave"
	Name  string  `json:"name,omitempty"`
	Shape string  `json:"shape,omitempty"` // join only
	FX    float64 `json:"fx"`              // join/move: position at time T
	FY    float64 `json:"fy"`
	X     float64 `json:"x"` // join/move: destination
	Y     float64 `json:"y"`
	Text  string  `json:"text,omitempty"`

	to []int // recipient player IDs; nil means everyone
}

func (e *Event) visibleTo(playerID int) bool {
	return e.to == nil || slices.Contains(e.to, playerID)
}

// Player is a participant in a game. Movement is stored as intent (start
// point, destination, start time) so any position can be computed exactly.
type Player struct {
	ID       int
	Name     string
	Shape    string
	token    string
	fx, fy   float64
	tx, ty   float64
	t0       float64
	lastSeen time.Time
}

func (p *Player) posAt(t float64) (x, y float64) {
	dx, dy := p.tx-p.fx, p.ty-p.fy
	d := math.Hypot(dx, dy)
	if d == 0 {
		return p.tx, p.ty
	}
	f := math.Min(1, math.Max(0, moveSpeed*(t-p.t0)/d))
	return p.fx + dx*f, p.fy + dy*f
}

// Game holds the state of one match. All fields are guarded by mu.
type Game struct {
	mu         sync.Mutex
	id         string
	start      time.Time
	lastActive time.Time
	players    map[int]*Player
	nextID     int
	events     []Event
}

func newGame(id string) *Game {
	now := time.Now()
	return &Game{id: id, start: now, lastActive: now, players: map[int]*Player{}, nextID: 1}
}

func (g *Game) now() float64 {
	return time.Since(g.start).Seconds()
}

func (g *Game) appendEvent(e Event) {
	e.Seq = len(g.events) + 1
	g.events = append(g.events, e)
}

// pickShape returns a random shape not already used by a player in this game.
func (g *Game) pickShape() string {
	inUse := map[string]bool{}
	for _, p := range g.players {
		inUse[p.Shape] = true
	}
	var free []string
	for _, s := range shapes {
		if !inUse[s] {
			free = append(free, s)
		}
	}
	return free[mrand.IntN(len(free))]
}

// dropIdlePlayers removes players who haven't synced within playerTimeout.
func (g *Game) dropIdlePlayers(now time.Time) {
	for id, p := range g.players {
		if now.Sub(p.lastSeen) > playerTimeout {
			delete(g.players, id)
			g.appendEvent(Event{T: g.now(), From: id, Type: "leave", Name: p.Name})
			log.Printf("game %q: dropped player %d (%s) after %.1fs without a sync",
				g.id, id, p.Name, now.Sub(p.lastSeen).Seconds())
		}
	}
}

// apply validates a client action and appends the resulting event(s).
// Unknown or invalid actions are silently dropped.
func (g *Game) apply(p *Player, ce ClientEvent, now float64) {
	switch ce.Type {
	case "move":
		x := math.Min(worldW, math.Max(0, ce.X))
		y := math.Min(worldH, math.Max(0, ce.Y))
		t := actionTime(p, ce.T, now)
		p.fx, p.fy = p.posAt(t)
		p.tx, p.ty, p.t0 = x, y, t
		g.appendEvent(Event{T: t, From: p.ID, Type: "move", FX: p.fx, FY: p.fy, X: x, Y: y})
	case "chat":
		text := strings.TrimSpace(ce.Text)
		if r := []rune(text); len(r) > maxChatLen {
			text = string(r[:maxChatLen])
		}
		if text != "" {
			// Include the name, since the sender may be gone when a
			// later joiner receives this as recent chat.
			g.appendEvent(Event{T: now, From: p.ID, Type: "chat", Name: p.Name, Text: text})
		}
	}
}

// actionTime returns when to consider a client action to have happened.
// Honoring the client's timestamp lets the server's version of a move match
// the client's local prediction, but it may not be in the future, more than
// maxActionLag in the past, or earlier than the player's previous move.
func actionTime(p *Player, clientT, now float64) float64 {
	if clientT <= 0 {
		return now
	}
	return math.Min(now, math.Max(clientT, math.Max(p.t0, now-maxActionLag)))
}

type Server struct {
	mu    sync.Mutex
	games map[string]*Game
}

// game returns the game with the given ID, creating it if create is true.
func (s *Server) game(id string, create bool) *Game {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.games[id]
	if g == nil && create {
		g = newGame(id)
		log.Printf("game %q: created", id)
		s.games[id] = g
	}
	return g
}

type joinRequest struct {
	Name string `json:"name"`
}

// PlayerInfo describes a player currently in a game, including their motion.
type PlayerInfo struct {
	ID    int     `json:"id"`
	Name  string  `json:"name"`
	Shape string  `json:"shape"`
	FX    float64 `json:"fx"` // position at time T
	FY    float64 `json:"fy"`
	X     float64 `json:"x"` // destination
	Y     float64 `json:"y"`
	T     float64 `json:"t"`
}

// joinResponse gives a new player a snapshot of the game, so the client
// can start syncing from its own join instead of replaying the whole log.
type joinResponse struct {
	Player     int          `json:"player"`
	Token      string       `json:"token"`
	ServerTime float64      `json:"serverTime"`
	Speed      float64      `json:"speed"`
	Players    []PlayerInfo `json:"players"`    // everyone in the game, including the new player
	RecentChat []Event      `json:"recentChat"` // up to recentChatLen, oldest first
	Last       int          `json:"last"`       // sequence number to send as "after" on the first sync
}

// snapshotPlayers returns info on every current player, ordered by ID.
func (g *Game) snapshotPlayers() []PlayerInfo {
	out := make([]PlayerInfo, 0, len(g.players))
	for _, id := range slices.Sorted(maps.Keys(g.players)) {
		p := g.players[id]
		out = append(out, PlayerInfo{ID: p.ID, Name: p.Name, Shape: p.Shape,
			FX: p.fx, FY: p.fy, X: p.tx, Y: p.ty, T: p.t0})
	}
	return out
}

// recentChat returns the last few chat events visible to the given player.
func (g *Game) recentChat(playerID int) []Event {
	out := make([]Event, 0, recentChatLen)
	for i := len(g.events) - 1; i >= 0 && len(out) < recentChatLen; i-- {
		if e := g.events[i]; e.Type == "chat" && e.visibleTo(playerID) {
			out = append(out, e)
		}
	}
	slices.Reverse(out)
	return out
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > maxNameLen {
		writeError(w, http.StatusBadRequest, "invalid name")
		return
	}

	g := s.game(r.PathValue("id"), true)
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.players) >= maxPlayers {
		writeError(w, http.StatusConflict, "game is full")
		return
	}

	now := g.now()
	p := &Player{ID: g.nextID, Name: name, Shape: g.pickShape(), token: newToken(), t0: now, lastSeen: time.Now()}
	g.nextID++
	angle := float64(p.ID) * math.Pi / 3 // spawn in a ring around the center
	p.fx = worldW/2 + 80*math.Cos(angle)
	p.fy = worldH/2 + 80*math.Sin(angle)
	p.tx, p.ty = p.fx, p.fy
	g.players[p.ID] = p
	g.lastActive = time.Now()
	g.appendEvent(Event{T: now, From: p.ID, Type: "join", Name: name, Shape: p.Shape, FX: p.fx, FY: p.fy, X: p.tx, Y: p.ty})

	log.Printf("game %q: %s joined as player %d (%s)", g.id, name, p.ID, p.Shape)
	writeJSON(w, joinResponse{
		Player:     p.ID,
		Token:      p.token,
		ServerTime: now,
		Speed:      moveSpeed,
		Players:    g.snapshotPlayers(),
		RecentChat: g.recentChat(p.ID),
		Last:       len(g.events),
	})
}

type syncRequest struct {
	Player int           `json:"player"`
	Token  string        `json:"token"`
	After  int           `json:"after"` // last sequence number the client has seen
	Events []ClientEvent `json:"events"`
}

type syncResponse struct {
	ServerTime float64 `json:"serverTime"`
	Last       int     `json:"last"` // sequence number to send as "after" next time
	Events     []Event `json:"events"`
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req syncRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	reject := func(status int, msg string) {
		log.Printf("game %q: rejected sync from player %d (%s): %s", id, req.Player, r.RemoteAddr, msg)
		writeError(w, status, msg)
	}
	g := s.game(id, false)
	if g == nil {
		reject(http.StatusNotFound, "no such game")
		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.players[req.Player]
	if p == nil {
		// Usually a player dropped for not syncing within playerTimeout.
		reject(http.StatusGone, "not in this game")
		return
	}
	if p.token != req.Token {
		reject(http.StatusForbidden, "bad token")
		return
	}
	if req.After < 0 || req.After > len(g.events) {
		reject(http.StatusBadRequest, fmt.Sprintf("invalid 'after' %d (log has %d events)", req.After, len(g.events)))
		return
	}
	// Log near misses, to help diagnose players being dropped.
	if gap := time.Since(p.lastSeen); gap > 2*time.Second {
		log.Printf("game %q: player %d (%s) synced after a %.1fs gap", id, p.ID, p.Name, gap.Seconds())
	}
	p.lastSeen = time.Now()
	g.lastActive = p.lastSeen

	now := g.now()
	for _, ce := range req.Events {
		g.apply(p, ce, now)
	}

	out := make([]Event, 0)
	for _, e := range g.events[req.After:] {
		if e.visibleTo(p.ID) {
			out = append(out, e)
		}
	}
	writeJSON(w, syncResponse{ServerTime: now, Last: len(g.events), Events: out})
}

// reapIdle periodically drops players who have stopped syncing, and
// discards games nobody has synced recently.
func (s *Server) reapIdle() {
	for now := range time.Tick(time.Second) {
		s.mu.Lock()
		for id, g := range s.games {
			g.mu.Lock()
			g.dropIdlePlayers(now)
			idle := now.Sub(g.lastActive) > gameIdleTTL
			g.mu.Unlock()
			if idle {
				delete(s.games, id)
				log.Printf("game %q: discarded after %v idle", id, gameIdleTTL)
			}
		}
		s.mu.Unlock()
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		log.Printf("%s %s from %s: bad JSON: %v", r.Method, r.URL.Path, r.RemoteAddr, err)
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing response: %v", err)
	}
}

// writeError replies with an HTTP error status and a JSON error body.
// Note that Mini Micro's http.post discards the body of a non-2xx reply and
// returns only the status line, so clients must rely on the status code.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// withCORS allows the web build of Mini Micro to reach the server.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("addr", ":8737", "listen address")
	flag.Parse()

	s := &Server{games: map[string]*Game{}}
	go s.reapIdle()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /games/{id}/join", s.handleJoin)
	mux.HandleFunc("POST /games/{id}/sync", s.handleSync)

	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, withCORS(mux)))
}
