package main

// Tests for the pure game logic that guards persistent character state
// (design doc §3/§6/§7). Plumbing (websockets, RabbitMQ, MySQL) is
// deliberately untested — those fail loudly; the math fails silently.

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// newChar builds a live character normalized to exp: level, max_hp, and hp
// are derived exactly as production does it (applyExp restores to full on
// the initial level-up from zero).
func newChar(name string, exp int64) *character {
	c := &character{Name: name, Alive: true}
	c.applyExp(exp)
	return c
}

// The design doc §3 anchor table for the cubic (quadratic-per-level) curve.
func TestTotalExpForLevel(t *testing.T) {
	cases := map[int]int64{1: 0, 2: 25, 3: 125, 4: 350, 5: 750, 10: 7125, 20: 61750, 30: 213875}
	for level, want := range cases {
		if got := totalExpForLevel(level); got != want {
			t.Errorf("totalExpForLevel(%d) = %d, want %d", level, got, want)
		}
	}
}

func TestLevelForExp(t *testing.T) {
	cases := []struct {
		exp  int64
		want int
	}{
		{-5, 1}, // negative clamps to level 1
		{0, 1},
		{24, 1}, // one short of a threshold must not round up
		{25, 2},
		{124, 2},
		{125, 3},
		{349, 3},
		{350, 4},
		{749, 4},
		{750, 5},
		{7124, 9},
		{7125, 10},
		{61749, 19},
		{61750, 20},
		{213875, 30},
	}
	for _, tc := range cases {
		if got := levelForExp(tc.exp); got != tc.want {
			t.Errorf("levelForExp(%d) = %d, want %d", tc.exp, got, tc.want)
		}
	}
}

// The per-message grant: flat base plus a sqrt(logins) veteran bonus — the
// design doc §3 anchors, deliberately not linear in logins.
func TestExpPerMessage(t *testing.T) {
	cases := map[int64]int64{-1: 5, 0: 5, 1: 6, 25: 10, 100: 15, 400: 25}
	for logins, want := range cases {
		if got := expPerMessage(logins); got != want {
			t.Errorf("expPerMessage(%d) = %d, want %d", logins, got, want)
		}
	}
}

// levelForExp must invert totalExpForLevel exactly at every threshold: a
// level must never land early or late.
func TestLevelCurveRoundTrip(t *testing.T) {
	for level := 1; level <= 100; level++ {
		threshold := totalExpForLevel(level)
		if got := levelForExp(threshold); got != level {
			t.Errorf("levelForExp(totalExpForLevel(%d)=%d) = %d, want %d", level, threshold, got, level)
		}
		if level >= 2 {
			if got := levelForExp(threshold - 1); got != level-1 {
				t.Errorf("levelForExp(%d) = %d, want %d (one exp under the level-%d threshold)", threshold-1, got, level-1, level)
			}
		}
	}
}

func TestExpNext(t *testing.T) {
	if got := newChar("a", 0).expNext(); got != 25 {
		t.Errorf("level-1 expNext = %d, want 25", got)
	}
	if got := newChar("a", 25).expNext(); got != 125 {
		t.Errorf("level-2 expNext = %d, want 125", got)
	}
}

func TestApplyExp(t *testing.T) {
	t.Run("level-up restores hp to new max", func(t *testing.T) {
		c := newChar("a", 0)
		c.HP = 3
		c.applyExp(25)
		if c.Level != 2 || c.MaxHP != 18 || c.HP != 18 {
			t.Errorf("got level %d, hp %d/%d, want level 2, hp 18/18", c.Level, c.HP, c.MaxHP)
		}
	})

	t.Run("gain without level-up keeps current hp", func(t *testing.T) {
		c := newChar("a", 25)
		c.HP = 5
		c.applyExp(5)
		if c.Level != 2 || c.HP != 5 {
			t.Errorf("got level %d, hp %d, want level 2, hp 5", c.Level, c.HP)
		}
	})

	t.Run("multi-level jump", func(t *testing.T) {
		c := newChar("a", 0)
		c.applyExp(750)
		if c.Level != 5 || c.MaxHP != 30 || c.HP != 30 {
			t.Errorf("got level %d, hp %d/%d, want level 5, hp 30/30", c.Level, c.HP, c.MaxHP)
		}
	})

	t.Run("deduction caps hp at the lower max", func(t *testing.T) {
		// The !smite / !revive-cost path: exp only ever goes down via DM
		// commands, so this branch never runs in normal play.
		c := newChar("a", 7125) // level 10, 50/50 hp
		c.applyExp(-7000)       // exp 125 -> level 3
		if c.Level != 3 || c.MaxHP != 22 || c.HP != 22 {
			t.Errorf("got level %d, hp %d/%d, want level 3, hp 22/22", c.Level, c.HP, c.MaxHP)
		}
	})

	t.Run("exp never goes negative", func(t *testing.T) {
		c := newChar("a", 5)
		c.applyExp(-100)
		if c.Exp != 0 || c.Level != 1 || c.MaxHP != 14 {
			t.Errorf("got exp %d, level %d, max_hp %d, want 0, 1, 14", c.Exp, c.Level, c.MaxHP)
		}
	})
}

func TestPartyManager(t *testing.T) {
	t.Run("join, then full at four", func(t *testing.T) {
		p := newPartyManager(newOverlayHub(), nil)
		for i := 0; i < partyMaxSize; i++ {
			if msg := p.join(newChar(fmt.Sprintf("member%d", i), 0)); msg != "" {
				t.Fatalf("join %d refused: %q", i, msg)
			}
		}
		if msg := p.join(newChar("overflow", 0)); !strings.Contains(msg, "full") {
			t.Errorf("fifth join = %q, want a 'party is full' refusal", msg)
		}
	})

	t.Run("dead characters cannot join", func(t *testing.T) {
		p := newPartyManager(newOverlayHub(), nil)
		c := newChar("ghost", 0)
		c.Alive = false
		if msg := p.join(c); !strings.Contains(msg, "dead") {
			t.Errorf("dead join = %q, want a 'dead' refusal", msg)
		}
	})

	t.Run("duplicate join is case-insensitive", func(t *testing.T) {
		p := newPartyManager(newOverlayHub(), nil)
		p.join(newChar("Alice", 0))
		if msg := p.join(newChar("alice", 0)); !strings.Contains(msg, "already") {
			t.Errorf("duplicate join = %q, want an 'already in the party' refusal", msg)
		}
	})

	t.Run("kick frees the slot", func(t *testing.T) {
		p := newPartyManager(newOverlayHub(), nil)
		c := newChar("Bob", 0)
		p.join(c)
		if got := p.kick("BOB"); got != c {
			t.Fatalf("kick returned %v, want the joined character", got)
		}
		if p.findInParty("bob") != nil {
			t.Error("Bob still in party after kick")
		}
		if msg := p.join(newChar("Bob", 0)); msg != "" {
			t.Errorf("rejoin after kick refused: %q", msg)
		}
	})

	t.Run("kick unknown returns nil", func(t *testing.T) {
		p := newPartyManager(newOverlayHub(), nil)
		if got := p.kick("nobody"); got != nil {
			t.Errorf("kick(nobody) = %v, want nil", got)
		}
	})

	t.Run("removeForDeath", func(t *testing.T) {
		p := newPartyManager(newOverlayHub(), nil)
		c := newChar("Casualty", 0)
		p.join(c)
		if got := p.removeForDeath("casualty"); got != c {
			t.Fatalf("removeForDeath returned %v, want the joined character", got)
		}
		if got := p.removeForDeath("casualty"); got != nil {
			t.Errorf("second removeForDeath = %v, want nil", got)
		}
	})

	t.Run("newParty ejects everyone", func(t *testing.T) {
		p := newPartyManager(newOverlayHub(), nil)
		p.join(newChar("a", 0))
		p.join(newChar("b", 0))
		if ejected := p.newParty(); len(ejected) != 2 {
			t.Fatalf("newParty ejected %d, want 2", len(ejected))
		}
		if p.findInParty("a") != nil || p.findInParty("b") != nil {
			t.Error("members still present after newParty")
		}
		if msg := p.join(newChar("c", 0)); msg != "" {
			t.Errorf("join after newParty refused: %q", msg)
		}
	})

	t.Run("findInParty returns the live pointer", func(t *testing.T) {
		// DM commands mutate the returned character in place; a copy would
		// silently desync the broadcast from the database.
		p := newPartyManager(newOverlayHub(), nil)
		c := newChar("Live", 0)
		p.join(c)
		if got := p.findInParty("live"); got != c {
			t.Errorf("findInParty returned %p, want %p", got, c)
		}
	})
}

func TestTavernManager(t *testing.T) {
	t.Run("touch adds once, keyed case-insensitively", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub(), nil)
		tv.touch(newChar("Dude", 0))
		tv.touch(newChar("dude", 0))
		if len(tv.dudes) != 1 {
			t.Errorf("roster has %d dudes, want 1", len(tv.dudes))
		}
	})

	t.Run("touch tracks level changes", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub(), nil)
		tv.touch(newChar("Dude", 0))
		tv.touch(newChar("Dude", 25))
		if got := tv.dudes["dude"].Level; got != 2 {
			t.Errorf("roster level = %d, want 2", got)
		}
	})

	t.Run("remove is case-insensitive and idempotent", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub(), nil)
		tv.touch(newChar("Dude", 0))
		tv.remove("DUDE")
		if len(tv.dudes) != 0 {
			t.Errorf("roster has %d dudes after remove, want 0", len(tv.dudes))
		}
		tv.remove("DUDE") // must not panic or broadcast
	})

	t.Run("sweep drops only idle dudes", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub(), nil)
		tv.touch(newChar("Fresh", 0))
		tv.touch(newChar("Stale", 0))
		tv.dudes["stale"].LastSeen = time.Now().Add(-tavernIdleTimeout - time.Minute)
		tv.sweep(time.Now().Add(-tavernIdleTimeout))
		if _, ok := tv.dudes["stale"]; ok {
			t.Error("stale dude survived the sweep")
		}
		if _, ok := tv.dudes["fresh"]; !ok {
			t.Error("fresh dude was swept")
		}
	})
}

func TestExpCooldown(t *testing.T) {
	tr := newExpCooldownTracker()
	if !tr.ready(1) {
		t.Error("first message should be off cooldown")
	}
	if tr.ready(1) {
		t.Error("second message inside the window should be on cooldown")
	}
	if !tr.ready(2) {
		t.Error("cooldowns must be per-chatter")
	}
}

func TestIsAuthorizedForOther(t *testing.T) {
	badge := func(setID string) map[string]any {
		return map[string]any{"set_id": setID}
	}
	cases := []struct {
		name  string
		event map[string]any
		want  bool
	}{
		{"broadcaster by id", map[string]any{"chatter_user_id": "42", "broadcaster_user_id": "42"}, true},
		{"moderator badge", map[string]any{"chatter_user_id": "7", "broadcaster_user_id": "42", "badges": []any{badge("moderator")}}, true},
		{"broadcaster badge", map[string]any{"chatter_user_id": "7", "broadcaster_user_id": "42", "badges": []any{badge("broadcaster")}}, true},
		{"subscriber badge only", map[string]any{"chatter_user_id": "7", "broadcaster_user_id": "42", "badges": []any{badge("subscriber")}}, false},
		{"no badges", map[string]any{"chatter_user_id": "7", "broadcaster_user_id": "42"}, false},
		{"empty ids must not match each other", map[string]any{"chatter_user_id": "", "broadcaster_user_id": ""}, false},
		{"empty event", map[string]any{}, false},
	}
	for _, tc := range cases {
		if got := isAuthorizedForOther(tc.event); got != tc.want {
			t.Errorf("%s: isAuthorizedForOther = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Chat text is arbitrary user input rendered via innerHTML in the OBS
// browser source — it must always come out escaped.
func TestFormatChatEscapesHostileInput(t *testing.T) {
	event := map[string]any{
		"chatter_user_name": `<img src=x onerror=alert(1)>`,
		"message": map[string]any{
			"text": `<script>alert('xss')</script>`,
			"fragments": []any{
				map[string]any{"type": "text", "text": `<script>alert('xss')</script>`},
			},
		},
	}
	msg := formatChat(event)
	if msg == nil {
		t.Fatal("formatChat returned nil")
	}
	for _, hostile := range []string{"<script", "<img src=x"} {
		if strings.Contains(msg.MessageHTML, hostile) {
			t.Errorf("MessageHTML contains unescaped %q: %s", hostile, msg.MessageHTML)
		}
	}
	if !strings.Contains(msg.MessageHTML, "&lt;script&gt;") {
		t.Errorf("MessageHTML lost the escaped message text: %s", msg.MessageHTML)
	}
}

// Same guarantee when the message has no fragments (the fallback branch).
func TestRenderHTMLEscapesBareMessage(t *testing.T) {
	msg := &chatMessage{Username: "user", Message: `<b onmouseover=evil()>hi</b>`}
	got := renderHTML(msg)
	if strings.Contains(got, "<b ") {
		t.Errorf("renderHTML contains unescaped tag: %s", got)
	}
}

func TestTrimName(t *testing.T) {
	cases := map[string]string{"@Bob": "Bob", " @Bob ": "Bob", "bob": "bob", " bob ": "bob"}
	for in, want := range cases {
		if got := trimName(in); got != want {
			t.Errorf("trimName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpriteVariant(t *testing.T) {
	if spriteVariant("Alice") != spriteVariant("alice") {
		t.Error("sprite variant should be case-insensitive so it never changes with display-name casing")
	}
	for _, name := range []string{"a", "somebody", "アリス"} {
		if v := spriteVariant(name); v < 0 || v > 8 {
			t.Errorf("spriteVariant(%q) = %d, want 0..8", name, v)
		}
	}
}

// testCatalog has two shirts sharing the torso slot and a hat on its own.
func testCatalog() *cosmeticCatalog {
	return &cosmeticCatalog{
		Slots: []string{"torso", "head"},
		Items: map[string]cosmeticItem{
			"shirtA": {Slot: "torso", Src: "a.png"},
			"shirtB": {Slot: "torso", Src: "b.png"},
			"tophat": {Slot: "head", Src: "hat.png"},
		},
	}
}

const testCatalogJSON = `{"slots":["torso","head"],"items":{
	"shirtA":{"slot":"torso","src":"a.png"},
	"shirtB":{"slot":"torso","src":"b.png"},
	"tophat":{"slot":"head","src":"hat.png"}}}`

// testCatalogStore serves raw from a temp cosmetics.json.
func testCatalogStore(t *testing.T, raw string) *cosmeticCatalogStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cosmetics.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return newCosmeticCatalogStore(path)
}

// lastBroadcast drains the hub and decodes the newest payload of type typ.
func lastBroadcast(t *testing.T, hub *overlayHub, typ string) map[string]any {
	t.Helper()
	var last map[string]any
	for {
		select {
		case data := <-hub.broadcast:
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["type"] == typ {
				last = payload
			}
		default:
			if last == nil {
				t.Fatalf("no %s broadcast", typ)
			}
			return last
		}
	}
}

func TestParseCosmeticCatalog(t *testing.T) {
	if _, err := parseCosmeticCatalog([]byte(testCatalogJSON)); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	bad := map[string]string{
		"not json":                `{`,
		"undeclared slot":         `{"slots":["torso"],"items":{"tophat":{"slot":"head","src":"hat.png"}}}`,
		"missing src":             `{"slots":["torso"],"items":{"shirtA":{"slot":"torso"}}}`,
		"id chat can't type":      `{"slots":["torso"],"items":{"red shirt":{"slot":"torso","src":"a.png"}}}`,
		"ids differ only by case": `{"slots":["torso"],"items":{"shirtA":{"slot":"torso","src":"a.png"},"SHIRTA":{"slot":"torso","src":"b.png"}}}`,
		"slot listed twice":       `{"slots":["torso","torso"],"items":{}}`,
	}
	for name, raw := range bad {
		if _, err := parseCosmeticCatalog([]byte(raw)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

// The catalog that ships in overlay/ must load, and every layer it names
// must exist, or the overlay draws a broken image on whoever wears it.
func TestShippedCosmeticCatalog(t *testing.T) {
	data, err := os.ReadFile("../overlay/assets/cosmetics/cosmetics.json")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := parseCosmeticCatalog(data)
	if err != nil {
		t.Fatalf("shipped catalog rejected: %v", err)
	}
	for id, item := range cat.Items {
		if _, err := os.Stat(filepath.Join("../overlay", item.Src)); err != nil {
			t.Errorf("%s: layer image missing: %v", id, err)
		}
	}
}

func TestCosmeticRules(t *testing.T) {
	t.Run("give rejects items the catalog doesn't have", func(t *testing.T) {
		c := newChar("Hez", 0)
		if _, _, err := testCatalog().give(c, "shritA"); err == nil {
			t.Error("typo was granted")
		}
		if len(c.Cosmetics) != 0 {
			t.Errorf("wardrobe = %v, want empty", c.Cosmetics)
		}
	})

	t.Run("give stores the catalog's spelling", func(t *testing.T) {
		c := newChar("Hez", 0)
		id, _, err := testCatalog().give(c, "SHIRTA")
		if err != nil || id != "shirtA" || !slices.Equal(c.Cosmetics, []string{"shirtA"}) {
			t.Errorf("give(SHIRTA) = %q, %v; wardrobe %v, want shirtA", id, err, c.Cosmetics)
		}
	})

	t.Run("give rejects duplicates", func(t *testing.T) {
		c := newChar("Hez", 0)
		cat := testCatalog()
		cat.give(c, "shirtA")
		if _, _, err := cat.give(c, "shirta"); err == nil {
			t.Error("second shirtA was granted")
		}
		if len(c.Cosmetics) != 1 {
			t.Errorf("wardrobe = %v, want one shirtA", c.Cosmetics)
		}
	})

	t.Run("first item in a slot is worn, later ones wait for !equip", func(t *testing.T) {
		c := newChar("Hez", 0)
		cat := testCatalog()
		if _, worn, _ := cat.give(c, "shirtA"); !worn {
			t.Error("first shirt wasn't put on")
		}
		if _, worn, _ := cat.give(c, "shirtB"); worn {
			t.Error("second shirt replaced the first")
		}
		if _, worn, _ := cat.give(c, "tophat"); !worn {
			t.Error("first hat wasn't put on")
		}
		want := map[string]string{"torso": "shirtA", "head": "tophat"}
		if !maps.Equal(c.Equipped, want) {
			t.Errorf("equipped = %v, want %v", c.Equipped, want)
		}
	})

	t.Run("give fills a slot whose worn item left the catalog", func(t *testing.T) {
		c := newChar("Hez", 0)
		c.Equipped = map[string]string{"torso": "retiredShirt"}
		if _, worn, _ := testCatalog().give(c, "shirtA"); !worn {
			t.Error("new shirt wasn't put on over a retired one")
		}
	})

	t.Run("equip needs ownership", func(t *testing.T) {
		c := newChar("Chika", 0)
		cat := testCatalog()
		if _, err := cat.equip(c, "shirtB"); err == nil {
			t.Error("equipped an unowned shirt")
		}
		if _, err := cat.equip(c, "nonsense"); err == nil {
			t.Error("equipped an unknown item")
		}
		if len(c.Equipped) != 0 {
			t.Errorf("equipped = %v, want nothing", c.Equipped)
		}
	})

	t.Run("equip swaps within a slot and leaves other slots alone", func(t *testing.T) {
		c := newChar("Chika", 0)
		cat := testCatalog()
		cat.give(c, "shirtA")
		cat.give(c, "shirtB")
		cat.give(c, "tophat")
		if id, err := cat.equip(c, "SHIRTB"); err != nil || id != "shirtB" {
			t.Fatalf("equip(SHIRTB) = %q, %v", id, err)
		}
		want := map[string]string{"torso": "shirtB", "head": "tophat"}
		if !maps.Equal(c.Equipped, want) {
			t.Errorf("equipped = %v, want %v", c.Equipped, want)
		}
	})

	t.Run("unequip by item or by slot", func(t *testing.T) {
		c := newChar("Hez", 0)
		cat := testCatalog()
		cat.give(c, "shirtA")
		cat.give(c, "tophat")
		if id, err := unequipCosmetic(c, "ShirtA"); err != nil || id != "shirtA" {
			t.Errorf("unequip(ShirtA) = %q, %v", id, err)
		}
		if id, err := unequipCosmetic(c, "head"); err != nil || id != "tophat" {
			t.Errorf("unequip(head) = %q, %v", id, err)
		}
		if _, err := unequipCosmetic(c, "shirtA"); err == nil {
			t.Error("unequipped a shirt that wasn't worn")
		}
		if len(c.Cosmetics) != 2 {
			t.Errorf("unequip changed the wardrobe: %v", c.Cosmetics)
		}
	})

	t.Run("take removes the item and takes it off", func(t *testing.T) {
		c := newChar("Hez", 0)
		cat := testCatalog()
		cat.give(c, "shirtA")
		cat.give(c, "tophat")
		if id, ok := takeCosmetic(c, "shirta"); !ok || id != "shirtA" {
			t.Fatalf("take(shirta) = %q, %v", id, ok)
		}
		if !slices.Equal(c.Cosmetics, []string{"tophat"}) {
			t.Errorf("wardrobe = %v, want [tophat]", c.Cosmetics)
		}
		if want := map[string]string{"head": "tophat"}; !maps.Equal(c.Equipped, want) {
			t.Errorf("equipped = %v, want %v", c.Equipped, want)
		}
		if _, ok := takeCosmetic(c, "shirtA"); ok {
			t.Error("took a shirt twice")
		}
	})

	t.Run("take clears ids the catalog doesn't know", func(t *testing.T) {
		// Pre-catalog !give stored anything typed, typos included.
		c := newChar("Hez", 0)
		c.Cosmetics = []string{"shritA"}
		if _, ok := takeCosmetic(c, "SHRITA"); !ok || len(c.Cosmetics) != 0 {
			t.Errorf("typo not cleared: %v", c.Cosmetics)
		}
	})
}

func TestCosmeticLayers(t *testing.T) {
	cat := testCatalog()
	got := cat.layers(map[string]string{"head": "tophat", "torso": "shirtB"})
	if want := []string{"b.png", "hat.png"}; !slices.Equal(got, want) {
		t.Errorf("layers = %v, want %v (slot order, not map order)", got, want)
	}
	if got := cat.layers(map[string]string{"torso": "retiredShirt", "head": "shirtA"}); len(got) != 0 {
		t.Errorf("layers = %v, want unknown items and wrong-slot items skipped", got)
	}
	if got := cat.layers(nil); len(got) != 0 {
		t.Errorf("layers(nil) = %v, want none", got)
	}
}

func TestWardrobeSummary(t *testing.T) {
	cat := testCatalog()
	c := newChar("Hez", 0)
	if got := cat.wardrobeSummary(c); !strings.Contains(got, "don't own") {
		t.Errorf("empty wardrobe = %q", got)
	}
	c.Cosmetics = []string{"shirtA", "retiredShirt", "shirtB", "shirta"}
	c.Equipped = map[string]string{"torso": "shirtB"}
	if got, want := cat.wardrobeSummary(c), "your wardrobe: shirtA, shirtB (wearing)"; got != want {
		t.Errorf("wardrobe = %q, want %q", got, want)
	}
}

func TestCosmeticCatalogStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cosmetics.json")
	s := newCosmeticCatalogStore(path)
	if got := s.get(); got == nil || len(got.Items) != 0 {
		t.Fatalf("missing file served %v, want an empty catalog", got)
	}

	write := func(raw string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	write(`{"slots":["torso"],"items":{"shirtA":{"slot":"torso","src":"a.png"}}}`, base)
	if n := len(s.get().Items); n != 1 {
		t.Errorf("loaded %d items, want 1", n)
	}
	write(testCatalogJSON, base.Add(time.Minute))
	if n := len(s.get().Items); n != 3 {
		t.Errorf("after edit: %d items, want 3 (hot reload)", n)
	}
	write(`{"slots":[`, base.Add(2*time.Minute))
	if n := len(s.get().Items); n != 3 {
		t.Errorf("after broken edit: %d items, want the last good 3", n)
	}

	var none *cosmeticCatalogStore
	if none.get() == nil {
		t.Error("nil store returned a nil catalog")
	}
}

func TestOutfitsReachTheOverlay(t *testing.T) {
	t.Run("tavern roster carries layers", func(t *testing.T) {
		hub := newOverlayHub()
		tv := newTavernManager(hub, testCatalogStore(t, testCatalogJSON))
		c := newChar("Hez", 0)
		c.Equipped = map[string]string{"torso": "shirtA"}
		tv.touch(c)
		dudes := lastBroadcast(t, hub, "tavern.roster")["dudes"].([]any)
		layers := dudes[0].(map[string]any)["layers"].([]any)
		if len(layers) != 1 || layers[0] != "a.png" {
			t.Errorf("roster layers = %v, want [a.png]", layers)
		}
	})

	t.Run("party cards carry layers", func(t *testing.T) {
		hub := newOverlayHub()
		p := newPartyManager(hub, testCatalogStore(t, testCatalogJSON))
		c := newChar("Chika", 0)
		c.Equipped = map[string]string{"torso": "shirtB"}
		p.join(c)
		members := lastBroadcast(t, hub, "party.update")["members"].([]any)
		layers := members[0].(map[string]any)["layers"].([]any)
		if len(layers) != 1 || layers[0] != "b.png" {
			t.Errorf("party layers = %v, want [b.png]", layers)
		}
	})

	t.Run("touch redraws on an outfit change", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub(), nil)
		c := newChar("Hez", 0)
		tv.touch(c)
		c.Equipped = map[string]string{"torso": "shirtA"}
		tv.touch(c)
		if got := tv.dudes["hez"].Equipped["torso"]; got != "shirtA" {
			t.Errorf("roster outfit = %q, want shirtA", got)
		}
	})

	t.Run("roster keeps its own copy of the outfit", func(t *testing.T) {
		// Aliasing the character's map would make every later change look
		// like "no change" and never redraw.
		tv := newTavernManager(newOverlayHub(), nil)
		c := newChar("Hez", 0)
		c.Equipped = map[string]string{"torso": "shirtA"}
		tv.touch(c)
		c.Equipped["torso"] = "shirtB"
		if got := tv.dudes["hez"].Equipped["torso"]; got != "shirtA" {
			t.Errorf("roster outfit changed to %q without a touch", got)
		}
	})

	t.Run("refresh redraws present dudes but never summons absent ones", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub(), nil)
		absent := newChar("Lurker", 0)
		tv.refresh(absent)
		if len(tv.dudes) != 0 {
			t.Error("refresh added a dude who hasn't chatted")
		}
		c := newChar("Hez", 0)
		tv.touch(c)
		c.Equipped = map[string]string{"torso": "shirtC"}
		tv.refresh(c)
		if got := tv.dudes["hez"].Equipped["torso"]; got != "shirtC" {
			t.Errorf("roster outfit = %q, want shirtC", got)
		}
	})
}

// The two login-bonus redeems share a handler; their chat lines must not.
func TestLoginMessages(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{dailyLoginMessage("dabi", 7), "@dabi your daily login count is now 7!"},
		{firstLoginMessage("dabi", 1), "@dabi you've logged in first 1 time!"},
		{firstLoginMessage("dabi", 12), "@dabi you've logged in first 12 times!"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
