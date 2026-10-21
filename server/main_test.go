package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer() (*Server, http.Handler) {
	s := &Server{games: map[string]*Game{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /games/{id}/join", s.handleJoin)
	mux.HandleFunc("POST /games/{id}/sync", s.handleSync)
	return s, withCORS(mux)
}

func newTestHandler() http.Handler {
	_, h := newTestServer()
	return h
}

func post(t *testing.T, h http.Handler, path string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(string(b))))
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s: bad response %q: %v", path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

func TestJoinAndSync(t *testing.T) {
	h := newTestHandler()
	var alice, bob joinResponse
	post(t, h, "/games/g1/join", joinRequest{Name: "Alice"}, &alice)
	post(t, h, "/games/g1/join", joinRequest{Name: "Bob"}, &bob)
	if alice.Player != 1 || bob.Player != 2 || alice.Token == "" {
		t.Fatalf("unexpected joins: %+v %+v", alice, bob)
	}

	var resp syncResponse
	post(t, h, "/games/g1/sync", syncRequest{
		Player: alice.Player, Token: alice.Token,
		Events: []ClientEvent{{Type: "move", X: 100, Y: 100}, {Type: "chat", Text: " hi "}, {Type: "bogus"}},
	}, &resp)
	types := []string{}
	for _, e := range resp.Events {
		types = append(types, e.Type)
	}
	if got := strings.Join(types, ","); got != "join,join,move,chat" || resp.Last != 4 {
		t.Fatalf("got events %s, last %d", got, resp.Last)
	}
	if resp.Events[3].Text != "hi" {
		t.Errorf("chat text not trimmed: %q", resp.Events[3].Text)
	}

	// Bob, having seen through seq 2, gets only the new events.
	post(t, h, "/games/g1/sync", syncRequest{Player: bob.Player, Token: bob.Token, After: 2}, &resp)
	if len(resp.Events) != 2 || resp.Events[0].Seq != 3 {
		t.Fatalf("bob got %+v", resp.Events)
	}

	if code := post(t, h, "/games/g1/sync", syncRequest{Player: 1, Token: "wrong"}, nil); code != http.StatusForbidden {
		t.Errorf("bad token: got status %d", code)
	}
}

func TestGameFull(t *testing.T) {
	h := newTestHandler()
	for i := 0; i < maxPlayers; i++ {
		post(t, h, "/games/g/join", joinRequest{Name: "P"}, nil)
	}
	if code := post(t, h, "/games/g/join", joinRequest{Name: "Late"}, nil); code != http.StatusConflict {
		t.Errorf("seventh join: got status %d", code)
	}
}

func TestJoinSnapshot(t *testing.T) {
	s, h := newTestServer()
	var alice, bob joinResponse
	post(t, h, "/games/g/join", joinRequest{Name: "Alice"}, &alice)
	post(t, h, "/games/g/join", joinRequest{Name: "Bob"}, &bob)
	var chats []ClientEvent
	for i := 1; i <= recentChatLen+3; i++ {
		chats = append(chats, ClientEvent{Type: "chat", Text: fmt.Sprint("msg ", i)})
	}
	chats = append(chats, ClientEvent{Type: "move", X: 10, Y: 20})
	post(t, h, "/games/g/sync", syncRequest{Player: alice.Player, Token: alice.Token, Events: chats}, nil)

	// Bob leaves; Carol joins late.
	g := s.games["g"]
	g.players[bob.Player].lastSeen = time.Now().Add(-playerTimeout - time.Second)
	g.dropIdlePlayers(time.Now())
	var carol joinResponse
	post(t, h, "/games/g/join", joinRequest{Name: "Carol"}, &carol)

	if len(carol.Players) != 2 || carol.Players[0].Name != "Alice" || carol.Players[1].Name != "Carol" {
		t.Fatalf("snapshot players: %+v", carol.Players)
	}
	if a := carol.Players[0]; a.X != 10 || a.Y != 20 || a.Shape == "" {
		t.Errorf("Alice's snapshot motion/shape: %+v", a)
	}
	if n := len(carol.RecentChat); n != recentChatLen {
		t.Fatalf("got %d recent chats", n)
	}
	if first, last := carol.RecentChat[0], carol.RecentChat[recentChatLen-1]; first.Text != "msg 4" ||
		last.Text != fmt.Sprint("msg ", recentChatLen+3) || last.Name != "Alice" {
		t.Errorf("recent chat runs %+v .. %+v", first, last)
	}
	if carol.Last != len(g.events) {
		t.Errorf("last = %d, want %d", carol.Last, len(g.events))
	}

	// Carol's first sync, from her own join, has nothing old in it.
	var resp syncResponse
	post(t, h, "/games/g/sync", syncRequest{Player: carol.Player, Token: carol.Token, After: carol.Last}, &resp)
	if len(resp.Events) != 0 {
		t.Errorf("first sync returned old events: %+v", resp.Events)
	}
}

func TestUniqueShapes(t *testing.T) {
	s, h := newTestServer()
	for i := 0; i < maxPlayers; i++ {
		post(t, h, "/games/g/join", joinRequest{Name: "P"}, nil)
	}
	seen := map[string]bool{}
	for _, p := range s.games["g"].players {
		if p.Shape == "" || seen[p.Shape] {
			t.Fatalf("missing or duplicate shape %q", p.Shape)
		}
		seen[p.Shape] = true
	}
}

func TestDropIdlePlayers(t *testing.T) {
	s, h := newTestServer()
	var alice, bob joinResponse
	post(t, h, "/games/g/join", joinRequest{Name: "Alice"}, &alice)
	post(t, h, "/games/g/join", joinRequest{Name: "Bob"}, &bob)

	g := s.games["g"]
	g.players[alice.Player].lastSeen = time.Now().Add(-playerTimeout - time.Second)
	g.dropIdlePlayers(time.Now())

	var resp syncResponse
	post(t, h, "/games/g/sync", syncRequest{Player: bob.Player, Token: bob.Token, After: 2}, &resp)
	if len(resp.Events) != 1 || resp.Events[0].Type != "leave" || resp.Events[0].From != alice.Player {
		t.Fatalf("bob got %+v", resp.Events)
	}
	if code := post(t, h, "/games/g/sync", syncRequest{Player: alice.Player, Token: alice.Token}, nil); code != http.StatusGone {
		t.Errorf("dropped player sync: got status %d", code)
	}
}

func TestActionTime(t *testing.T) {
	p := &Player{t0: 8}
	cases := []struct{ clientT, want float64 }{
		{9.8, 9.8}, // recent: honored
		{0, 10},    // missing: use now
		{12, 10},   // future: clamped to now
		{5, 9.5},   // too old: clamped to now - maxActionLag
	}
	for _, c := range cases {
		if got := actionTime(p, c.clientT, 10); got != c.want {
			t.Errorf("actionTime(%v) = %v, want %v", c.clientT, got, c.want)
		}
	}
	p.t0 = 9.9 // can't backdate before the previous move
	if got := actionTime(p, 9.6, 10); got != 9.9 {
		t.Errorf("before previous move: got %v", got)
	}
}

func TestPosAt(t *testing.T) {
	p := &Player{fx: 0, fy: 0, tx: 400, ty: 0, t0: 10}
	if x, _ := p.posAt(11); x != moveSpeed {
		t.Errorf("after 1s: x=%v", x)
	}
	if x, _ := p.posAt(100); x != 400 {
		t.Errorf("should stop at destination: x=%v", x)
	}
}
