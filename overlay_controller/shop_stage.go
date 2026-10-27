package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// The shop's OBS side: !openshop switches OBS to the Dabi-Shop scene and the
// stream music to the shop's mood (polite-shop.mp3), and !closeshop puts
// both back. It goes through pd-streamdeck, which already holds the OBS
// connection and plays the stream music from the Pi, so the music crossfades
// instead of two tracks playing at once. What the shop replaced is kept in
// shop.json, so a restart while it's open still knows what to go back to.

const defaultShopScene = "Dabi-Shop"
const defaultShopMood = "shop"

// shopBefore is what was on when the shop opened: the OBS scene, and the
// music's mood if any was playing.
type shopBefore struct {
	Scene string `json:"scene,omitempty"`
	Mood  string `json:"mood,omitempty"`
}

// deckState is the part of pd-streamdeck's GET /api/state the shop needs.
type deckState struct {
	OBS struct {
		CurrentScene string `json:"current_scene"`
	} `json:"obs"`
	Music struct {
		Playing bool   `json:"playing"`
		Mood    string `json:"mood"`
	} `json:"music"`
}

// deckClient calls pd-streamdeck's HTTP API with the deck's token.
type deckClient struct {
	url   string
	token string
	http  *http.Client
}

func newDeckClient(url, token string) *deckClient {
	return &deckClient{url: strings.TrimRight(url, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

func (d *deckClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.url+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Deck-Token", d.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		// The deck's own errors are {"error": ...}; FastAPI's (a bad token) are {"detail": ...}.
		var problem struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(data, &problem)
		why := firstString(problem.Error, problem.Detail, strings.TrimSpace(string(data)))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, why)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (d *deckClient) state(ctx context.Context) (deckState, error) {
	var resp struct {
		State deckState `json:"state"`
	}
	err := d.do(ctx, http.MethodGet, "/api/state", nil, &resp)
	return resp.State, err
}

// shopStage switches OBS and the music for the shop, one open or close at a
// time and off the chat handler, so a slow or missing deck never holds up chat.
type shopStage struct {
	deck  *deckClient // nil: no deck set up, so the shop leaves OBS alone
	shop  *shopStore
	scene string
	mood  string
	jobs  chan bool // true opens, false closes, in the order they were asked for
}

func newShopStage(deck *deckClient, shop *shopStore, scene, mood string) *shopStage {
	return &shopStage{deck: deck, shop: shop, scene: scene, mood: mood, jobs: make(chan bool, 8)}
}

func (st *shopStage) run(ctx context.Context) {
	for {
		select {
		case open := <-st.jobs:
			if open {
				st.open(ctx)
			} else {
				st.close(ctx)
			}
		case <-ctx.Done():
			return
		}
	}
}

// queue asks for the shop's scene and music, or for what was on before.
func (st *shopStage) queue(open bool) {
	if st == nil || st.deck == nil {
		return
	}
	select {
	case st.jobs <- open:
	default:
		log.Print("shop: too many !openshop/!closeshop at once, skipping one")
	}
}

// open remembers what's on, unless OBS is already on the shop (a repeated
// !openshop), then switches to the shop's scene and mood.
func (st *shopStage) open(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	state, err := st.deck.state(opCtx)
	if err != nil {
		log.Printf("shop: can't reach pd-streamdeck, so OBS and the music stay as they are: %v", err)
		return
	}
	if state.OBS.CurrentScene != st.scene {
		before := &shopBefore{Scene: state.OBS.CurrentScene}
		if state.Music.Playing && state.Music.Mood != st.mood {
			before.Mood = state.Music.Mood
		}
		if err := st.shop.setBefore(before); err != nil {
			log.Printf("shop: failed to save what was on before: %v", err)
		}
	}
	if err := st.deck.do(opCtx, http.MethodPost, "/api/obs/scene", map[string]string{"scene": st.scene}, nil); err != nil {
		log.Printf("shop: couldn't switch OBS to the %s scene (does OBS have one called that?): %v", st.scene, err)
	}
	if err := st.deck.do(opCtx, http.MethodPost, "/api/music/play", map[string]string{"mood": st.mood}, nil); err != nil {
		log.Printf("shop: couldn't play the %s music (pd-streamdeck plays it from songs/%s, which a rescan picks up): %v", st.mood, st.mood, err)
	}
}

// close puts back what the shop replaced, but only where the shop is still
// showing: a scene or music changed by hand since is left alone.
func (st *shopStage) close(ctx context.Context) {
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	state, err := st.deck.state(opCtx)
	if err != nil {
		// What was on before is kept, so another !closeshop can still put it back.
		log.Printf("shop: can't reach pd-streamdeck, so OBS and the music stay as they are: %v", err)
		return
	}
	before := st.shop.takeBefore()
	if state.OBS.CurrentScene == st.scene {
		if before == nil || before.Scene == "" {
			log.Printf("shop: don't know which scene was on before the shop, so OBS stays on %s", st.scene)
		} else if err := st.deck.do(opCtx, http.MethodPost, "/api/obs/scene", map[string]string{"scene": before.Scene}, nil); err != nil {
			log.Printf("shop: couldn't switch OBS back to %s: %v", before.Scene, err)
		}
	}
	if state.Music.Playing && state.Music.Mood == st.mood {
		if before != nil && before.Mood != "" {
			err = st.deck.do(opCtx, http.MethodPost, "/api/music/play", map[string]string{"mood": before.Mood}, nil)
		} else {
			err = st.deck.do(opCtx, http.MethodPost, "/api/music/stop", nil, nil)
		}
		if err != nil {
			log.Printf("shop: couldn't put the music back: %v", err)
		}
	}
}
