package main

// Tests for /admin. The page's own plumbing is mostly left alone, except what
// would fail silently: the password and cross-site checks (a broken one just
// leaves the API open), the cosmetic catalog writes (a bad one corrupts what
// every Dabling wears), and never deleting art that ships with the repo.

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testPNG encodes a w x h image: opaque fills every pixel, otherwise it's
// transparent apart from one small block, like a real layer.
func testPNG(t *testing.T, w, h int, opaque bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	paint := image.Rect(0, 0, 10, 10)
	if opaque {
		paint = img.Bounds()
	}
	draw.Draw(img, paint, image.NewUniform(color.NRGBA{R: 200, G: 40, B: 40, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testAdmin is an adminServer over a temp data dir whose catalog has one
// uploaded item (shirtA, torso) and one that ships with the repo (hat, head).
func testAdmin(t *testing.T, password string) (*adminServer, string) {
	t.Helper()
	dir := t.TempDir()
	cosmeticsDir := filepath.Join(dir, "cosmetics")
	if err := os.MkdirAll(cosmeticsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cosmeticsDir, "shirtA-1234abcd.png"), testPNG(t, 500, 600, false))
	writeTestFile(t, filepath.Join(cosmeticsDir, "cosmetics.json"), []byte(`{"slots":["torso","head"],"items":{
		"shirtA":{"slot":"torso","src":"cosmetics/shirtA-1234abcd.png"},
		"hat":{"slot":"head","src":"assets/cosmetics/hat.png"}}}`))
	hub := newOverlayHub()
	cosmetics := newCosmeticCatalogStore(filepath.Join(cosmeticsDir, "cosmetics.json"))
	return &adminServer{
		password:     password,
		static:       http.NotFoundHandler(),
		sounds:       newSoundBoard(filepath.Join(dir, "sounds"), hub),
		cosmetics:    cosmetics,
		cosmeticsDir: cosmeticsDir,
		party:        newPartyManager(hub, cosmetics),
		tavern:       newTavernManager(hub, cosmetics),
	}, cosmeticsDir
}

// call sends one API request the way the page does (with the admin header)
// and decodes the JSON reply.
func call(t *testing.T, a *adminServer, method, path string, body io.Reader, contentType string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set(adminRequestHeader, "1")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func upload(t *testing.T, fields map[string]string, file []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	part, err := w.CreateFormFile("file", "upload")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(file)
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func catalogItem(t *testing.T, state map[string]any, id string) map[string]any {
	t.Helper()
	for _, raw := range state["items"].([]any) {
		if item := raw.(map[string]any); item["id"] == id {
			return item
		}
	}
	return nil
}

func TestAdminRefusesCrossSiteWrites(t *testing.T) {
	a, _ := testAdmin(t, "")
	req := httptest.NewRequest(http.MethodDelete, "/admin/api/cosmetics/shirtA", nil) // no admin header
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("headerless DELETE = %d, want 403", rec.Code)
	}
	if _, _, ok := a.cosmetics.get().lookup("shirtA"); !ok {
		t.Error("the headerless DELETE went through")
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/sounds", nil) // reads don't need it
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET = %d, want 200", rec.Code)
	}
}

func TestAdminPassword(t *testing.T) {
	a, _ := testAdmin(t, "hunter2")
	cases := []struct {
		name   string
		user   string
		pass   string
		send   bool
		status int
	}{
		{"no credentials", "", "", false, http.StatusUnauthorized},
		{"wrong password", "pd", "hunter3", true, http.StatusUnauthorized},
		{"right password, any username", "whoever", "hunter2", true, http.StatusOK},
	}
	for _, tc := range cases {
		for _, path := range []string{"/admin/api/sounds", "/admin/"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if tc.send {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, req)
			want := tc.status
			if path == "/admin/" && want == http.StatusOK {
				want = http.StatusNotFound // let through to the (stubbed) static files
			}
			if rec.Code != want {
				t.Errorf("%s, %s: status %d, want %d", tc.name, path, rec.Code, want)
			}
			if want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("%s, %s: 401 without a WWW-Authenticate header, so the browser won't ask", tc.name, path)
			}
		}
	}
}

func TestAdminSoundUpload(t *testing.T) {
	a, _ := testAdmin(t, "")
	body, ctype := upload(t, map[string]string{"name": "Buhee", "cooldown_seconds": "7", "volume": "80"}, fakeWAV)
	status, state := call(t, a, http.MethodPost, "/admin/api/sounds", body, ctype)
	if status != http.StatusOK {
		t.Fatalf("upload = %d %v", status, state)
	}
	sounds := state["sounds"].([]any)
	if len(sounds) != 1 {
		t.Fatalf("sounds = %v, want one", sounds)
	}
	s := sounds[0].(map[string]any)
	if s["name"] != "buhee" || s["file"] != "buhee.wav" || s["cooldown_seconds"] != 7.0 || s["volume"] != 80.0 {
		t.Errorf("uploaded sound = %v", s)
	}

	status, _ = call(t, a, http.MethodPut, "/admin/api/sounds/buhee", strings.NewReader(`{"name":"buhee"}`), "application/json")
	if status != http.StatusBadRequest {
		t.Errorf("PUT without settings = %d, want 400 (a missing volume mustn't save as silence)", status)
	}
}

func TestReadUploadLimit(t *testing.T) {
	body, ctype := upload(t, nil, bytes.Repeat([]byte("x"), 100))
	req := httptest.NewRequest(http.MethodPost, "/admin/api/sounds", body)
	req.Header.Set("Content-Type", ctype)
	_, err := readUpload(httptest.NewRecorder(), req, 50)
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) || tooBig.Limit != 50 {
		t.Errorf("readUpload over the limit = %v, want a MaxBytesError for 50 bytes", err)
	}
}

func TestCheckCosmeticPNG(t *testing.T) {
	if err := checkCosmeticPNG(testPNG(t, 500, 600, false)); err != nil {
		t.Errorf("a 500x600 transparent PNG was refused: %v", err)
	}
	if err := checkCosmeticPNG(testPNG(t, 1000, 1200, false)); err != nil {
		t.Errorf("a same-shape 2x export was refused: %v", err)
	}
	bad := map[string][]byte{
		"wrong shape":       testPNG(t, 640, 480, false),
		"opaque background": testPNG(t, 500, 600, true),
		"huge":              testPNG(t, 2500, 3000, false),
		"not a png":         fakeWAV,
	}
	for name, data := range bad {
		if err := checkCosmeticPNG(data); err == nil {
			t.Errorf("%s: accepted, want a refusal", name)
		}
	}
}

// The overlay stretches every layer over Dabling.png, so the canvas the
// admin checks against has to be that image's real size.
func TestCosmeticCanvasMatchesDabling(t *testing.T) {
	f, err := os.Open("../overlay/assets/Dabling.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != cosmeticCanvasWidth || cfg.Height != cosmeticCanvasHeight {
		t.Errorf("Dabling.png is %dx%d, the admin checks for %dx%d", cfg.Width, cfg.Height, cosmeticCanvasWidth, cosmeticCanvasHeight)
	}
}

func TestAdminCosmetics(t *testing.T) {
	t.Run("add stores the image in the data dir under a content hash", func(t *testing.T) {
		a, dir := testAdmin(t, "")
		body, ctype := upload(t, map[string]string{"id": "tophat", "slot": "head"}, testPNG(t, 500, 600, false))
		status, state := call(t, a, http.MethodPost, "/admin/api/cosmetics", body, ctype)
		if status != http.StatusOK {
			t.Fatalf("add = %d %v", status, state)
		}
		item := catalogItem(t, state, "tophat")
		src, _ := item["src"].(string)
		if item == nil || item["slot"] != "head" || !strings.HasPrefix(src, "cosmetics/tophat-") {
			t.Fatalf("tophat = %v", item)
		}
		if !fileExists(filepath.Join(dir, strings.TrimPrefix(src, "cosmetics/"))) {
			t.Error("the image isn't on disk")
		}
		if item["url"] != "/"+src {
			t.Errorf("url = %v, want /%s (src is relative to the overlay, not /admin)", item["url"], src)
		}
	})

	t.Run("add refuses taken names, unknown slots and bad art, leaving no files", func(t *testing.T) {
		a, dir := testAdmin(t, "")
		for _, fields := range []map[string]string{
			{"id": "SHIRTA", "slot": "torso"}, // shirtA, ignoring case
			{"id": "cape", "slot": "back"},
			{"id": "two words", "slot": "torso"},
		} {
			body, ctype := upload(t, fields, testPNG(t, 500, 600, false))
			if status, _ := call(t, a, http.MethodPost, "/admin/api/cosmetics", body, ctype); status == http.StatusOK {
				t.Errorf("%v was added", fields)
			}
		}
		body, ctype := upload(t, map[string]string{"id": "cape", "slot": "torso"}, testPNG(t, 640, 480, false))
		if status, _ := call(t, a, http.MethodPost, "/admin/api/cosmetics", body, ctype); status != http.StatusBadRequest {
			t.Errorf("wrong-canvas upload = %d, want 400", status)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 2 { // cosmetics.json and shirtA's image
			t.Errorf("data dir has %d files after refused uploads, want 2", len(entries))
		}
	})

	t.Run("replace swaps the image and deletes the old upload", func(t *testing.T) {
		a, dir := testAdmin(t, "")
		art := testPNG(t, 1000, 1200, false)
		body, ctype := upload(t, nil, art)
		status, state := call(t, a, http.MethodPost, "/admin/api/cosmetics/shirta/image", body, ctype)
		if status != http.StatusOK {
			t.Fatalf("replace = %d %v", status, state)
		}
		src := catalogItem(t, state, "shirtA")["src"].(string)
		if src == "cosmetics/shirtA-1234abcd.png" || !fileExists(filepath.Join(dir, strings.TrimPrefix(src, "cosmetics/"))) {
			t.Errorf("shirtA src = %s, want a new file on disk", src)
		}
		if fileExists(filepath.Join(dir, "shirtA-1234abcd.png")) {
			t.Error("the old image wasn't deleted")
		}
	})

	t.Run("delete removes uploads but never shipped art", func(t *testing.T) {
		a, dir := testAdmin(t, "")
		if status, _ := call(t, a, http.MethodDelete, "/admin/api/cosmetics/shirtA", nil, ""); status != http.StatusOK {
			t.Fatalf("delete shirtA = %d", status)
		}
		if fileExists(filepath.Join(dir, "shirtA-1234abcd.png")) {
			t.Error("shirtA's uploaded image wasn't deleted")
		}
		// hat's src is under assets/, outside the data dir: only the catalog
		// entry may go.
		status, state := call(t, a, http.MethodDelete, "/admin/api/cosmetics/hat", nil, "")
		if status != http.StatusOK || catalogItem(t, state, "hat") != nil {
			t.Errorf("delete hat = %d %v", status, state)
		}
		if status, _ := call(t, a, http.MethodDelete, "/admin/api/cosmetics/hat", nil, ""); status != http.StatusNotFound {
			t.Errorf("second delete = %d, want 404", status)
		}
	})

	t.Run("slots: add, reorder, and never strand an item", func(t *testing.T) {
		a, _ := testAdmin(t, "")
		status, state := call(t, a, http.MethodPut, "/admin/api/slots", strings.NewReader(`{"slots":["head","torso","hand"]}`), "application/json")
		if status != http.StatusOK || strings.Join(toStrings(state["slots"]), ",") != "head,torso,hand" {
			t.Errorf("reorder = %d %v", status, state["slots"])
		}
		status, _ = call(t, a, http.MethodPut, "/admin/api/slots", strings.NewReader(`{"slots":["torso"]}`), "application/json")
		if status != http.StatusConflict {
			t.Errorf("dropping the head slot hat uses = %d, want 409", status)
		}
		status, _ = call(t, a, http.MethodPut, "/admin/api/slots", strings.NewReader(`{"slots":["head","torso","Bad Slot"]}`), "application/json")
		if status != http.StatusBadRequest {
			t.Errorf("an untypeable slot = %d, want 400", status)
		}
		status, _ = call(t, a, http.MethodPut, "/admin/api/slots", strings.NewReader(`{"slots":["head","torso","torso"]}`), "application/json")
		if status != http.StatusBadRequest {
			t.Errorf("a duplicate slot = %d, want 400", status)
		}
	})

	t.Run("an item's slot can change", func(t *testing.T) {
		a, _ := testAdmin(t, "")
		status, state := call(t, a, http.MethodPut, "/admin/api/cosmetics/shirtA", strings.NewReader(`{"slot":"head"}`), "application/json")
		if status != http.StatusOK || catalogItem(t, state, "shirtA")["slot"] != "head" {
			t.Errorf("move shirtA = %d %v", status, state)
		}
		status, _ = call(t, a, http.MethodPut, "/admin/api/cosmetics/shirtA", strings.NewReader(`{"slot":"feet"}`), "application/json")
		if status != http.StatusBadRequest {
			t.Errorf("move to a missing slot = %d, want 400", status)
		}
	})

	t.Run("an edit redraws the overlay", func(t *testing.T) {
		a, _ := testAdmin(t, "")
		call(t, a, http.MethodPut, "/admin/api/cosmetics/shirtA", strings.NewReader(`{"slot":"head"}`), "application/json")
		lastBroadcast(t, a.party.hub, "tavern.roster")
	})
}

func TestCosmeticCatalogUpdate(t *testing.T) {
	s := testCatalogStore(t, testCatalogJSON)
	s.get()
	err := s.update(func(cat *cosmeticCatalog) error {
		cat.Items["cape"] = cosmeticItem{Slot: "back", Src: "cape.png"} // no back slot
		return nil
	})
	if err == nil {
		t.Fatal("an item in an undeclared slot was saved")
	}
	if _, _, ok := s.get().lookup("cape"); ok {
		t.Error("the refused change is live anyway")
	}

	before := s.get()
	if err := s.update(func(cat *cosmeticCatalog) error {
		cat.Items["cape"] = cosmeticItem{Slot: "torso", Src: "cape.png"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := before.lookup("cape"); ok {
		t.Error("update changed a catalog someone was already holding")
	}
	reloaded := newCosmeticCatalogStore(s.path)
	if _, _, ok := reloaded.get().lookup("cape"); !ok {
		t.Error("the change didn't reach the file")
	}
}

// readCatalog parses a catalog file a test wrote or synced.
func readCatalog(t *testing.T, path string) *cosmeticCatalog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := parseCosmeticCatalog(data)
	if err != nil {
		t.Fatalf("%s doesn't parse: %v", path, err)
	}
	return cat
}

// The live catalog is the one /admin edits, but items the repo ships later
// (an autoRewards item, say) still have to reach it, without undoing /admin.
func TestSyncStarterCatalog(t *testing.T) {
	setup := func(t *testing.T) (starter, live string) {
		dir := t.TempDir()
		starter = filepath.Join(dir, "starter.json")
		writeTestFile(t, starter, []byte(testCatalogJSON))
		return starter, filepath.Join(dir, "data", "cosmetics", "cosmetics.json")
	}

	t.Run("first run copies the starter catalog", func(t *testing.T) {
		starter, live := setup(t)
		if _, err := syncStarterCatalog(starter, live); err != nil {
			t.Fatal(err)
		}
		if got := readCatalog(t, live); len(got.Items) != 3 || len(got.Seeded) != 3 {
			t.Errorf("first run gave %d items, %d seeded, want 3 and 3", len(got.Items), len(got.Seeded))
		}
	})

	t.Run("a pulled starter item arrives, in a new slot if need be", func(t *testing.T) {
		starter, live := setup(t)
		syncStarterCatalog(starter, live)
		writeTestFile(t, starter, []byte(`{"slots":["torso","head","feet"],"items":{
			"shirtA":{"slot":"torso","src":"a.png"},
			"shirtB":{"slot":"torso","src":"b.png"},
			"tophat":{"slot":"head","src":"hat.png"},
			"boots":{"slot":"feet","src":"boots.png"}}}`))
		if _, err := syncStarterCatalog(starter, live); err != nil {
			t.Fatal(err)
		}
		got := readCatalog(t, live)
		if item, ok := got.Items["boots"]; !ok || item.Slot != "feet" || !slices.Contains(got.Slots, "feet") {
			t.Errorf("boots = %+v, slots %v: want it added along with its feet slot", item, got.Slots)
		}
	})

	t.Run("what /admin deleted or redrew stays that way", func(t *testing.T) {
		starter, live := setup(t)
		syncStarterCatalog(starter, live)
		s := newCosmeticCatalogStore(live)
		s.get()
		err := s.update(func(cat *cosmeticCatalog) error {
			delete(cat.Items, "shirtB")
			item := cat.Items["shirtA"]
			item.Src = "cosmetics/shirtA-new.png"
			cat.Items["shirtA"] = item
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := syncStarterCatalog(starter, live); err != nil {
			t.Fatal(err)
		}
		got := readCatalog(t, live)
		if _, back := got.Items["shirtB"]; back {
			t.Error("shirtB came back after /admin deleted it")
		}
		if src := got.Items["shirtA"].Src; src != "cosmetics/shirtA-new.png" {
			t.Errorf("shirtA's src = %s, want /admin's new art kept", src)
		}
	})

	t.Run("a live catalog from before seeded existed isn't duplicated", func(t *testing.T) {
		starter, live := setup(t)
		os.MkdirAll(filepath.Dir(live), 0o755)
		writeTestFile(t, live, []byte(`{"slots":["torso","head"],"items":{"shirtA":{"slot":"torso","src":"cosmetics/mine.png"},"tophat":{"slot":"head","src":"hat.png"}}}`))
		if _, err := syncStarterCatalog(starter, live); err != nil {
			t.Fatal(err)
		}
		got := readCatalog(t, live)
		if got.Items["shirtA"].Src != "cosmetics/mine.png" || len(got.Items) != 3 || len(got.Seeded) != 3 {
			t.Errorf("synced to %+v, want shirtA left alone and shirtB added", got)
		}
	})

	t.Run("a broken live catalog is left for a human", func(t *testing.T) {
		starter, live := setup(t)
		os.MkdirAll(filepath.Dir(live), 0o755)
		writeTestFile(t, live, []byte(`{"slots":[`))
		if _, err := syncStarterCatalog(starter, live); err == nil {
			t.Error("synced over a broken live catalog")
		}
		if got, _ := os.ReadFile(live); string(got) != `{"slots":[` {
			t.Errorf("broken live catalog was rewritten to %s", got)
		}
	})

	t.Run("a missing starter catalog is an error", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := syncStarterCatalog(filepath.Join(dir, "missing.json"), filepath.Join(dir, "live.json")); err == nil {
			t.Error("synced from a missing starter catalog")
		}
	})
}

// A git pull that ships a cosmetic reaches the running controller without a
// restart, the way hand edits do.
func TestStoreTakesInPulledStarterItems(t *testing.T) {
	dir := t.TempDir()
	starter := filepath.Join(dir, "starter.json")
	live := filepath.Join(dir, "cosmetics.json")
	base := time.Now().Add(-time.Hour)
	writeTestFile(t, starter, []byte(`{"slots":["torso"],"items":{"shirtA":{"slot":"torso","src":"a.png"}}}`))
	os.Chtimes(starter, base, base)
	if _, err := syncStarterCatalog(starter, live); err != nil {
		t.Fatal(err)
	}
	s := newCosmeticCatalogStore(live)
	s.starterPath = starter
	if n := len(s.get().Items); n != 1 {
		t.Fatalf("loaded %d items, want 1", n)
	}

	writeTestFile(t, starter, []byte(`{"slots":["torso"],"items":{"shirtA":{"slot":"torso","src":"a.png"},"bronze":{"slot":"torso","src":"b.png"}}}`))
	os.Chtimes(starter, base.Add(time.Minute), base.Add(time.Minute))
	if _, _, ok := s.get().lookup("bronze"); !ok {
		t.Error("the pulled item didn't reach the live catalog")
	}
}

func TestServeMedia(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "buhee.wav"), fakeWAV)
	writeTestFile(t, filepath.Join(dir, soundSettingsFile), []byte(`{}`))
	h := http.StripPrefix("/sounds/", serveMedia(dir, soundExts...))
	for path, want := range map[string]int{
		"/sounds/buhee.wav":       http.StatusOK,
		"/sounds/sounds.json":     http.StatusNotFound,
		"/sounds/":                http.StatusNotFound, // no listing
		"/sounds/../main.go":      http.StatusNotFound,
		"/sounds/missing.mp3":     http.StatusNotFound,
		"/sounds/buhee.wav?v=123": http.StatusOK,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}
