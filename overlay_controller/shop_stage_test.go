package main

// Tests for the shop's OBS side: what !openshop and !closeshop ask
// pd-streamdeck to do, against a fake deck. Closing should put OBS and the
// music back where they were, and never fight a change made by hand.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeDeck is just enough of pd-streamdeck's API: the scene and music it
// reports, and every change asked of it.
type fakeDeck struct {
	mu      sync.Mutex
	scene   string
	playing bool
	mood    string
	asked   []string
	fail    int // when set, every call answers with this status
}

func (f *fakeDeck) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("X-Deck-Token") != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"detail": "Bad or missing deck token"})
		return
	}
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "OBS is not connected"})
		return
	}
	var body struct {
		Scene string `json:"scene"`
		Mood  string `json:"mood"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.Method + " " + r.URL.Path {
	case "GET /api/state":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "state": map[string]any{
			"obs":   map[string]any{"current_scene": f.scene},
			"music": map[string]any{"playing": f.playing, "mood": f.mood},
		}})
		return
	case "POST /api/obs/scene":
		f.scene = body.Scene
		f.asked = append(f.asked, "scene "+body.Scene)
	case "POST /api/music/play":
		f.playing, f.mood = true, body.Mood
		f.asked = append(f.asked, "play "+body.Mood)
	case "POST /api/music/stop":
		f.playing = false
		f.asked = append(f.asked, "stop")
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// took hands back, and forgets, what the deck was asked to do.
func (f *fakeDeck) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	asked := f.asked
	f.asked = nil
	return asked
}

func (f *fakeDeck) failWith(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = status
}

func (f *fakeDeck) set(scene string, playing bool, mood string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scene, f.playing, f.mood = scene, playing, mood
}

func testStage(t *testing.T, f *fakeDeck, token string) (*shopStage, *shopStore, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, path, _ := testShop(t)
	s.stage = newShopStage(newDeckClient(srv.URL, token), s, defaultShopScene, defaultShopMood)
	return s.stage, s, path
}

func TestShopStageOpensAndCloses(t *testing.T) {
	f := &fakeDeck{scene: "Main", playing: true, mood: "chill"}
	st, s, path := testStage(t, f, "secret")
	ctx := context.Background()

	st.open(ctx)
	if got, want := f.took(), []string{"scene Dabi-Shop", "play shop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("opening asked for %v, want %v", got, want)
	}
	want := &shopBefore{Scene: "Main", Mood: "chill"}
	if got := newShopStore(path, newOverlayHub(), s.cosmetics, nil).stock.Before; !reflect.DeepEqual(got, want) {
		t.Errorf("saved what was on as %+v, want %+v (it has to survive a restart)", got, want)
	}

	st.close(ctx)
	if got, want := f.took(), []string{"scene Main", "play chill"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closing asked for %v, want %v", got, want)
	}
	if s.stock.Before != nil {
		t.Errorf("still remembers %+v after closing", s.stock.Before)
	}
}

// A second !openshop while OBS is already on the shop mustn't record the shop
// itself as what to go back to.
func TestShopStageRepeatedOpen(t *testing.T) {
	f := &fakeDeck{scene: "Main", playing: true, mood: "chill"}
	st, s, _ := testStage(t, f, "secret")
	st.open(context.Background())
	st.open(context.Background())
	if want := (&shopBefore{Scene: "Main", Mood: "chill"}); !reflect.DeepEqual(s.stock.Before, want) {
		t.Errorf("after two opens, remembers %+v, want %+v", s.stock.Before, want)
	}
}

// Whatever was changed by hand while the shop was open stays as it is.
func TestShopStageLeavesHandChangesAlone(t *testing.T) {
	f := &fakeDeck{scene: "Main", playing: true, mood: "chill"}
	st, _, _ := testStage(t, f, "secret")
	st.open(context.Background())
	f.took()
	f.set("BRB", true, "hype")
	st.close(context.Background())
	if got := f.took(); len(got) != 0 {
		t.Errorf("closing asked for %v after the scene and music were changed by hand", got)
	}
}

func TestShopStageNoMusicBefore(t *testing.T) {
	f := &fakeDeck{scene: "Main"}
	st, _, _ := testStage(t, f, "secret")
	st.open(context.Background())
	f.took()
	st.close(context.Background())
	if got, want := f.took(), []string{"scene Main", "stop"}; !reflect.DeepEqual(got, want) {
		t.Errorf("closing asked for %v, want %v: no music was on before", got, want)
	}
}

// A wrong token or a deck that can't reach OBS gets logged, and nothing else.
func TestShopStageDeckProblems(t *testing.T) {
	f := &fakeDeck{scene: "Main"}
	st, s, _ := testStage(t, f, "wrong")
	st.open(context.Background())
	if got := f.took(); len(got) != 0 || s.stock.Before != nil {
		t.Errorf("with a bad token: asked %v, remembered %+v", got, s.stock.Before)
	}

	// Closing while the deck is down keeps what was on before, for a retry.
	f2 := &fakeDeck{scene: "Main", playing: true, mood: "chill"}
	st2, s2, _ := testStage(t, f2, "secret")
	st2.open(context.Background())
	f2.took()
	f2.failWith(http.StatusServiceUnavailable)
	st2.close(context.Background())
	if want := (&shopBefore{Scene: "Main", Mood: "chill"}); !reflect.DeepEqual(s2.stock.Before, want) {
		t.Errorf("a close that couldn't reach the deck forgot what was on: %+v", s2.stock.Before)
	}
	f2.failWith(0)
	st2.close(context.Background())
	if got, want := f2.took(), []string{"scene Main", "play chill"}; !reflect.DeepEqual(got, want) {
		t.Errorf("retrying the close asked for %v, want %v", got, want)
	}

	var none *shopStage
	none.queue(true) // no deck set up: nothing to do, and no panic
	(&shopStage{}).queue(true)
}

// Opens and closes run one at a time, in the order chat asked for them.
func TestShopStageQueue(t *testing.T) {
	f := &fakeDeck{scene: "Main", playing: true, mood: "chill"}
	st, _, _ := testStage(t, f, "secret")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go st.run(ctx)
	st.queue(true)
	st.queue(false)

	want := []string{"scene Dabi-Shop", "play shop", "scene Main", "play chill"}
	var got []string
	for deadline := time.Now().Add(5 * time.Second); len(got) < len(want) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got = append(got, f.took()...)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("queued open then close asked for %v, want %v", got, want)
	}
}
