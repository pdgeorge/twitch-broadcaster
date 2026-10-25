package main

// Tests for sound commands: the rules that decide what plays (names,
// cooldowns, the folder scan) and the /admin edits that move files around.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Fake audio: sniffAudio only reads the header, so these pass as each format.
var (
	fakeWAV = []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00")
	fakeMP3 = []byte("ID3\x04\x00\x00\x00\x00\x00\x00\xff\xfb\x90\x00")
	fakeOGG = []byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00")
)

func testSoundBoard(t *testing.T) (*soundBoard, string) {
	t.Helper()
	dir := t.TempDir()
	return newSoundBoard(dir, newOverlayHub()), dir
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestCheckSoundName(t *testing.T) {
	for _, name := range []string{"buhee", "ara-ara", "punch_2", "a", "9lives"} {
		if err := checkSoundName(name); err != nil {
			t.Errorf("checkSoundName(%q) = %v, want ok", name, err)
		}
	}
	bad := []string{"", "Buhee", "two words", "../etc", "-dash", "_under", "café", strings.Repeat("a", 33)}
	// Built-in commands are taken: a sound would go off on top of them.
	bad = append(bad, "roll", "give", "sounds", "dabi")
	for _, name := range bad {
		if err := checkSoundName(name); err == nil {
			t.Errorf("checkSoundName(%q) passed, want a refusal", name)
		}
	}
}

func TestSniffAudio(t *testing.T) {
	good := []struct {
		name string
		data []byte
		want string
	}{
		{"wav", fakeWAV, ".wav"},
		{"mp3 with ID3 tag", fakeMP3, ".mp3"},
		{"mp3 bare frame", []byte{0xFF, 0xFB, 0x90, 0x00}, ".mp3"},
		{"ogg", fakeOGG, ".ogg"},
	}
	for _, tc := range good {
		if got, err := sniffAudio(tc.data); err != nil || got != tc.want {
			t.Errorf("sniffAudio(%s) = %q, %v, want %s", tc.name, got, err, tc.want)
		}
	}
	bad := map[string][]byte{
		"empty":             nil,
		"png":               []byte("\x89PNG\r\n\x1a\n"),
		"text":              []byte("hello"),
		"aac (adts)":        {0xFF, 0xF1, 0x50, 0x80},
		"riff but not wave": []byte("RIFF\x24\x00\x00\x00AVI LIST"),
	}
	for name, data := range bad {
		if ext, err := sniffAudio(data); err == nil {
			t.Errorf("sniffAudio(%s) = %q, want a refusal", name, ext)
		}
	}
}

// The folder is the list: file name = command, whatever can't be a command is
// skipped, and settings fall back to the defaults field by field.
func TestSoundBoardScan(t *testing.T) {
	b, dir := testSoundBoard(t)
	writeTestFile(t, filepath.Join(dir, "buhee.wav"), fakeWAV)
	writeTestFile(t, filepath.Join(dir, "Punch.MP3"), fakeMP3)     // case doesn't matter
	writeTestFile(t, filepath.Join(dir, "punch.ogg"), fakeOGG)     // a second !punch: ignored
	writeTestFile(t, filepath.Join(dir, "two words.wav"), fakeWAV) // not a chat word
	writeTestFile(t, filepath.Join(dir, "roll.wav"), fakeWAV)      // reserved
	writeTestFile(t, filepath.Join(dir, "notes.txt"), []byte("not audio"))
	writeTestFile(t, filepath.Join(dir, soundSettingsFile), []byte(`{"buhee": {"cooldown_seconds": 30}}`))

	list, err := b.list()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"buhee", "punch"}) {
		t.Fatalf("sounds = %v, want [buhee punch]", names)
	}
	if got := list[0].soundSettings; got.CooldownSeconds != 30 || got.Volume != 100 {
		t.Errorf("buhee settings = %+v, want the saved 30s cooldown and the default volume", got)
	}
	if got := list[1]; got.File != "Punch.MP3" || got.CooldownSeconds != defaultSoundCooldownSeconds {
		t.Errorf("punch = %+v, want Punch.MP3 on the default cooldown", got)
	}
	if !strings.HasPrefix(list[1].URL, "/sounds/Punch.MP3?v=") {
		t.Errorf("punch url = %q, want /sounds/Punch.MP3 plus a version", list[1].URL)
	}
}

func TestBrokenSoundSettingsDontMute(t *testing.T) {
	b, dir := testSoundBoard(t)
	writeTestFile(t, filepath.Join(dir, "buhee.wav"), fakeWAV)
	writeTestFile(t, filepath.Join(dir, soundSettingsFile), []byte(`{"buhee": {`))
	list, err := b.list()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v, want buhee", list, err)
	}
	if list[0].soundSettings != defaultSoundSettings() {
		t.Errorf("settings = %+v, want the defaults", list[0].soundSettings)
	}
}

func TestSoundCooldown(t *testing.T) {
	b, dir := testSoundBoard(t)
	writeTestFile(t, filepath.Join(dir, "buhee.wav"), fakeWAV)
	writeTestFile(t, filepath.Join(dir, "punch.mp3"), fakeMP3)
	writeTestFile(t, filepath.Join(dir, soundSettingsFile), []byte(`{"buhee": {"cooldown_seconds": 5}, "punch": {"cooldown_seconds": 0}}`))
	t0 := time.Now()

	if !b.trigger("buhee", "alice", t0) {
		t.Fatal("first !buhee didn't play")
	}
	if b.trigger("buhee", "bob", t0.Add(4*time.Second)) {
		t.Error("!buhee played inside its cooldown: the cooldown is per sound, not per chatter")
	}
	// The refused attempt above must not have pushed the cooldown back.
	if !b.trigger("buhee", "bob", t0.Add(5*time.Second)) {
		t.Error("!buhee didn't play once its cooldown ran out")
	}
	if !b.trigger("punch", "alice", t0) || !b.trigger("punch", "alice", t0) {
		t.Error("a 0s cooldown blocked a replay")
	}
	if b.trigger("nope", "alice", t0) {
		t.Error("played a sound that doesn't exist")
	}
}

func TestSoundPlayReachesTheOverlay(t *testing.T) {
	b, dir := testSoundBoard(t)
	writeTestFile(t, filepath.Join(dir, "buhee.wav"), fakeWAV)
	writeTestFile(t, filepath.Join(dir, soundSettingsFile), []byte(`{"buhee": {"cooldown_seconds": 5, "volume": 40}}`))
	b.trigger("buhee", "alice", time.Now())
	payload := lastBroadcast(t, b.hub, "sound.play")
	if payload["name"] != "buhee" || payload["volume"] != 0.4 || !strings.HasPrefix(payload["url"].(string), "/sounds/buhee.wav?v=") {
		t.Errorf("sound.play = %v", payload)
	}
}

func TestSoundBoardEdits(t *testing.T) {
	t.Run("add stores the file under the command, in its real format", func(t *testing.T) {
		b, dir := testSoundBoard(t)
		s, err := b.add("buhee", fakeMP3, soundSettings{CooldownSeconds: 12, Volume: 80})
		if err != nil {
			t.Fatal(err)
		}
		if s.File != "buhee.mp3" || !fileExists(filepath.Join(dir, "buhee.mp3")) {
			t.Errorf("added %+v, want buhee.mp3 on disk", s)
		}
		if s.CooldownSeconds != 12 || s.Volume != 80 {
			t.Errorf("settings = %+v, want 12s at 80%%", s.soundSettings)
		}
	})

	t.Run("add refuses duplicates, junk and bad settings", func(t *testing.T) {
		b, _ := testSoundBoard(t)
		if _, err := b.add("buhee", fakeWAV, defaultSoundSettings()); err != nil {
			t.Fatal(err)
		}
		if _, err := b.add("buhee", fakeMP3, defaultSoundSettings()); err == nil {
			t.Error("a second !buhee was added")
		}
		if _, err := b.add("fake", []byte("not audio"), defaultSoundSettings()); err == nil {
			t.Error("a non-audio file was added")
		}
		if _, err := b.add("loud", fakeWAV, soundSettings{Volume: 150}); err == nil {
			t.Error("volume 150 was accepted")
		}
		if _, err := b.add("roll", fakeWAV, defaultSoundSettings()); err == nil {
			t.Error("a sound took over !roll")
		}
	})

	t.Run("replace can change format, keeping the settings", func(t *testing.T) {
		b, dir := testSoundBoard(t)
		b.add("buhee", fakeWAV, soundSettings{CooldownSeconds: 9, Volume: 70})
		s, err := b.replace("buhee", fakeMP3)
		if err != nil {
			t.Fatal(err)
		}
		if fileExists(filepath.Join(dir, "buhee.wav")) || !fileExists(filepath.Join(dir, "buhee.mp3")) {
			t.Error("want buhee.mp3 in place of buhee.wav")
		}
		if s.CooldownSeconds != 9 || s.Volume != 70 {
			t.Errorf("settings = %+v, want them kept", s.soundSettings)
		}
	})

	t.Run("rename moves the file, settings and running cooldown", func(t *testing.T) {
		b, dir := testSoundBoard(t)
		b.add("buhee", fakeWAV, soundSettings{CooldownSeconds: 10, Volume: 100})
		t0 := time.Now()
		b.trigger("buhee", "alice", t0)
		s, err := b.update("buhee", "boohee", soundSettings{CooldownSeconds: 10, Volume: 50})
		if err != nil {
			t.Fatal(err)
		}
		if s.File != "boohee.wav" || fileExists(filepath.Join(dir, "buhee.wav")) {
			t.Errorf("renamed to %+v, want boohee.wav and no buhee.wav", s)
		}
		if b.trigger("boohee", "bob", t0.Add(time.Second)) {
			t.Error("renaming reset the cooldown")
		}
		raw, _ := os.ReadFile(filepath.Join(dir, soundSettingsFile))
		if strings.Contains(string(raw), `"buhee"`) || !strings.Contains(string(raw), `"boohee"`) {
			t.Errorf("settings file = %s, want boohee's entry and none for buhee", raw)
		}
	})

	t.Run("rename refuses a name that's taken", func(t *testing.T) {
		b, _ := testSoundBoard(t)
		b.add("buhee", fakeWAV, defaultSoundSettings())
		b.add("punch", fakeMP3, defaultSoundSettings())
		if _, err := b.update("buhee", "punch", defaultSoundSettings()); err == nil {
			t.Error("renamed buhee over punch")
		}
	})

	t.Run("remove deletes the file and its settings", func(t *testing.T) {
		b, dir := testSoundBoard(t)
		b.add("buhee", fakeWAV, soundSettings{CooldownSeconds: 3, Volume: 100})
		if err := b.remove("buhee"); err != nil {
			t.Fatal(err)
		}
		if fileExists(filepath.Join(dir, "buhee.wav")) {
			t.Error("buhee.wav still there")
		}
		raw, _ := os.ReadFile(filepath.Join(dir, soundSettingsFile))
		if strings.Contains(string(raw), "buhee") {
			t.Errorf("settings file still mentions buhee: %s", raw)
		}
		if err := b.remove("buhee"); err == nil {
			t.Error("removed buhee twice")
		}
	})
}

func TestSoundListReply(t *testing.T) {
	if got := soundListReply(nil); !strings.Contains(got, "no sound") {
		t.Errorf("empty reply = %q", got)
	}
	if got, want := soundListReply([]sound{{Name: "araara"}, {Name: "buhee"}}), "sounds: !araara !buhee"; got != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
	var many []sound
	for i := 0; i < 100; i++ {
		many = append(many, sound{Name: strings.Repeat("x", 30)})
	}
	if got := soundListReply(many); len(got) > 500 || !strings.Contains(got, "more") {
		t.Errorf("long reply is %d chars (Twitch caps at 500): %q", len(got), got)
	}
}
