package main

// Tests for the pure game logic that guards persistent character state
// (design doc §3/§6/§7). Plumbing (websockets, RabbitMQ, MySQL) is
// deliberately untested — those fail loudly; the math fails silently.

import (
	"fmt"
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
		p := newPartyManager(newOverlayHub())
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
		p := newPartyManager(newOverlayHub())
		c := newChar("ghost", 0)
		c.Alive = false
		if msg := p.join(c); !strings.Contains(msg, "dead") {
			t.Errorf("dead join = %q, want a 'dead' refusal", msg)
		}
	})

	t.Run("duplicate join is case-insensitive", func(t *testing.T) {
		p := newPartyManager(newOverlayHub())
		p.join(newChar("Alice", 0))
		if msg := p.join(newChar("alice", 0)); !strings.Contains(msg, "already") {
			t.Errorf("duplicate join = %q, want an 'already in the party' refusal", msg)
		}
	})

	t.Run("kick frees the slot", func(t *testing.T) {
		p := newPartyManager(newOverlayHub())
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
		p := newPartyManager(newOverlayHub())
		if got := p.kick("nobody"); got != nil {
			t.Errorf("kick(nobody) = %v, want nil", got)
		}
	})

	t.Run("removeForDeath", func(t *testing.T) {
		p := newPartyManager(newOverlayHub())
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
		p := newPartyManager(newOverlayHub())
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
		p := newPartyManager(newOverlayHub())
		c := newChar("Live", 0)
		p.join(c)
		if got := p.findInParty("live"); got != c {
			t.Errorf("findInParty returned %p, want %p", got, c)
		}
	})
}

func TestTavernManager(t *testing.T) {
	t.Run("touch adds once, keyed case-insensitively", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub())
		tv.touch(newChar("Dude", 0))
		tv.touch(newChar("dude", 0))
		if len(tv.dudes) != 1 {
			t.Errorf("roster has %d dudes, want 1", len(tv.dudes))
		}
	})

	t.Run("touch tracks level changes", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub())
		tv.touch(newChar("Dude", 0))
		tv.touch(newChar("Dude", 25))
		if got := tv.dudes["dude"].Level; got != 2 {
			t.Errorf("roster level = %d, want 2", got)
		}
	})

	t.Run("remove is case-insensitive and idempotent", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub())
		tv.touch(newChar("Dude", 0))
		tv.remove("DUDE")
		if len(tv.dudes) != 0 {
			t.Errorf("roster has %d dudes after remove, want 0", len(tv.dudes))
		}
		tv.remove("DUDE") // must not panic or broadcast
	})

	t.Run("sweep drops only idle dudes", func(t *testing.T) {
		tv := newTavernManager(newOverlayHub())
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
