package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
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

const soundSettingsFile = "sounds.json"

// defaultSoundCooldownSeconds is the cooldown for a sound with no saved
// settings, and what the /admin upload form starts on.
const defaultSoundCooldownSeconds = 5

const maxSoundCooldownSeconds = 24 * 60 * 60

// soundExts are the formats sniffAudio recognises, all of which OBS's browser
// source can play.
var soundExts = []string{".wav", ".mp3", ".ogg"}

// soundNamePattern keeps names to one chat word that's also a safe file name.
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
	if s.Volume < 0 || s.Volume > 100 {
		return badRequest("volume has to be between 0 and 100")
	}
	return nil
}

// sound is one playable command, as a scan of the folder found it.
type sound struct {
	Name string `json:"name"`
	File string `json:"file"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
	soundSettings
}

// soundBoard is the sounds folder plus the cooldowns, behind one lock so an
// /admin upload or rename can't race a chat trigger.
type soundBoard struct {
	dir string
	hub *overlayHub

	mu         sync.Mutex
	lastPlayed map[string]time.Time
	warned     map[string]bool
}

func newSoundBoard(dir string, hub *overlayHub) *soundBoard {
	return &soundBoard{dir: dir, hub: hub, lastPlayed: map[string]time.Time{}, warned: map[string]bool{}}
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
func (b *soundBoard) readSettingsLocked() map[string]soundSettings {
	settings := map[string]soundSettings{}
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
		s := defaultSoundSettings()
		if err := json.Unmarshal(msg, &s); err != nil || s.check() != nil {
			b.warnOnceLocked(soundSettingsFile+"/"+name, "sounds: ignoring bad settings for !%s: %s", name, msg)
			continue
		}
		settings[strings.ToLower(name)] = s
	}
	return settings
}

// writeSettingsLocked saves settings for the sounds that exist, dropping any
// left over from files deleted by hand.
func (b *soundBoard) writeSettingsLocked(settings map[string]soundSettings) error {
	sounds, err := b.scanLocked()
	if err != nil {
		return err
	}
	for name := range settings {
		if _, ok := sounds[name]; !ok {
			delete(settings, name)
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
// Files whose name won't work as a command are skipped (and logged once).
func (b *soundBoard) scanLocked() (map[string]sound, error) {
	entries, err := os.ReadDir(b.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]sound{}, nil
	}
	if err != nil {
		return nil, err
	}
	settings := b.readSettingsLocked()
	sounds := map[string]sound{}
	for _, entry := range entries {
		file := entry.Name()
		ext := filepath.Ext(file)
		if !entry.Type().IsRegular() || !slices.Contains(soundExts, strings.ToLower(ext)) {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(file, ext))
		if err := checkSoundName(name); err != nil {
			b.warnOnceLocked(file, "sounds: ignoring %s: %v", file, err)
			continue
		}
		if other, dup := sounds[name]; dup {
			b.warnOnceLocked(file, "sounds: ignoring %s: !%s already plays %s", file, name, other.File)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		s := sound{Name: name, File: file, Size: info.Size(), soundSettings: defaultSoundSettings()}
		if saved, ok := settings[name]; ok {
			s.soundSettings = saved
		}
		// The mtime in the URL makes a replaced file a new URL, so the
		// browser source fetches it rather than replaying its cached copy.
		s.URL = "/sounds/" + url.PathEscape(file) + "?v=" + strconv.FormatInt(info.ModTime().UnixMilli(), 10)
		sounds[name] = s
	}
	return sounds, nil
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

func (b *soundBoard) broadcast(s sound) {
	broadcastJSON(b.hub, map[string]any{
		"type":   "sound.play",
		"name":   s.Name,
		"url":    s.URL,
		"volume": float64(s.Volume) / 100,
	})
}

// trigger plays !name if that's a sound and it's off cooldown. The cooldown
// belongs to the sound, not the chatter: once it plays, nobody can set it off
// again until cooldown_seconds have passed. Reports whether it played.
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
	b.lastPlayed[name] = now
	b.broadcast(s)
	log.Printf("sounds: !%s played for %s", name, chatter)
	return true
}

// preview plays a sound on stream for /admin's test button, skipping the
// cooldown without starting one.
func (b *soundBoard) preview(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, _, err := b.findLocked(name)
	if err != nil {
		return err
	}
	b.broadcast(s)
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
	all[name] = settings
	if err := b.writeSettingsLocked(all); err != nil {
		os.Remove(path)
		return sound{}, err
	}
	s, _, err := b.findLocked(name)
	return s, err
}

// replace swaps in new audio for a sound, keeping its name and settings. The
// format can change (wav to mp3), in which case the old file goes.
func (b *soundBoard) replace(name string, audio []byte) (sound, error) {
	ext, err := sniffAudio(audio)
	if err != nil {
		return sound{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	old, _, err := b.findLocked(name)
	if err != nil {
		return sound{}, err
	}
	file := name + ext
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

// update saves a sound's settings, renaming it first if newName differs.
func (b *soundBoard) update(name, newName string, settings soundSettings) (sound, error) {
	if err := settings.check(); err != nil {
		return sound{}, err
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
		file := newName + strings.ToLower(filepath.Ext(s.File))
		if err := os.Rename(filepath.Join(b.dir, s.File), filepath.Join(b.dir, file)); err != nil {
			return sound{}, err
		}
		if last, ok := b.lastPlayed[name]; ok {
			b.lastPlayed[newName] = last
			delete(b.lastPlayed, name)
		}
	}
	all := b.readSettingsLocked()
	delete(all, name)
	all[newName] = settings
	if err := b.writeSettingsLocked(all); err != nil {
		return sound{}, err
	}
	s, _, err = b.findLocked(newName)
	return s, err
}

// remove deletes a sound's file and settings.
func (b *soundBoard) remove(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, _, err := b.findLocked(name)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(b.dir, s.File)); err != nil {
		return err
	}
	delete(b.lastPlayed, name)
	all := b.readSettingsLocked()
	delete(all, name)
	return b.writeSettingsLocked(all)
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
