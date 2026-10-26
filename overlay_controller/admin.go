package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The /admin page manages sound commands, the cosmetic catalog and what Dabi's
// shop sells from a browser, so adding or swapping a file never means SSHing
// into the Pi. The page itself is static (overlay/admin/, so a git pull
// updates it like the overlay); this is the JSON API behind it, under
// /admin/api/.
//
// It's built for the LAN/Tailscale: there's no login unless ADMIN_PASSWORD is
// set, and then the browser asks for it (any username) via basic auth.

// adminRequestHeader has to be on every API write. Browsers won't put a custom
// header on a cross-site request without CORS approval, which this server
// never gives, so some random website can't drive the API through the
// streamer's browser.
const adminRequestHeader = "X-Overlay-Admin"

const (
	maxSoundUpload    = 20 << 20 // about two minutes of CD-quality wav
	maxCosmeticUpload = 10 << 20
)

// cosmeticUploadPrefix is where uploaded cosmetic images are served from
// (data/cosmetics on disk). Images whose src doesn't start with it ship with
// the repo, under overlay/assets, and /admin never deletes those.
const cosmeticUploadPrefix = "cosmetics/"

// cosmeticCanvasWidth/Height is Dabling.png's canvas. The overlay stretches
// every cosmetic layer over the whole sprite, so a layer of any other shape
// would be squashed out of line (a same-shape 2x export is fine).
const cosmeticCanvasWidth, cosmeticCanvasHeight = 500, 600

// cosmeticIDPattern is for new ids: one chat word, and safe in a file name.
var cosmeticIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// slotNamePattern is for new slots, which chat types too (!unequip head).
var slotNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// adminError is a refusal the page shows as is, with its HTTP status.
type adminError struct {
	status int
	msg    string
}

func (e *adminError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &adminError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &adminError{http.StatusNotFound, fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &adminError{http.StatusConflict, fmt.Sprintf(format, args...)}
}

type adminServer struct {
	password     string
	static       http.Handler // the overlay dir, which holds the page at admin/
	sounds       *soundBoard
	cosmetics    *cosmeticCatalogStore
	cosmeticsDir string
	botCommands  *botCommandStore
	party        *partyManager
	tavern       *tavernManager
	shop         *shopStore
}

func (a *adminServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.password != "" {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="overlay admin", charset="UTF-8"`)
			http.Error(w, "password required", http.StatusUnauthorized)
			return
		}
	}
	if !strings.HasPrefix(r.URL.Path, "/admin/api/") {
		a.static.ServeHTTP(w, r)
		return
	}

	var result any
	var err error
	if r.Method != http.MethodGet && r.Header.Get(adminRequestHeader) == "" {
		err = &adminError{http.StatusForbidden, "API writes need the " + adminRequestHeader + " header"}
	} else {
		result, err = a.route(w, r)
	}
	var refusal *adminError
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		if r.Method != http.MethodGet {
			log.Printf("admin: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, result)
	case errors.As(err, &refusal):
		writeJSON(w, refusal.status, map[string]string{"error": refusal.msg})
	case errors.As(err, &tooBig):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("that's over the %d MB limit", tooBig.Limit>>20)})
	default:
		log.Printf("admin: %s %s: %v", r.Method, r.URL.Path, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// route maps METHOD /admin/api/<resource>[/<name>[/<action>]] to a handler.
func (a *adminServer) route(w http.ResponseWriter, r *http.Request) (any, error) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/api/"), "/"), "/")
	shape := []string{r.Method, parts[0]}
	var name string
	if len(parts) > 1 {
		name = parts[1]
		shape = append(append(shape, ":name"), parts[2:]...)
	}

	switch strings.Join(shape, " ") {
	case "GET sounds":
		return a.soundState()
	case "POST sounds":
		return a.addSound(w, r)
	case "PUT sounds :name":
		return a.updateSound(w, r, name)
	case "DELETE sounds :name":
		if err := a.sounds.remove(name); err != nil {
			return nil, err
		}
		return a.soundState()
	case "POST sounds :name file":
		audio, err := readUpload(w, r, maxSoundUpload)
		if err != nil {
			return nil, err
		}
		return a.sounds.replace(name, audio)
	case "POST sounds :name play":
		return map[string]bool{"ok": true}, a.sounds.preview(name)

	case "GET cosmetics":
		return a.cosmeticState(), nil
	case "POST cosmetics":
		return a.addCosmetic(w, r)
	case "PUT cosmetics :name":
		return a.setCosmeticSlot(w, r, name)
	case "DELETE cosmetics :name":
		return a.deleteCosmetic(name)
	case "POST cosmetics :name image":
		return a.replaceCosmeticImage(w, r, name)
	case "PUT slots":
		return a.setSlots(w, r)

	case "GET guide":
		return a.guideState(), nil

	case "GET shop":
		return a.shop.adminState(), nil
	case "PUT shop":
		return a.setShop(w, r)
	}
	return nil, notFound("no such endpoint: %s %s", r.Method, r.URL.Path)
}

func (a *adminServer) soundState() (any, error) {
	sounds, err := a.sounds.list()
	if err != nil {
		return nil, err
	}
	return map[string]any{"sounds": sounds, "default_cooldown_seconds": defaultSoundCooldownSeconds}, nil
}

// checkNotBotCommand refuses sound names that a #a text command already uses,
// since both would answer the same message.
func (a *adminServer) checkNotBotCommand(name string) error {
	if a.botCommands == nil {
		return nil
	}
	if _, ok := a.botCommands.lookup("!" + name); ok {
		return conflict("!%s is already a chat command (one added with #a)", name)
	}
	return nil
}

// addSound takes a multipart form: name, cooldown_seconds, volume and file.
func (a *adminServer) addSound(w http.ResponseWriter, r *http.Request) (any, error) {
	audio, err := readUpload(w, r, maxSoundUpload)
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	settings := defaultSoundSettings()
	for field, dst := range map[string]*int{"cooldown_seconds": &settings.CooldownSeconds, "volume": &settings.Volume} {
		if v := strings.TrimSpace(r.FormValue(field)); v != "" {
			if *dst, err = strconv.Atoi(v); err != nil {
				return nil, badRequest("%s has to be a whole number", field)
			}
		}
	}
	if err := a.checkNotBotCommand(name); err != nil {
		return nil, err
	}
	if _, err := a.sounds.add(name, audio, settings); err != nil {
		return nil, err
	}
	return a.soundState()
}

// updateSound takes JSON {name, cooldown_seconds, volume}; a different name
// renames the command.
func (a *adminServer) updateSound(w http.ResponseWriter, r *http.Request, name string) (any, error) {
	var req struct {
		Name            string `json:"name"`
		CooldownSeconds *int   `json:"cooldown_seconds"`
		Volume          *int   `json:"volume"`
	}
	if err := readJSON(w, r, &req); err != nil {
		return nil, err
	}
	if req.CooldownSeconds == nil || req.Volume == nil {
		return nil, badRequest("send cooldown_seconds and volume")
	}
	newName := strings.ToLower(strings.TrimSpace(req.Name))
	if newName == "" {
		newName = name
	}
	if newName != name {
		if err := a.checkNotBotCommand(newName); err != nil {
			return nil, err
		}
	}
	return a.sounds.update(name, newName, soundSettings{CooldownSeconds: *req.CooldownSeconds, Volume: *req.Volume})
}

type cosmeticView struct {
	ID   string `json:"id"`
	Slot string `json:"slot"`
	Src  string `json:"src"`
	URL  string `json:"url"`
}

// overlayURL makes a cosmetic's src, which is relative to the overlay page at
// /, work from pages that live elsewhere (/admin/, /shop/).
func overlayURL(src string) string {
	if !strings.HasPrefix(src, "/") && !strings.Contains(src, "://") {
		return "/" + src
	}
	return src
}

// cosmeticState is the whole catalog as the page draws it. Every write returns
// it too, so the page simply redraws.
func (a *adminServer) cosmeticState() any {
	cat := a.cosmetics.get()
	items := make([]cosmeticView, 0, len(cat.Items))
	for _, id := range cat.ids() {
		item := cat.Items[id]
		items = append(items, cosmeticView{ID: id, Slot: item.Slot, Src: item.Src, URL: overlayURL(item.Src)})
	}
	slots := cat.Slots
	if slots == nil {
		slots = []string{}
	}
	return map[string]any{
		"slots":  slots,
		"items":  items,
		"base":   "/assets/Dabling.png",
		"canvas": []int{cosmeticCanvasWidth, cosmeticCanvasHeight},
	}
}

// redrawOverlay re-sends the party cards, the tavern roster and the shop, so
// an edited cosmetic shows on stream without waiting for anyone to chat.
func (a *adminServer) redrawOverlay() {
	if a.party != nil {
		a.party.notifyChange()
	}
	if a.tavern != nil {
		a.tavern.redraw()
	}
	if a.shop != nil {
		a.shop.broadcast()
	}
}

// setShop takes JSON {boxes: [{item, price}, ...]}, one entry per box, with
// "" as the item for an empty box.
func (a *adminServer) setShop(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		Boxes []shopBox `json:"boxes"`
	}
	if err := readJSON(w, r, &req); err != nil {
		return nil, err
	}
	if err := a.shop.set(r.Context(), req.Boxes); err != nil {
		return nil, err
	}
	return a.shop.adminState(), nil
}

// addCosmetic takes a multipart form: id, slot and file.
func (a *adminServer) addCosmetic(w http.ResponseWriter, r *http.Request) (any, error) {
	data, err := readUpload(w, r, maxCosmeticUpload)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(r.FormValue("id"))
	slot := strings.TrimSpace(r.FormValue("slot"))
	if !cosmeticIDPattern.MatchString(id) {
		return nil, badRequest("%q won't work as a cosmetic name: use up to 32 letters, digits, - and _", id)
	}
	if err := checkCosmeticPNG(data); err != nil {
		return nil, err
	}
	src, err := a.storeCosmeticImage(id, data)
	if err != nil {
		return nil, err
	}
	err = a.cosmetics.update(func(cat *cosmeticCatalog) error {
		if existing, _, ok := cat.lookup(id); ok {
			return conflict("there's already a cosmetic called %s", existing)
		}
		if !slices.Contains(cat.Slots, slot) {
			return badRequest("there's no %q slot", slot)
		}
		cat.Items[id] = cosmeticItem{Slot: slot, Src: src}
		return nil
	})
	if err != nil {
		a.dropCosmeticImage(src)
		return nil, err
	}
	a.redrawOverlay()
	return a.cosmeticState(), nil
}

// replaceCosmeticImage swaps an item's art, keeping its id, so everyone who
// owns it sees the new version.
func (a *adminServer) replaceCosmeticImage(w http.ResponseWriter, r *http.Request, name string) (any, error) {
	data, err := readUpload(w, r, maxCosmeticUpload)
	if err != nil {
		return nil, err
	}
	if err := checkCosmeticPNG(data); err != nil {
		return nil, err
	}
	id, _, ok := a.cosmetics.get().lookup(name)
	if !ok {
		return nil, notFound("there's no cosmetic called %s", name)
	}
	src, err := a.storeCosmeticImage(id, data)
	if err != nil {
		return nil, err
	}
	var old string
	err = a.cosmetics.update(func(cat *cosmeticCatalog) error {
		item, ok := cat.Items[id]
		if !ok {
			return notFound("there's no cosmetic called %s", id)
		}
		old, item.Src = item.Src, src
		cat.Items[id] = item
		return nil
	})
	if err != nil {
		a.dropCosmeticImage(src)
		return nil, err
	}
	a.dropCosmeticImage(old)
	a.redrawOverlay()
	return a.cosmeticState(), nil
}

// setCosmeticSlot takes JSON {slot}. Whoever was wearing the item has to
// !equip it again, since what they wear is stored per slot.
func (a *adminServer) setCosmeticSlot(w http.ResponseWriter, r *http.Request, name string) (any, error) {
	var req struct {
		Slot string `json:"slot"`
	}
	if err := readJSON(w, r, &req); err != nil {
		return nil, err
	}
	err := a.cosmetics.update(func(cat *cosmeticCatalog) error {
		id, item, ok := cat.lookup(name)
		if !ok {
			return notFound("there's no cosmetic called %s", name)
		}
		if !slices.Contains(cat.Slots, req.Slot) {
			return badRequest("there's no %q slot", req.Slot)
		}
		item.Slot = req.Slot
		cat.Items[id] = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	a.redrawOverlay()
	return a.cosmeticState(), nil
}

// deleteCosmetic drops an item from the catalog. Chatters who own it keep it
// in their saved wardrobe, where it's skipped like any retired item (and comes
// back if an item with the same id is added again).
func (a *adminServer) deleteCosmetic(name string) (any, error) {
	var old string
	err := a.cosmetics.update(func(cat *cosmeticCatalog) error {
		id, item, ok := cat.lookup(name)
		if !ok {
			return notFound("there's no cosmetic called %s", name)
		}
		old = item.Src
		delete(cat.Items, id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	a.dropCosmeticImage(old)
	a.redrawOverlay()
	return a.cosmeticState(), nil
}

// setSlots takes JSON {slots}: the full list in draw order, which is how the
// page adds, removes and reorders them.
func (a *adminServer) setSlots(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		Slots []string `json:"slots"`
	}
	if err := readJSON(w, r, &req); err != nil {
		return nil, err
	}
	err := a.cosmetics.update(func(cat *cosmeticCatalog) error {
		for _, slot := range req.Slots {
			if !slices.Contains(cat.Slots, slot) && !slotNamePattern.MatchString(slot) {
				return badRequest("%q won't work as a slot name: use up to 32 lowercase letters, digits, - and _", slot)
			}
		}
		for _, id := range cat.ids() {
			if slot := cat.Items[id].Slot; !slices.Contains(req.Slots, slot) {
				return conflict("%s is in the %s slot, so move or delete it first", id, slot)
			}
		}
		cat.Slots = req.Slots
		return nil
	})
	if err != nil {
		return nil, err
	}
	a.redrawOverlay()
	return a.cosmeticState(), nil
}

// guideLevels is how far the party guide's level table goes.
const guideLevels = 10

// guideState feeds the Party guide tab the rules as the code has them now,
// so the page stays right when they're tuned.
func (a *adminServer) guideState() any {
	newcomer := expPerMessage(0)
	levels := make([]map[string]any, 0, guideLevels)
	for level := 1; level <= guideLevels; level++ {
		exp := totalExpForLevel(level)
		levels = append(levels, map[string]any{
			"level":    level,
			"exp":      exp,
			"max_hp":   maxHPForLevel(level),
			"messages": (exp + newcomer - 1) / newcomer, // for a new chatter to get there
		})
	}
	rates := make([]map[string]int64, 0, 3)
	for _, logins := range []int64{0, 25, 100} {
		rates = append(rates, map[string]int64{"logins": logins, "exp": expPerMessage(logins)})
	}
	voiceTiers := make([]int64, 0, len(ttsVoiceTiers))
	for _, tier := range ttsVoiceTiers {
		if tier.minLogins > 0 {
			voiceTiers = append(voiceTiers, tier.minLogins)
		}
	}
	redeemFor := map[string]string{loginsCounter: dailyLoginRewardTitle, firstLoginsCounter: firstLoginRewardTitle}
	rewards := make([]map[string]any, 0, len(autoRewards))
	for _, r := range autoRewards {
		rewards = append(rewards, map[string]any{
			"counter":  r.Counter,
			"redeem":   redeemFor[r.Counter],
			"count":    r.AtLeast,
			"cosmetic": r.Cosmetic,
			"noun":     r.Noun,
		})
	}
	return map[string]any{
		"redeems": map[string]string{
			"join":        joinPartyRewardTitle,
			"daily_login": dailyLoginRewardTitle,
			"first_login": firstLoginRewardTitle,
		},
		"party_size":           partyMaxSize,
		"exp_rates":            rates,
		"exp_cooldown_seconds": int(expCooldownDuration / time.Second),
		"voice_tiers":          voiceTiers,
		"revive_levels":        reviveDefaultLevels,
		"sheet_seconds":        int(sheetDuration / time.Second),
		"tavern_idle_minutes":  int(tavernIdleTimeout / time.Minute),
		"levels":               levels,
		"auto_rewards":         rewards,
	}
}

// storeCosmeticImage saves an upload as <id>-<hash>.png. The hash gives new
// art a new URL, so the overlay redraws with it instead of a cached copy.
func (a *adminServer) storeCosmeticImage(id string, data []byte) (string, error) {
	if err := os.MkdirAll(a.cosmeticsDir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	file := fmt.Sprintf("%s-%x.png", id, sum[:4])
	if err := writeFileAtomic(filepath.Join(a.cosmeticsDir, file), data); err != nil {
		return "", err
	}
	return cosmeticUploadPrefix + file, nil
}

// dropCosmeticImage deletes an uploaded image once no item uses it. Art that
// ships in overlay/assets is never touched.
func (a *adminServer) dropCosmeticImage(src string) {
	file, ok := strings.CutPrefix(src, cosmeticUploadPrefix)
	if !ok || file != filepath.Base(file) || !strings.HasSuffix(file, ".png") {
		return
	}
	for _, item := range a.cosmetics.get().Items {
		if item.Src == src {
			return
		}
	}
	if err := os.Remove(filepath.Join(a.cosmeticsDir, file)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("admin: couldn't delete %s: %v", file, err)
	}
}

// checkCosmeticPNG refuses images that would draw wrong over the Dabling: not
// a PNG, the wrong shape, or no transparency at all (its background would
// cover the whole body).
func checkCosmeticPNG(data []byte) error {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return badRequest("that isn't a PNG file")
	}
	if cfg.Width*cosmeticCanvasHeight != cfg.Height*cosmeticCanvasWidth {
		return badRequest("cosmetics have to be on Dabling.png's %dx%d canvas, and this one is %dx%d", cosmeticCanvasWidth, cosmeticCanvasHeight, cfg.Width, cfg.Height)
	}
	if cfg.Width > 4*cosmeticCanvasWidth {
		return badRequest("that's %dx%d, much bigger than it needs to be: export it at %dx%d", cfg.Width, cfg.Height, cosmeticCanvasWidth, cosmeticCanvasHeight)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return badRequest("couldn't read that PNG: %v", err)
	}
	if opaque, ok := img.(interface{ Opaque() bool }); ok && opaque.Opaque() {
		return badRequest("that PNG has no transparent pixels, so export it with a transparent background")
	}
	return nil
}

// readUpload reads the "file" part of a multipart upload, up to limit bytes.
func readUpload(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20) // headroom for the other fields
	if err := r.ParseMultipartForm(limit); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, &http.MaxBytesError{Limit: limit}
		}
		return nil, badRequest("couldn't read the upload: %v", err)
	}
	defer r.MultipartForm.RemoveAll()
	file, _, err := r.FormFile("file")
	if err != nil {
		return nil, badRequest("the upload has no file in it")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}
	return data, nil
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(dst); err != nil {
		return badRequest("couldn't read the request: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeFileAtomic writes through a temp file and a rename, so a reader (the
// browser source, the catalog reloader) never sees a half-written file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has happened
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// serveMedia serves uploaded files from dir, with the given extensions only:
// no directory listings, and none of the JSON kept beside them.
func serveMedia(dir string, exts ...string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(exts, strings.ToLower(path.Ext(r.URL.Path))) {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
