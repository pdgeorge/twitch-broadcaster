package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sound commands: any chatter types !<name> and the overlay plays <name>.wav
// (or .mp3/.ogg) from the sounds folder, data/sounds on the Pi. The folder is
// the list of sounds — the file name is the command — and sounds.json beside
// them holds each one's cooldown and volume. /admin manages both, but a file
// copied in by hand works too, on the default settings.
//
// A sound can also have variants: come.2.wav and come.3.wav beside come.wav
// make !come play one of the three at random, by chances set on /admin (an
// even share until then), each at its own volume, all under !come's one
// cooldown. come2.wav, with no dot, is still a command of its own.

const soundSettingsFile = "sounds.json"

// defaultSoundCooldownSeconds is the cooldown for a sound with no saved
// settings, and what the /admin upload form starts on.
const defaultSoundCooldownSeconds = 5

const maxSoundCooldownSeconds = 24 * 60 * 60

// soundExts are the formats sniffAudio recognises, all of which OBS's browser
// source can play.
var soundExts = []string{".wav", ".mp3", ".ogg"}

// soundNamePattern keeps names to one chat word that's also a safe file name.
// Variant ids (the 2 in come.2.wav) follow it too.
var soundNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// reservedSoundNames are commands something already answers, here or in
// DabiReborn (!dabi, !dabireset, !dabichat), so a sound with the same name
// would go off on top of them.
var reservedSoundNames = []string{
	"sounds", "roll", "wardrobe", "equip", "unequip",
	"other", "fire", "grant", "smite", "bless", "revive", "give", "take", "newparty", "kick", "sheet",
	"dabi", "dabireset", "dabichat",
}

// checkSoundName says why name can't be a sound command, or nil if it can.
func checkSoundName(name string) error {
	if !soundNamePattern.MatchString(name) {
		return badRequest("%q won't work as a command: use up to 32 lowercase letters, digits, - and _", name)
	}
	if slices.Contains(reservedSoundNames, name) {
		return badRequest("!%s is already a command", name)
	}
	return nil
}

// soundSettings is how a sound plays: the cooldown is the whole command's,
// the volume its main file's.
type soundSettings struct {
	CooldownSeconds int `json:"cooldown_seconds"`
	Volume          int `json:"volume"` // percent
}

func defaultSoundSettings() soundSettings {
	return soundSettings{CooldownSeconds: defaultSoundCooldownSeconds, Volume: 100}
}

func (s soundSettings) check() error {
	if s.CooldownSeconds < 0 || s.CooldownSeconds > maxSoundCooldownSeconds {
		return badRequest("cooldown has to be between 0 and %d seconds", maxSoundCooldownSeconds)
	}
	return checkPercent("volume", s.Volume)
}

// checkPercent refuses a volume or chance outside 0-100.
func checkPercent(what string, n int) error {
	if n < 0 || n > 100 {
		return badRequest("%s has to be between 0 and 100", what)
	}
	return nil
}

// soundEntry is a sound's line in sounds.json. Chance (the main file's) and
// Variants only appear once the sound has variants, and a chance that isn't
// there means an even share.
type soundEntry struct {
	soundSettings
	Chance   *int                       `json:"chance,omitempty"`
	Variants map[string]variantSettings `json:"variants,omitempty"`
}

// variantSettings is how one variant plays.
type variantSettings struct {
	Volume int  `json:"volume"` // percent
	Chance *int `json:"chance,omitempty"`
}

// UnmarshalJSON starts from full volume, so a volume a hand edit leaves out
// doesn't mean silence.
func (v *variantSettings) UnmarshalJSON(data []byte) error {
	type plain variantSettings // without this method, so it doesn't recurse
	p := plain{Volume: 100}
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*v = variantSettings(p)
	return nil
}

func (e soundEntry) check() error {
	if err := e.soundSettings.check(); err != nil {
		return err
	}
	chances := []*int{e.Chance}
	for _, v := range e.Variants {
		if err := checkPercent("volume", v.Volume); err != nil {
			return err
		}
		chances = append(chances, v.Chance)
	}
	for _, c := range chances {
		if c != nil {
			if err := checkPercent("chance", *c); err != nil {
				return err
			}
		}
	}
	return nil
}

// sound is one playable command, as a scan of the folder found it: its main
// file, <name>.wav, and any variants.
type sound struct {
	Name string `json:"name"`
	File string `json:"file"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
	soundSettings
	// Chance is how often the main file is the one that plays, next to the
	// variants' chances: 100 when there are none.
	Chance   int         `json:"chance"`
	Variants []soundFile `json:"variants"`
}

// soundFile is one file a sound can play: a variant (come.2.wav is !come's
// variant "2"), or with no ID, the main file.
type soundFile struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Volume int    `json:"volume"` // percent
	Chance int    `json:"chance"` // percent
}

// files lists everything the sound can play, its main file first.
func (s sound) files() []soundFile {
	main := soundFile{File: s.File, URL: s.URL, Size: s.Size, Volume: s.Volume, Chance: s.Chance}
	return append([]soundFile{main}, s.Variants...)
}

// file finds one of the sound's files by variant id, "" being the main one.
func (s sound) file(id string) (soundFile, bool) {
	for _, f := range s.files() {
		if f.ID == id {
			return f, true
		}
	}
	return soundFile{}, false
}

// soundBoard is the sounds folder plus the cooldowns, behind one lock so an
// /admin upload or rename can't race a chat trigger.
type soundBoard struct {
	dir string
	hub *overlayHub
	// roll picks a number in [0, n) to choose between variants: rand.Intn,
	// or something fixed in tests.
	roll func(n int) int

	mu         sync.Mutex
	lastPlayed map[string]time.Time
	warned     map[string]bool
}

func newSoundBoard(dir string, hub *overlayHub) *soundBoard {
	return &soundBoard{dir: dir, hub: hub, roll: rand.Intn, lastPlayed: map[string]time.Time{}, warned: map[string]bool{}}
}

// warnOnceLocked logs a problem with a file the first time a scan trips over
// it, rather than on every chat command after.
func (b *soundBoard) warnOnceLocked(key, format string, args ...any) {
	if b.warned[key] {
		return
	}
	b.warned[key] = true
	log.Printf(format, args...)
}

// readSettingsLocked loads sounds.json. A field a hand edit leaves out keeps
// its default (so a missing volume doesn't mean silence), and a broken file
// is logged and ignored rather than muting anything.
func (b *soundBoard) readSettingsLocked() map[string]soundEntry {
	settings := map[string]soundEntry{}
	data, err := os.ReadFile(filepath.Join(b.dir, soundSettingsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return settings
	}
	var raw map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(data, &raw)
	}
	if err != nil {
		b.warnOnceLocked(soundSettingsFile, "sounds: ignoring %s, every sound is on default settings: %v", soundSettingsFile, err)
		return settings
	}
	delete(b.warned, soundSettingsFile)
	for name, msg := range raw {
		e := soundEntry{soundSettings: defaultSoundSettings()}
		if err := json.Unmarshal(msg, &e); err != nil || e.check() != nil {
			b.warnOnceLocked(soundSettingsFile+"/"+name, "sounds: ignoring bad settings for !%s: %s", name, msg)
			continue
		}
		settings[strings.ToLower(name)] = e
	}
	return settings
}

// writeSettingsLocked saves settings for the sounds and variants that exist,
// dropping any left over from files deleted by hand.
func (b *soundBoard) writeSettingsLocked(settings map[string]soundEntry) error {
	sounds, err := b.scanLocked()
	if err != nil {
		return err
	}
	for name, e := range settings {
		s, ok := sounds[name]
		if !ok {
			delete(settings, name)
			continue
		}
		for id := range e.Variants {
			if _, ok := s.file(id); !ok || id == "" {
				delete(e.Variants, id)
			}
		}
		if len(s.Variants) == 0 {
			e.Chance, e.Variants = nil, nil
			settings[name] = e
		}
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(b.dir, soundSettingsFile), append(data, '\n'))
}

// scanLocked reads the folder fresh every time: it's a handful of files, and
// this way a sound added, replaced or deleted by any route is live at once.
// Files whose name won't work as a command are skipped (and logged once), as
// are variants whose sound has no file of its own.
func (b *soundBoard) scanLocked() (map[string]sound, error) {
	entries, err := os.ReadDir(b.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]sound{}, nil
	}
	if err != nil {
		return nil, err
	}
	sounds := map[string]sound{}
	variants := map[string][]soundFile{}
	for _, entry := range entries {
		file := entry.Name()
		ext := filepath.Ext(file)
		if !entry.Type().IsRegular() || !slices.Contains(soundExts, strings.ToLower(ext)) {
			continue
		}
		// come.wav is !come, and come.2.wav its variant "2".
		name, id, isVariant := strings.Cut(strings.ToLower(strings.TrimSuffix(file, ext)), ".")
		if err := checkSoundName(name); err != nil {
			b.warnOnceLocked(file, "sounds: ignoring %s: %v", file, err)
			continue
		}
		if isVariant && !soundNamePattern.MatchString(id) {
			b.warnOnceLocked(file, "sounds: ignoring %s: %q won't work as a variant name", file, id)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		f := soundFile{ID: id, File: file, Size: info.Size()}
		// The mtime in the URL makes a replaced file a new URL, so the
		// browser source fetches it rather than replaying its cached copy.
		f.URL = "/sounds/" + url.PathEscape(file) + "?v=" + strconv.FormatInt(info.ModTime().UnixMilli(), 10)
		if isVariant {
			variants[name] = append(variants[name], f)
			continue
		}
		if other, dup := sounds[name]; dup {
			b.warnOnceLocked(file, "sounds: ignoring %s: !%s already plays %s", file, name, other.File)
			continue
		}
		sounds[name] = sound{Name: name, File: f.File, URL: f.URL, Size: f.Size, Variants: []soundFile{}}
	}

	settings := b.readSettingsLocked()
	for name, s := range sounds {
		for _, v := range variants[name] {
			if other, dup := s.file(v.ID); dup {
				b.warnOnceLocked(v.File, "sounds: ignoring %s: !%s's variant %s is already %s", v.File, name, v.ID, other.File)
				continue
			}
			s.Variants = append(s.Variants, v)
		}
		delete(variants, name)
		slices.SortFunc(s.Variants, func(a, b soundFile) int { return compareVariantIDs(a.ID, b.ID) })

		e, saved := settings[name]
		s.soundSettings = defaultSoundSettings()
		if saved {
			s.soundSettings = e.soundSettings
		}
		// A chance nobody has set is an even share (seven files with none
		// set get 15, 15, 14, 14, 14, 14, 14), and a variant nobody has set
		// plays as loud as the main file.
		even := spreadChances(make([]int, 1+len(s.Variants)), 100)
		s.Chance = 100
		if len(s.Variants) > 0 {
			s.Chance = chanceOr(e.Chance, even[0])
		}
		for i := range s.Variants {
			v := &s.Variants[i]
			vs, ok := e.Variants[v.ID]
			if !ok {
				vs = variantSettings{Volume: s.Volume}
			}
			v.Volume, v.Chance = vs.Volume, chanceOr(vs.Chance, even[i+1])
		}
		sounds[name] = s
	}
	for name, orphans := range variants {
		for _, v := range orphans {
			b.warnOnceLocked(v.File, "sounds: ignoring %s: it's a variant of !%s, which has no file of its own", v.File, name)
		}
	}
	return sounds, nil
}

// compareVariantIDs puts numbered variants in number order (2, 3, 10), then
// any named ones.
func compareVariantIDs(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		if c := cmp.Compare(na, nb); c != 0 {
			return c
		}
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// chanceOr is a saved chance, or the fallback when none was saved.
func chanceOr(saved *int, fallback int) int {
	if saved != nil {
		return *saved
	}
	return fallback
}

// findLocked scans for one sound, refusing with a 404 for /admin if it's gone.
func (b *soundBoard) findLocked(name string) (sound, map[string]sound, error) {
	sounds, err := b.scanLocked()
	if err != nil {
		return sound{}, nil, err
	}
	s, ok := sounds[name]
	if !ok {
		return sound{}, sounds, notFound("there's no !%s sound", name)
	}
	return s, sounds, nil
}

// findFileLocked finds one of a sound's files by variant id ("" for the main
// one), refusing with a 404 if either is gone.
func (b *soundBoard) findFileLocked(name, id string) (soundFile, sound, error) {
	s, _, err := b.findLocked(name)
	if err != nil {
		return soundFile{}, sound{}, err
	}
	f, ok := s.file(id)
	if !ok {
		return soundFile{}, sound{}, notFound("!%s has no variant %q", name, id)
	}
	return f, s, nil
}

// list returns every sound, sorted by name.
func (b *soundBoard) list() ([]sound, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sounds, err := b.scanLocked()
	if err != nil {
		return nil, err
	}
	out := make([]sound, 0, len(sounds))
	for _, s := range sounds {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b sound) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (b *soundBoard) broadcast(name string, f soundFile) {
	broadcastJSON(b.hub, map[string]any{
		"type":   "sound.play",
		"name":   name,
		"url":    f.URL,
		"volume": float64(f.Volume) / 100,
	})
}

// trigger plays !name if that's a sound and it's off cooldown, picking one of
// its files if it has variants. The cooldown belongs to the sound, not the
// chatter: once it plays, nobody can set it off again until cooldown_seconds
// have passed. Reports whether it played.
func (b *soundBoard) trigger(name, chatter string, now time.Time) bool {
	if checkSoundName(name) != nil {
		return false // !roll and friends never hit the disk
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sounds, err := b.scanLocked()
	if err != nil {
		log.Printf("sounds: can't read %s: %v", b.dir, err)
		return false
	}
	s, ok := sounds[name]
	if !ok {
		return false
	}
	if last, ok := b.lastPlayed[name]; ok && now.Sub(last) < time.Duration(s.CooldownSeconds)*time.Second {
		return false
	}
	f, ok := pickSoundFile(s.files(), b.roll)
	if !ok {
		return false // every file is on a 0% chance
	}
	b.lastPlayed[name] = now
	b.broadcast(name, f)
	log.Printf("sounds: !%s played %s for %s", name, f.File, chatter)
	return true
}

// pickSoundFile chooses which of a sound's files plays, by their chances:
// 50, 30 and 20 play them half, three tenths and a fifth of the time, and
// chances that don't add up to 100 count in proportion. A lone file always
// plays; ok is false only when every chance is 0.
func pickSoundFile(files []soundFile, roll func(n int) int) (soundFile, bool) {
	if len(files) == 1 {
		return files[0], true
	}
	total := 0
	for _, f := range files {
		total += f.Chance
	}
	if total <= 0 {
		return soundFile{}, false
	}
	n := roll(total)
	for _, f := range files {
		if n < f.Chance {
			return f, true
		}
		n -= f.Chance
	}
	return files[len(files)-1], true // only if roll went past total
}

// preview plays one of a sound's files ("" for the main one) on stream for
// /admin's test buttons, skipping the cooldown without starting one.
func (b *soundBoard) preview(name, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, _, err := b.findFileLocked(name, id)
	if err != nil {
		return err
	}
	b.broadcast(name, f)
	return nil
}

// add saves uploaded audio as a new sound.
func (b *soundBoard) add(name string, audio []byte, settings soundSettings) (sound, error) {
	if err := checkSoundName(name); err != nil {
		return sound{}, err
	}
	if err := settings.check(); err != nil {
		return sound{}, err
	}
	ext, err := sniffAudio(audio)
	if err != nil {
		return sound{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sounds, err := b.scanLocked()
	if err != nil {
		return sound{}, err
	}
	if _, exists := sounds[name]; exists {
		return sound{}, conflict("!%s already exists, replace its file instead", name)
	}
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return sound{}, err
	}
	path := filepath.Join(b.dir, name+ext)
	if err := writeFileAtomic(path, audio); err != nil {
		return sound{}, err
	}
	all := b.readSettingsLocked()
	all[name] = soundEntry{soundSettings: settings}
	if err := b.writeSettingsLocked(all); err != nil {
		os.Remove(path)
		return sound{}, err
	}
	s, _, err := b.findLocked(name)
	return s, err
}

// replace swaps in new audio for one of a sound's files ("" for the main
// one), keeping its name and settings. The format can change (wav to mp3),
// in which case the old file goes.
func (b *soundBoard) replace(name, id string, audio []byte) (sound, error) {
	ext, err := sniffAudio(audio)
	if err != nil {
		return sound{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	old, _, err := b.findFileLocked(name, id)
	if err != nil {
		return sound{}, err
	}
	file := soundFileBase(name, id) + ext
	if strings.EqualFold(file, old.File) {
		file = old.File // the same file, whatever case it was saved with
	}
	if err := writeFileAtomic(filepath.Join(b.dir, file), audio); err != nil {
		return sound{}, err
	}
	if file != old.File {
		if err := os.Remove(filepath.Join(b.dir, old.File)); err != nil {
			log.Printf("sounds: replaced !%s but couldn't delete %s: %v", name, old.File, err)
		}
	}
	s, _, err := b.findLocked(name)
	return s, err
}

// update saves a sound's settings, and its main file's chance if it has
// variants, renaming it (variants and all) first if newName differs.
func (b *soundBoard) update(name, newName string, settings soundSettings, chance *int) (sound, error) {
	if err := settings.check(); err != nil {
		return sound{}, err
	}
	if chance != nil {
		if err := checkPercent("chance", *chance); err != nil {
			return sound{}, err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, sounds, err := b.findLocked(name)
	if err != nil {
		return sound{}, err
	}
	if newName != name {
		if err := checkSoundName(newName); err != nil {
			return sound{}, err
		}
		if _, exists := sounds[newName]; exists {
			return sound{}, conflict("!%s already exists", newName)
		}
		if err := b.renameFilesLocked(s, newName); err != nil {
			return sound{}, err
		}
		if last, ok := b.lastPlayed[name]; ok {
			b.lastPlayed[newName] = last
			delete(b.lastPlayed, name)
		}
	}
	all := b.readSettingsLocked()
	e := all[name]
	delete(all, name)
	e.soundSettings = settings
	if chance != nil && len(s.Variants) > 0 {
		e.Chance = chance
	}
	all[newName] = e
	if err := b.writeSettingsLocked(all); err != nil {
		return sound{}, err
	}
	s, _, err = b.findLocked(newName)
	return s, err
}

// renameFilesLocked moves all of a sound's files over to newName, moving back
// the ones already done if one fails.
func (b *soundBoard) renameFilesLocked(s sound, newName string) error {
	var moved [][2]string
	for _, f := range s.files() {
		from := filepath.Join(b.dir, f.File)
		to := filepath.Join(b.dir, soundFileBase(newName, f.ID)+strings.ToLower(filepath.Ext(f.File)))
		if err := os.Rename(from, to); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				os.Rename(moved[i][1], moved[i][0])
			}
			return err
		}
		moved = append(moved, [2]string{from, to})
	}
	return nil
}

// remove deletes a sound: its file, its variants' and its settings.
func (b *soundBoard) remove(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, _, err := b.findLocked(name)
	if err != nil {
		return err
	}
	// The variants first: if one won't go, !name is still there to retry.
	for _, v := range s.Variants {
		if err := os.Remove(filepath.Join(b.dir, v.File)); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(b.dir, s.File)); err != nil {
		return err
	}
	delete(b.lastPlayed, name)
	all := b.readSettingsLocked()
	delete(all, name)
	return b.writeSettingsLocked(all)
}

// addVariant saves uploaded audio as another file !name can play, numbered
// after the ones it has (come.2.wav, come.3.wav, ...). It takes an even share
// of the chances, the others giving theirs up in proportion, and starts at
// the main file's volume.
func (b *soundBoard) addVariant(name string, audio []byte) (sound, error) {
	ext, err := sniffAudio(audio)
	if err != nil {
		return sound{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s, _, err := b.findLocked(name)
	if err != nil {
		return sound{}, err
	}
	id := nextVariantID(s)
	path := filepath.Join(b.dir, soundFileBase(name, id)+ext)
	if err := writeFileAtomic(path, audio); err != nil {
		return sound{}, err
	}
	files := s.files()
	share := (100 + (len(files)+1)/2) / (len(files) + 1) // 100/(n+1), rounded
	chances := append(spreadChances(chancesOf(files), 100-share), share)
	files = append(files, soundFile{ID: id, Volume: s.Volume})
	all := b.readSettingsLocked()
	all[name] = entryWithChances(s.soundSettings, files, chances)
	if err := b.writeSettingsLocked(all); err != nil {
		os.Remove(path)
		return sound{}, err
	}
	s, _, err = b.findLocked(name)
	return s, err
}

// updateVariant saves one variant's volume and chance. The other chances stay
// as they are, and /admin says when they no longer add up to 100.
func (b *soundBoard) updateVariant(name, id string, volume, chance int) (sound, error) {
	if err := checkPercent("volume", volume); err != nil {
		return sound{}, err
	}
	if err := checkPercent("chance", chance); err != nil {
		return sound{}, err
	}
	if id == "" {
		return sound{}, notFound("!%s has no variant %q", name, id)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, s, err := b.findFileLocked(name, id)
	if err != nil {
		return sound{}, err
	}
	files := s.files()
	chances := chancesOf(files)
	for i := range files {
		if files[i].ID == id {
			files[i].Volume, chances[i] = volume, chance
		}
	}
	all := b.readSettingsLocked()
	all[name] = entryWithChances(s.soundSettings, files, chances)
	if err := b.writeSettingsLocked(all); err != nil {
		return sound{}, err
	}
	s, _, err = b.findLocked(name)
	return s, err
}

// removeVariant deletes one of a sound's variants. The files left share out
// its chance in proportion, so they still add up to 100.
func (b *soundBoard) removeVariant(name, id string) (sound, error) {
	if id == "" {
		return sound{}, notFound("!%s has no variant %q", name, id)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f, s, err := b.findFileLocked(name, id)
	if err != nil {
		return sound{}, err
	}
	if err := os.Remove(filepath.Join(b.dir, f.File)); err != nil {
		return sound{}, err
	}
	left := slices.DeleteFunc(s.files(), func(f soundFile) bool { return f.ID == id })
	all := b.readSettingsLocked()
	all[name] = entryWithChances(s.soundSettings, left, spreadChances(chancesOf(left), 100))
	if err := b.writeSettingsLocked(all); err != nil {
		return sound{}, err
	}
	s, _, err = b.findLocked(name)
	return s, err
}

// entryWithChances is a sound's sounds.json entry with every file's chance
// written out, the main file's first, as the variant edits save them.
func entryWithChances(settings soundSettings, files []soundFile, chances []int) soundEntry {
	e := soundEntry{soundSettings: settings}
	if len(files) < 2 {
		return e // no variants, so nothing to choose between
	}
	main := chances[0]
	e.Chance = &main
	e.Variants = map[string]variantSettings{}
	for i, f := range files[1:] {
		chance := chances[i+1]
		e.Variants[f.ID] = variantSettings{Volume: f.Volume, Chance: &chance}
	}
	return e
}

// nextVariantID numbers a new variant: 2 for the first (the main file being
// 1, in effect), then the lowest number that's free.
func nextVariantID(s sound) string {
	for n := 2; ; n++ {
		if _, taken := s.file(strconv.Itoa(n)); !taken {
			return strconv.Itoa(n)
		}
	}
}

// soundFileBase is a sound file's name without its extension: come, or
// come.2 for variant 2.
func soundFileBase(name, id string) string {
	if id == "" {
		return name
	}
	return name + "." + id
}

func chancesOf(files []soundFile) []int {
	chances := make([]int, len(files))
	for i, f := range files {
		chances[i] = f.Chance
	}
	return chances
}

// spreadChances scales chances to add up to total, keeping their proportions,
// and gives what rounding leaves over to the biggest fractions. All zeros
// share evenly.
func spreadChances(chances []int, total int) []int {
	if len(chances) == 0 {
		return nil
	}
	weights := slices.Clone(chances)
	sum := 0
	for _, w := range weights {
		sum += w
	}
	if sum == 0 {
		for i := range weights {
			weights[i] = 1
		}
		sum = len(weights)
	}
	out := make([]int, len(weights))
	order := make([]int, len(weights)) // biggest fraction lost to rounding first
	given := 0
	for i, w := range weights {
		out[i] = w * total / sum
		given += out[i]
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(weights[b]*total%sum, weights[a]*total%sum)
	})
	for i := 0; given < total; i++ {
		out[order[i]]++
		given++
	}
	return out
}

// sniffAudio names the format from the data itself, so the stored extension
// is right whatever the uploaded file was called.
func sniffAudio(data []byte) (string, error) {
	switch {
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return ".wav", nil
	case bytes.HasPrefix(data, []byte("ID3")),
		// A bare MPEG frame sync. Layer bits 00 would be AAC's ADTS header,
		// which OBS's browser source may not have the codec for.
		len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0 && data[1]&0x06 != 0:
		return ".mp3", nil
	case bytes.HasPrefix(data, []byte("OggS")):
		return ".ogg", nil
	}
	return "", badRequest("that doesn't look like a wav, mp3 or ogg file")
}

// soundListReply is the !sounds answer, cut short to fit Twitch's
// 500-character message limit.
func soundListReply(sounds []sound) string {
	if len(sounds) == 0 {
		return "there are no sound commands yet"
	}
	reply := "sounds:"
	for i, s := range sounds {
		next := " !" + s.Name
		if len(reply)+len(next) > 460 {
			return reply + fmt.Sprintf(" (and %d more)", len(sounds)-i)
		}
		reply += next
	}
	return reply
}

// handleSoundsCommand answers !sounds (any chatter) with every sound command.
func handleSoundsCommand(ctx context.Context, event map[string]any, sounds *soundBoard, commands *commandPublisher) {
	broadcasterID := firstString(event["broadcaster_user_id"], "")
	if broadcasterID == "" {
		return
	}
	list, err := sounds.list()
	if err != nil {
		log.Printf("sounds: !sounds failed: %v", err)
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := commands.publish(opCtx, "channel.command.send_chat", map[string]any{
		"channel_id": broadcasterID,
		"message":    soundListReply(list),
	}); err != nil {
		log.Printf("sounds: failed to reply to !sounds: %v", err)
	}
}
