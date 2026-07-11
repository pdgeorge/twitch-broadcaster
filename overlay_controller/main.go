package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"strconv"

        "github.com/gorilla/websocket"
        amqp "github.com/rabbitmq/amqp091-go"
        "github.com/yuin/goldmark"
        _ "github.com/go-sql-driver/mysql"
)

type config struct {
	rabbitURL      string
	rabbitExchange string
	commandExchange string
	queueName      string
	httpPort       string
	staticDir      string
	mysqlDSN       string
}

type chatFragment struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	EmoteID  string `json:"emote_id,omitempty"`
	EmoteURL string `json:"emote_url,omitempty"`
}

type chatMessage struct {
	Type         string         `json:"type"`
	Username     string         `json:"username"`
	Message      string         `json:"message"`
	MessageHTML  string         `json:"message_html"`
	Fragments    []chatFragment `json:"fragments,omitempty"`
	ChannelLogin string         `json:"channel_login,omitempty"`
	ChannelID    string         `json:"channel_id,omitempty"`
}

type eventPayload struct {
	EventType    string         `json:"event_type"`
	EventVersion string         `json:"event_version"`
	Event        map[string]any `json:"event"`
	Metadata     map[string]any `json:"metadata"`
}

type overlayHub struct {
	mu        sync.Mutex
	clients   map[*websocket.Conn]struct{}
	broadcast chan []byte
}

type loginStore struct {
	db *sql.DB
}

type botCommandStore struct {
	db      *sql.DB
	mu      sync.RWMutex
	commands map[string]string // trigger (lowercase) -> response
}

type commandPublisher struct {
	url      string
	exchange string
	conn     *amqp.Connection
	channel  *amqp.Channel
}

const dailyLoginRewardTitle = "daily login bonus"
const joinPartyRewardTitle = "join the party"
const partyMaxSize = 4
const expCooldownDuration = 45 * time.Second
const expLevelDivisor = 100 // level = 1 + exp/expLevelDivisor; placeholder curve, tune freely

func newOverlayHub() *overlayHub {
	return &overlayHub{
		clients:   make(map[*websocket.Conn]struct{}),
		broadcast: make(chan []byte, 128),
	}
}

type otherManager struct {
	mu                 sync.Mutex
	hub                *overlayHub
	baseHTML           string
	announcementTimer  *time.Timer
	pongStop           chan struct{}
	pongInitialMessage string
}

func (h *overlayHub) add(conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[conn] = struct{}{}
}

func (h *overlayHub) remove(conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, conn)
}

func (h *overlayHub) run(ctx context.Context) {
	for {
		select {
		case msg := <-h.broadcast:
			h.mu.Lock()
			for conn := range h.clients {
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					log.Printf("failed to write to websocket client: %v", err)
					conn.Close()
					delete(h.clients, conn)
				}
			}
			h.mu.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

func newLoginStore(db *sql.DB) *loginStore {
	return &loginStore{db: db}
}

func newBotCommandStore(db *sql.DB) *botCommandStore {
	return &botCommandStore{
		db:       db,
		commands: make(map[string]string),
	}
}

func newCommandPublisher(cfg config) *commandPublisher {
	return &commandPublisher{
		url:      cfg.rabbitURL,
		exchange: cfg.commandExchange,
	}
}

func (p *commandPublisher) ensureConnected() error {
	if p.conn != nil && !p.conn.IsClosed() && p.channel != nil && !p.channel.IsClosed() {
		return nil
	}

	conn, err := amqp.Dial(p.url)
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return err
	}
	if err := ch.ExchangeDeclare(p.exchange, "fanout", true, false, false, false, nil); err != nil {
		ch.Close()
		conn.Close()
		return err
	}

	p.conn = conn
	p.channel = ch
	return nil
}

func (p *commandPublisher) publish(ctx context.Context, eventType string, payload any) error {
	if err := p.ensureConnected(); err != nil {
		return err
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	err = p.channel.PublishWithContext(ctx, p.exchange, "", false, false, amqp.Publishing{
		ContentType: "application/json",
		Type:        eventType,
		Body:        body,
	})
	if err != nil {
		_ = p.channel.Close()
		_ = p.conn.Close()
		p.channel = nil
		p.conn = nil
	}
	return err
}

func (p *commandPublisher) close() {
	if p.channel != nil {
		_ = p.channel.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
}

func (s *loginStore) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS chatters (
	twitch_chatter_id   BIGINT       NOT NULL,
	twitch_chatter_name VARCHAR(64)  NOT NULL,
	logins              INT          NOT NULL DEFAULT 0,
	exp                 INT          NOT NULL DEFAULT 0,
	money               INT          NOT NULL DEFAULT 0,
	cosmetics           JSON         NULL,
	last_seen_at        DATETIME     NULL,
	created_at          DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at          DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	PRIMARY KEY (twitch_chatter_id),
	INDEX idx_chatters_name (twitch_chatter_name)
	)`)
	return err
}

func (s *loginStore) increment(ctx context.Context, userID, userLogin string) (int64, error) {
	// Convert Twitch user id from string -> int64 for BIGINT column
	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid userID %q: %w", userID, err)
	}

	_, err = s.db.ExecContext(ctx, `
	INSERT INTO chatters (twitch_chatter_id, twitch_chatter_name, logins, last_seen_at)
	VALUES (?, ?, 1, NOW())
	ON DUPLICATE KEY UPDATE
	  logins = logins + 1,
	  twitch_chatter_name = VALUES(twitch_chatter_name),
	  last_seen_at = NOW()
	`, id, userLogin)
	if err != nil {
		return 0, err
	}

	// Fetch the new count
	row := s.db.QueryRowContext(ctx, `
	SELECT logins FROM chatters WHERE twitch_chatter_id = ?
	`, id)

	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// character is the West Marches character sheet for one chatter, backed by
// the chatters table. Exp/hp/level/alive/money/cosmetics are the live values;
// mutations always write straight back to MySQL (no batching).
type character struct {
	UserID    int64
	Name      string
	Logins    int64
	Exp       int64
	Level     int
	HP        int
	MaxHP     int
	Alive     bool
	Money     int64
	Cosmetics []string
}

func (c *character) expNext() int64 {
	return int64(c.Level) * expLevelDivisor
}

// applyExp recomputes level/max_hp/hp after an exp change. Level-up restores
// HP to the new max; level never decreases HP below the (possibly lower) cap.
func (c *character) applyExp(delta int64) {
	c.Exp += delta
	if c.Exp < 0 {
		c.Exp = 0
	}
	newLevel := 1 + int(c.Exp/expLevelDivisor)
	if newLevel < 1 {
		newLevel = 1
	}
	leveledUp := newLevel > c.Level
	c.Level = newLevel
	c.MaxHP = 10 + c.Level*4
	if leveledUp {
		c.HP = c.MaxHP
	} else if c.HP > c.MaxHP {
		c.HP = c.MaxHP
	}
}

// spriteVariant is the deterministic tavern-sprite tint index used until a
// chatter has explicit cosmetics: hash(username) % 9.
func spriteVariant(name string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(name)))
	return int(h.Sum32() % 9)
}

type characterStore struct {
	db *sql.DB
}

func newCharacterStore(db *sql.DB) *characterStore {
	return &characterStore{db: db}
}

func (s *characterStore) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
	ALTER TABLE chatters
	  ADD COLUMN IF NOT EXISTS level  INT  NOT NULL DEFAULT 1,
	  ADD COLUMN IF NOT EXISTS hp     INT  NOT NULL DEFAULT 14,
	  ADD COLUMN IF NOT EXISTS max_hp INT  NOT NULL DEFAULT 14,
	  ADD COLUMN IF NOT EXISTS alive  TINYINT(1) NOT NULL DEFAULT 1,
	  ADD COLUMN IF NOT EXISTS sheet  JSON NULL
	`)
	return err
}

func scanCharacter(row *sql.Row) (*character, error) {
	var c character
	var cosmeticsJSON sql.NullString
	var alive bool
	if err := row.Scan(&c.UserID, &c.Name, &c.Logins, &c.Exp, &c.Money, &cosmeticsJSON, &c.Level, &c.HP, &c.MaxHP, &alive); err != nil {
		return nil, err
	}
	c.Alive = alive
	if cosmeticsJSON.Valid && cosmeticsJSON.String != "" && cosmeticsJSON.String != "null" {
		_ = json.Unmarshal([]byte(cosmeticsJSON.String), &c.Cosmetics)
	}
	return &c, nil
}

const characterSelectColumns = "twitch_chatter_id, twitch_chatter_name, logins, exp, money, cosmetics, level, hp, max_hp, alive"

// getOrCreate ensures a chatters row exists for userID, then returns the
// full character sheet. userLogin is used only to seed/refresh the name.
func (s *characterStore) getOrCreate(ctx context.Context, userID, userLogin string) (*character, error) {
	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid userID %q: %w", userID, err)
	}

	_, err = s.db.ExecContext(ctx, `
	INSERT INTO chatters (twitch_chatter_id, twitch_chatter_name, last_seen_at)
	VALUES (?, ?, NOW())
	ON DUPLICATE KEY UPDATE
	  twitch_chatter_name = VALUES(twitch_chatter_name),
	  last_seen_at = NOW()
	`, id, userLogin)
	if err != nil {
		return nil, err
	}

	row := s.db.QueryRowContext(ctx, "SELECT "+characterSelectColumns+" FROM chatters WHERE twitch_chatter_id = ?", id)
	return scanCharacter(row)
}

// getByName looks up a character by chatter name (case-insensitive), for DM
// commands that only have a username to go on. Returns nil, nil if unknown.
func (s *characterStore) getByName(ctx context.Context, name string) (*character, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+characterSelectColumns+" FROM chatters WHERE twitch_chatter_name = ? LIMIT 1", name)
	c, err := scanCharacter(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *characterStore) save(ctx context.Context, c *character) error {
	cosmeticsJSON, err := json.Marshal(c.Cosmetics)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
	UPDATE chatters SET exp = ?, money = ?, cosmetics = ?, level = ?, hp = ?, max_hp = ?, alive = ?
	WHERE twitch_chatter_id = ?
	`, c.Exp, c.Money, string(cosmeticsJSON), c.Level, c.HP, c.MaxHP, c.Alive, c.UserID)
	return err
}

func (s *botCommandStore) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS bot_commands (
	` + "`trigger`" + `      VARCHAR(64)  NOT NULL,
	response     TEXT         NOT NULL,
	created_by   VARCHAR(64)  NOT NULL,
	created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	PRIMARY KEY (` + "`trigger`" + `)
	)`)
	if err != nil {
		return err
	}
	return s.loadAll(ctx)
}

func (s *botCommandStore) loadAll(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT `trigger`, response FROM bot_commands")
	if err != nil {
		return err
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var trigger, response string
		if err := rows.Scan(&trigger, &response); err != nil {
			return err
		}
		m[trigger] = response
	}

	s.mu.Lock()
	s.commands = m
	s.mu.Unlock()

	log.Printf("bot_commands: loaded %d command(s) from DB", len(m))
	return rows.Err()
}

// set upserts a command into the DB and, if verified, updates the in-memory map.
func (s *botCommandStore) set(ctx context.Context, trigger, response, createdBy string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO bot_commands (`trigger`, response, created_by) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE response = VALUES(response), created_by = VALUES(created_by)", trigger, response, createdBy)
	if err != nil {
		return err
	}

	// Verify it's actually in the DB before updating the in-memory map.
	var got string
	row := s.db.QueryRowContext(ctx, "SELECT response FROM bot_commands WHERE `trigger` = ?", trigger)
	if err := row.Scan(&got); err != nil {
		return fmt.Errorf("verification failed after set: %w", err)
	}

	s.mu.Lock()
	s.commands[trigger] = got
	s.mu.Unlock()
	return nil
}

// delete removes a command from the DB and, if confirmed gone, removes it from the in-memory map.
func (s *botCommandStore) delete(ctx context.Context, trigger string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM bot_commands WHERE `trigger` = ?", trigger)
	if err != nil {
		return err
	}

	// Verify it's actually gone before touching the map.
	var dummy string
	row := s.db.QueryRowContext(ctx, "SELECT response FROM bot_commands WHERE `trigger` = ?", trigger)
	if scanErr := row.Scan(&dummy); scanErr == nil {
		return fmt.Errorf("verification failed after delete: row still exists")
	} else if scanErr != sql.ErrNoRows {
		return fmt.Errorf("verification failed after delete: %w", scanErr)
	}

	s.mu.Lock()
	delete(s.commands, trigger)
	s.mu.Unlock()
	return nil
}

// lookup returns the response for a trigger, or ("", false) if not found.
func (s *botCommandStore) lookup(trigger string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	resp, ok := s.commands[trigger]
	return resp, ok
}

func (h *overlayHub) handleWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	h.add(conn)
	log.Printf("websocket client connected: %s", r.RemoteAddr)

	defer func() {
		h.remove(conn)
		conn.Close()
		log.Printf("websocket client disconnected: %s", r.RemoteAddr)
	}()

	for {
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}

func newOtherManager(hub *overlayHub) *otherManager {
	return &otherManager{hub: hub, pongInitialMessage: "<pre class=\"pong-frame\">Starting pong...</pre>"}
}

func (o *otherManager) send(obj any) {
	data, err := json.Marshal(obj)
	if err != nil {
		log.Printf("failed to encode other payload: %v", err)
		return
	}
	select {
	case o.hub.broadcast <- data:
	default:
		log.Print("dropping other payload: broadcast channel full")
	}
}

func (o *otherManager) setBase(html string) {
	o.mu.Lock()
	o.baseHTML = html
	o.mu.Unlock()
	o.send(map[string]any{"type": "other.update", "mode": "base", "html": html})
}

func (o *otherManager) startAnnouncement(html string, duration time.Duration) {
	o.mu.Lock()
	if o.announcementTimer != nil {
		o.announcementTimer.Stop()
	}
	o.announcementTimer = time.AfterFunc(duration, func() {
		o.restoreBase("base_restore")
	})
	o.mu.Unlock()

	o.send(map[string]any{
		"type":             "other.update",
		"mode":             "announcement",
		"html":             html,
		"duration_seconds": int(duration.Seconds()),
	})
}

func (o *otherManager) cancelAnnouncement() {
	o.mu.Lock()
	if o.announcementTimer != nil {
		o.announcementTimer.Stop()
		o.announcementTimer = nil
	}
	o.mu.Unlock()
	o.restoreBase("force_restore")
}

func (o *otherManager) restoreBase(mode string) {
	o.mu.Lock()
	base := o.baseHTML
	o.mu.Unlock()
	o.send(map[string]any{"type": "other.update", "mode": mode, "html": base})
}

func (o *otherManager) startPong(duration time.Duration) {
	o.mu.Lock()
	if o.pongStop != nil {
		o.mu.Unlock()
		return
	}
	if o.announcementTimer != nil {
		o.announcementTimer.Stop()
		o.announcementTimer = nil
	}
	stopChan := make(chan struct{})
	o.pongStop = stopChan
	o.mu.Unlock()

	o.send(map[string]any{
		"type":             "other.pong_start",
		"duration_seconds": int(duration.Seconds()),
		"html":             o.pongInitialMessage,
	})

	go o.runPong(stopChan, duration)
}

func (o *otherManager) runPong(stopChan chan struct{}, duration time.Duration) {
	ticker := time.NewTicker(180 * time.Millisecond)
	defer ticker.Stop()

	width := 28
	height := 6
	paddleSize := 3
	leftX, rightX := 0, width-1

	ballX, ballY := width/2, height/2
	dx, dy := 1, 1

	leftY, rightY := height/2, height/2
	leftDir, rightDir := 1, -1

	deadline := time.After(duration)

	render := func(ballX, ballY, leftY, rightY int) string {
		grid := make([][]rune, height)
		for i := 0; i < height; i++ {
			row := make([]rune, width)
			for j := 0; j < width; j++ {
				row[j] = ' '
			}
			grid[i] = row
		}

		for offset := -1; offset <= 1; offset++ {
			if y := leftY + offset; y >= 0 && y < height {
				grid[y][leftX] = '|'
			}
			if y := rightY + offset; y >= 0 && y < height {
				grid[y][rightX] = '|'
			}
		}

		if ballY >= 0 && ballY < height && ballX >= 0 && ballX < width {
			grid[ballY][ballX] = 'O'
		}

		var sb strings.Builder
		sb.WriteString("<pre class=\"pong-frame\">")
		for _, row := range grid {
			sb.WriteString(string(row))
			sb.WriteByte('\n')
		}
		sb.WriteString("</pre>")
		return sb.String()
	}

	for {
		select {
		case <-ticker.C:
			leftY += leftDir
			if leftY <= 1 {
				leftY = 1
				leftDir = 1
			} else if leftY >= height-2 {
				leftY = height - 2
				leftDir = -1
			}

			rightY += rightDir
			if rightY <= 1 {
				rightY = 1
				rightDir = 1
			} else if rightY >= height-2 {
				rightY = height - 2
				rightDir = -1
			}

			nextX := ballX + dx
			nextY := ballY + dy

			if nextY < 0 || nextY >= height {
				dy = -dy
				nextY = ballY + dy
			}

			if nextX <= leftX {
				if abs(nextY-leftY) <= paddleSize/2 {
					dx = 1
					if nextY < leftY {
						dy = -1
					} else if nextY > leftY {
						dy = 1
					}
				} else {
					dx = 1
				}
				nextX = ballX + dx
			} else if nextX >= rightX {
				if abs(nextY-rightY) <= paddleSize/2 {
					dx = -1
					if nextY < rightY {
						dy = -1
					} else if nextY > rightY {
						dy = 1
					}
				} else {
					dx = -1
				}
				nextX = ballX + dx
			}

			ballX, ballY = nextX, nextY
			o.send(map[string]any{"type": "other.pong_frame", "html": render(ballX, ballY, leftY, rightY)})
		case <-deadline:
			o.stopPong(stopChan)
			return
		case <-stopChan:
			o.stopPong(stopChan)
			return
		}
	}
}

func (o *otherManager) stopPong(stopChan chan struct{}) {
	o.mu.Lock()
	if o.pongStop != stopChan {
		o.mu.Unlock()
		return
	}
	o.pongStop = nil
	o.mu.Unlock()
	o.restoreBase("base_restore")
}

func startPongTicker(ctx context.Context, other *otherManager) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			other.startPong(time.Minute)
		case <-ctx.Done():
			return
		}
	}
}

// broadcastJSON marshals obj and pushes it to every connected overlay client.
func broadcastJSON(hub *overlayHub, obj any) {
	data, err := json.Marshal(obj)
	if err != nil {
		log.Printf("failed to encode broadcast payload: %v", err)
		return
	}
	select {
	case hub.broadcast <- data:
	default:
		log.Print("dropping broadcast payload: channel full")
	}
}

// partyManager holds the active possessed party (max partyMaxSize) in memory.
// Membership only changes on a successful "join the party" redemption or the
// DM commands !kick / !newparty — there is deliberately no expiry timer.
type partyManager struct {
	mu      sync.Mutex
	hub     *overlayHub
	members []*character
}

func newPartyManager(hub *overlayHub) *partyManager {
	return &partyManager{hub: hub}
}

type partyMemberPayload struct {
	Name    string `json:"name"`
	Level   int    `json:"level"`
	HP      int    `json:"hp"`
	MaxHP   int    `json:"max_hp"`
	Exp     int64  `json:"exp"`
	ExpNext int64  `json:"exp_next"`
	Variant int    `json:"variant"`
	Status  string `json:"status"`
}

func (p *partyManager) broadcastLocked() {
	members := make([]partyMemberPayload, 0, len(p.members))
	for _, c := range p.members {
		members = append(members, partyMemberPayload{
			Name: c.Name, Level: c.Level, HP: c.HP, MaxHP: c.MaxHP,
			Exp: c.Exp, ExpNext: c.expNext(), Variant: spriteVariant(c.Name), Status: "possessed",
		})
	}
	broadcastJSON(p.hub, map[string]any{"type": "party.update", "members": members})
}

func (p *partyManager) findLocked(name string) (int, *character) {
	lname := strings.ToLower(name)
	for i, c := range p.members {
		if strings.ToLower(c.Name) == lname {
			return i, c
		}
	}
	return -1, nil
}

// join adds c to the party if there's room and it's alive. Returns a
// human-readable refusal reason, or "" on success.
func (p *partyManager) join(c *character) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !c.Alive {
		return "your character is dead and can't rejoin until the DM revives them"
	}
	if _, existing := p.findLocked(c.Name); existing != nil {
		return "you're already in the party"
	}
	if len(p.members) >= partyMaxSize {
		return "the party is full (4/4) — ask the DM to !newparty"
	}
	p.members = append(p.members, c)
	broadcastJSON(p.hub, map[string]any{"type": "tavern.possess", "name": c.Name})
	p.broadcastLocked()
	return ""
}

// findInParty returns the live in-party character by name (nil if absent) so
// DM commands mutate the copy that's about to be re-broadcast, not a stale one.
func (p *partyManager) findInParty(name string) *character {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, c := p.findLocked(name)
	return c
}

// kick removes one member by name (normal return to the tavern, not death).
func (p *partyManager) kick(name string) *character {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, c := p.findLocked(name)
	if c == nil {
		return nil
	}
	p.members = append(p.members[:idx], p.members[idx+1:]...)
	broadcastJSON(p.hub, map[string]any{"type": "tavern.return", "name": c.Name})
	p.broadcastLocked()
	return c
}

// removeForDeath removes a member without the "return" animation — the
// caller sends tavern.death instead. No-op if the character wasn't in the party.
func (p *partyManager) removeForDeath(name string) *character {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, c := p.findLocked(name)
	if c == nil {
		return nil
	}
	p.members = append(p.members[:idx], p.members[idx+1:]...)
	p.broadcastLocked()
	return c
}

// newParty ejects everyone and reopens all slots.
func (p *partyManager) newParty() []*character {
	p.mu.Lock()
	defer p.mu.Unlock()
	ejected := p.members
	p.members = nil
	for _, c := range ejected {
		broadcastJSON(p.hub, map[string]any{"type": "tavern.return", "name": c.Name})
	}
	p.broadcastLocked()
	return ejected
}

// notifyChange re-broadcasts party.update after a DM command mutates a
// member already in the party in place (e.g. !grant, !smite, !bless).
func (p *partyManager) notifyChange() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broadcastLocked()
}

// expCooldownTracker gates exp-on-message per chatter. Memory-only by design
// — losing it on a controller restart just means everyone's briefly un-gated.
type expCooldownTracker struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

func newExpCooldownTracker() *expCooldownTracker {
	return &expCooldownTracker{last: make(map[int64]time.Time)}
}

// ready reports whether userID is off cooldown; if so it starts a new one.
func (t *expCooldownTracker) ready(userID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if last, ok := t.last[userID]; ok && now.Sub(last) < expCooldownDuration {
		return false
	}
	t.last[userID] = now
	return true
}

func main() {
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("mysql", cfg.mysqlDSN)
	if err != nil {
		log.Fatalf("failed to open mysql db: %v", err)
	}
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("failed to ping mysql db: %v", err)
	}
	defer db.Close()

	store := newLoginStore(db)
	if err := store.init(ctx); err != nil {
		log.Fatalf("failed to prepare mysql schema: %v", err)
	}

	characters := newCharacterStore(db)
	if err := characters.init(ctx); err != nil {
		log.Fatalf("failed to prepare character schema: %v", err)
	}

	botCommands := newBotCommandStore(db)
	if err := botCommands.init(ctx); err != nil {
		log.Fatalf("failed to prepare bot_commands schema: %v", err)
	}

	commandPublisher := newCommandPublisher(cfg)
	defer commandPublisher.close()

	hub := newOverlayHub()
	other := newOtherManager(hub)
	party := newPartyManager(hub)
	expCooldown := newExpCooldownTracker()
	go hub.run(ctx)
	go startPongTicker(ctx, other)
	go func() {
		if err := consumeChat(ctx, cfg, hub, other, store, characters, party, expCooldown, botCommands, commandPublisher); err != nil {
			log.Fatalf("rabbitmq consumer stopped: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/overlay", hub.handleWS)
	mux.Handle("/", http.FileServer(http.Dir(cfg.staticDir)))

	srv := &http.Server{Addr: ":" + cfg.httpPort, Handler: mux}

	go func() {
		log.Printf("overlay_controller listening on :%s", cfg.httpPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Print("overlay_controller shutdown complete")
}

func loadConfig() config {
	return config{
		rabbitURL:      env("RABBITMQ_URL", "amqp://guest:guest@twitch_broadcaster:5672/"),
		rabbitExchange: env("RABBITMQ_EXCHANGE", "twitch_events"),
		commandExchange: env("RABBITMQ_COMMAND_EXCHANGE", "twitch_commands"),
		queueName:      env("OVERLAY_QUEUE", "overlay_chat"),
		httpPort:       env("OVERLAY_HTTP_PORT", "8080"),
		staticDir:      env("OVERLAY_STATIC_DIR", "./overlay"),
		mysqlDSN:		env("MYSQL_DSN", "echoes:echoespw@tcp(mysql:3306)/echoes?parseTime=true"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func consumeChat(ctx context.Context, cfg config, hub *overlayHub, other *otherManager, store *loginStore, characters *characterStore, party *partyManager, expCooldown *expCooldownTracker, botCommands *botCommandStore, commands *commandPublisher) error {
	for {
		conn, err := amqp.Dial(cfg.rabbitURL)
		if err != nil {
			log.Printf("failed to connect to rabbitmq: %v", err)
			select {
			case <-time.After(5 * time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		ch, err := conn.Channel()
		if err != nil {
			conn.Close()
			log.Printf("failed to open channel: %v", err)
			continue
		}

		if err := ch.ExchangeDeclare(cfg.rabbitExchange, "fanout", true, false, false, false, nil); err != nil {
			log.Printf("failed to declare exchange: %v", err)
			conn.Close()
			continue
		}

		queue, err := ch.QueueDeclare(cfg.queueName, true, false, false, false, nil)
		if err != nil {
			log.Printf("failed to declare queue: %v", err)
			conn.Close()
			continue
		}

		if err := ch.QueueBind(queue.Name, "", cfg.rabbitExchange, false, nil); err != nil {
			log.Printf("failed to bind queue: %v", err)
			conn.Close()
			continue
		}

		deliveries, err := ch.Consume(queue.Name, "", false, false, false, false, nil)
		if err != nil {
			log.Printf("failed to register consumer: %v", err)
			conn.Close()
			continue
		}

		log.Print("overlay_controller consuming chat messages from RabbitMQ")
		reconnect := make(chan struct{})
		go func() {
			select {
			case <-conn.NotifyClose(make(chan *amqp.Error)):
			case <-ctx.Done():
			}
			close(reconnect)
		}()

		consumeLoop := true
		for consumeLoop {
			select {
			case d, ok := <-deliveries:
				if !ok {
					consumeLoop = false
					break
				}
				handleDelivery(ctx, d, hub, other, store, characters, party, expCooldown, botCommands, commands)
			case <-reconnect:
				consumeLoop = false
			case <-ctx.Done():
				consumeLoop = false
			}
		}

		conn.Close()

		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func handleDelivery(ctx context.Context, d amqp.Delivery, hub *overlayHub, other *otherManager, store *loginStore, characters *characterStore, party *partyManager, expCooldown *expCooldownTracker, botCommands *botCommandStore, commands *commandPublisher) {
	defer d.Ack(false)

	var payload eventPayload
	if err := json.Unmarshal(d.Body, &payload); err != nil {
		log.Printf("failed to decode payload: %v", err)
		return
	}

	eventType := d.Type
	if eventType == "" {
		eventType = payload.EventType
	}

	switch eventType {
	case "channel.chat.message":
		handleChatEvent(ctx, payload.Event, hub, other, characters, party, expCooldown, botCommands, commands)
	case "channel.channel_points_custom_reward_redemption.add":
		handleRedeemEvent(ctx, payload.Event, other, store, characters, party, commands)
	default:
		return
	}
}

func formatChat(event map[string]any) *chatMessage {
	if event == nil {
		return nil
	}

	username := firstString(event["chatter_user_name"], event["chatter_user_login"], event["broadcaster_user_name"], "chat")
	messageText := messageTextFromEvent(event)

	if messageText == "" {
		return nil
	}

	fragments := extractFragments(event)
	channelLogin := firstString(event["broadcaster_user_login"], event["chatter_user_login"], event["broadcaster_user_name"], event["chatter_user_name"], "")
	channelID := firstString(event["broadcaster_user_id"], event["chatter_user_id"], "")

	msg := &chatMessage{
		Type:         "chat.message",
		Username:     username,
		Message:      messageText,
		Fragments:    fragments,
		ChannelLogin: channelLogin,
		ChannelID:    channelID,
	}

	msg.MessageHTML = renderHTML(msg)
	return msg
}

func extractFragments(event map[string]any) []chatFragment {
	msgVal, ok := event["message"].(map[string]any)
	if !ok {
		return nil
	}

	fragVal, ok := msgVal["fragments"]
	if !ok {
		return nil
	}
	fragSlice, ok := fragVal.([]any)
	if !ok {
		return nil
	}

	var frags []chatFragment
	for _, raw := range fragSlice {
		fragMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeVal := firstString(fragMap["type"], "")
		text := firstString(fragMap["text"], "")
		if typeVal == "text" {
			frags = append(frags, chatFragment{Type: "text", Text: text})
			continue
		}

		if typeVal == "emote" {
			emoteData, _ := fragMap["emote"].(map[string]any)
			emoteID := firstString(emoteData["id"], "")
			format := pickEmoteFormat(emoteData["format"])
			if emoteID != "" {
				emoteURL := fmt.Sprintf("https://static-cdn.jtvnw.net/emoticons/v2/%s/%s/dark/2.0", emoteID, format)
				frags = append(frags, chatFragment{Type: "emote", Text: text, EmoteID: emoteID, EmoteURL: emoteURL})
				continue
			}
		}
	}
	return frags
}

func pickEmoteFormat(v any) string {
	formats, ok := v.([]any)
	if !ok {
		return "static"
	}
	for _, f := range formats {
		if s, ok := f.(string); ok {
			if s == "animated" {
				return s
			}
		}
	}
	return "static"
}

func renderHTML(msg *chatMessage) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("<span class='username'>%s</span>:", html.EscapeString(msg.Username)))

	if len(msg.Fragments) > 0 {
		for _, f := range msg.Fragments {
			switch f.Type {
			case "text":
				parts = append(parts, fmt.Sprintf(" <span class='text'>%s</span>", html.EscapeString(f.Text)))
			case "emote":
				if f.EmoteURL != "" {
					parts = append(parts, fmt.Sprintf(" <img class='emote' src='%s' alt='%s' />", html.EscapeString(f.EmoteURL), html.EscapeString(f.Text)))
				} else {
					parts = append(parts, fmt.Sprintf(" %s", html.EscapeString(f.Text)))
				}
			}
		}
	} else if msg.Message != "" {
		parts = append(parts, fmt.Sprintf(" <span class='text'>%s</span>", html.EscapeString(msg.Message)))
	}

	return strings.TrimSpace(strings.Join(parts, ""))
}

func firstString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func messageTextFromEvent(event map[string]any) string {
	if msg, ok := event["message"].(map[string]any); ok {
		return firstString(msg["text"], msg["message"], "")
	}
	return ""
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

func markdownToHTML(input string) string {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(input), &buf); err != nil {
		log.Printf("failed to render markdown: %v", err)
		return html.EscapeString(input)
	}
	return buf.String()
}

func normalizeMarkdownInput(input string) string {
	normalized := strings.ReplaceAll(input, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\\r\\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\\n", "\n")
	return normalized
}

func handleChatEvent(ctx context.Context, event map[string]any, hub *overlayHub, other *otherManager, characters *characterStore, party *partyManager, expCooldown *expCooldownTracker, botCommands *botCommandStore, commands *commandPublisher) {
	if event == nil {
		return
	}

	messageText := messageTextFromEvent(event)
	lower := strings.ToLower(strings.TrimSpace(messageText))

	if strings.Contains(lower, "ping") {
		other.startPong(time.Minute)
	}

	grantMessageExp(ctx, event, characters, party, expCooldown)

	if isAuthorizedForOther(event) {
		handleDMCommand(ctx, event, lower, messageText, other, characters, party, commands)
		if strings.HasPrefix(lower, "!other ") {
			content := strings.TrimSpace(messageText[len("!other "):])
			other.setBase(markdownToHTML(normalizeMarkdownInput(content)))
		} else if lower == "!fire" {
			other.cancelAnnouncement()
		} else if strings.HasPrefix(lower, "#a ") {
			// Format: #a <trigger> <response>
			rest := strings.TrimSpace(messageText[len("#a "):])
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) == 2 {
				trigger := strings.ToLower(strings.TrimSpace(parts[0]))
				response := strings.TrimSpace(parts[1])
				username := firstString(event["chatter_user_login"], event["chatter_user_name"], "mod")
				broadcasterID := firstString(event["broadcaster_user_id"], "")
				opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := botCommands.set(opCtx, trigger, response, username); err != nil {
					cancel()
					log.Printf("bot_commands: failed to add %q: %v", trigger, err)
					if broadcasterID != "" {
						_ = commands.publish(ctx, "channel.command.send_chat", map[string]any{
							"channel_id": broadcasterID,
							"message":    fmt.Sprintf("Unable to add %s", trigger),
						})
					}
				} else {
					cancel()
					log.Printf("bot_commands: added %q by %s", trigger, username)
					if broadcasterID != "" {
						_ = commands.publish(ctx, "channel.command.send_chat", map[string]any{
							"channel_id": broadcasterID,
							"message":    fmt.Sprintf("Added the %s command.", trigger),
						})
					}
				}
			} else {
				log.Printf("bot_commands: malformed #a command: %q", messageText)
			}
		} else if strings.HasPrefix(lower, "#d ") {
			// Format: #d <trigger>
			trigger := strings.ToLower(strings.TrimSpace(messageText[len("#d "):]))
			broadcasterID := firstString(event["broadcaster_user_id"], "")
			opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := botCommands.delete(opCtx, trigger); err != nil {
				cancel()
				log.Printf("bot_commands: failed to delete %q: %v", trigger, err)
				if broadcasterID != "" {
					_ = commands.publish(ctx, "channel.command.send_chat", map[string]any{
						"channel_id": broadcasterID,
						"message":    fmt.Sprintf("Unable to delete %s", trigger),
					})
				}
			} else {
				cancel()
				log.Printf("bot_commands: deleted %q", trigger)
				if broadcasterID != "" {
					_ = commands.publish(ctx, "channel.command.send_chat", map[string]any{
						"channel_id": broadcasterID,
						"message":    fmt.Sprintf("Deleted the %s command.", trigger),
					})
				}
			}
		}
	}

	// Command lookup — any chatter, only bother checking !-prefixed messages.
	if strings.HasPrefix(lower, "!") {
		if response, ok := botCommands.lookup(lower); ok {
			broadcasterID := firstString(event["broadcaster_user_id"], "")
			if broadcasterID != "" {
				opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := commands.publish(opCtx, "channel.command.send_chat", map[string]any{
					"channel_id": broadcasterID,
					"message":    response,
				}); err != nil {
					log.Printf("bot_commands: failed to respond to %q: %v", lower, err)
				}
				cancel()
			}
		}
	}

	msg := formatChat(event)
	if msg == nil {
		return
	}

	encoded, err := json.Marshal(msg)
	if err != nil {
		log.Printf("failed to encode chat message: %v", err)
		return
	}

	select {
	case hub.broadcast <- encoded:
	default:
		log.Print("dropping chat message: broadcast channel full")
	}
}

// grantMessageExp awards flat per-message exp (gated by expCooldown) and
// persists it immediately (design decision: exp writes on every grant, not
// batched). Never blocks chat rendering — errors just get logged.
func grantMessageExp(ctx context.Context, event map[string]any, characters *characterStore, party *partyManager, expCooldown *expCooldownTracker) {
	userID := firstString(event["chatter_user_id"], "")
	userLogin := firstString(event["chatter_user_login"], event["chatter_user_name"], userID)
	if userID == "" {
		return
	}
	id, err := strconv.ParseInt(userID, 10, 64)
	if err != nil {
		return
	}
	if !expCooldown.ready(id) {
		return
	}

	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	c, err := characters.getOrCreate(opCtx, userID, userLogin)
	if err != nil {
		log.Printf("exp-on-message: failed to load character for %s: %v", userLogin, err)
		return
	}
	if live := party.findInParty(c.Name); live != nil {
		c = live
	}

	c.applyExp(int64(10 + c.Logins/10))

	if err := characters.save(opCtx, c); err != nil {
		log.Printf("exp-on-message: failed to save character for %s: %v", userLogin, err)
		return
	}
	if party.findInParty(c.Name) != nil {
		party.notifyChange()
	}
}

func trimName(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

// killCharacter marks c dead, ejects it from the party (if present) with a
// death event instead of the normal return, and persists.
func killCharacter(ctx context.Context, c *character, party *partyManager, characters *characterStore) {
	c.HP = 0
	c.Alive = false
	party.removeForDeath(c.Name)
	broadcastJSON(party.hub, map[string]any{"type": "tavern.death", "name": c.Name})
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := characters.save(opCtx, c); err != nil {
		log.Printf("killCharacter: failed to save %s: %v", c.Name, err)
	}
}

// handleDMCommand parses and executes the broadcaster/mod-only party
// commands from design doc §6 (minus !extend and !season, both dropped: no
// possession timer exists to extend, and the campaign table was cut). No-ops
// for anything that isn't one of these prefixes — the caller still runs the
// pre-existing !other/!fire/#a/#d chain afterward.
func handleDMCommand(ctx context.Context, event map[string]any, lower, messageText string, other *otherManager, characters *characterStore, party *partyManager, commands *commandPublisher) {
	broadcasterID := firstString(event["broadcaster_user_id"], "")
	reply := func(format string, args ...any) {
		if broadcasterID == "" {
			return
		}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = commands.publish(opCtx, "channel.command.send_chat", map[string]any{
			"channel_id": broadcasterID,
			"message":    fmt.Sprintf(format, args...),
		})
	}

	// resolveForEdit returns the character to mutate: the live in-party copy
	// if possessed (so edits land in the next party.update), otherwise a
	// fresh load from the DB.
	resolveForEdit := func(rawName string) (*character, error) {
		name := trimName(rawName)
		if live := party.findInParty(name); live != nil {
			return live, nil
		}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return characters.getByName(opCtx, name)
	}

	persist := func(c *character) {
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := characters.save(opCtx, c); err != nil {
			log.Printf("dm command: failed to save %s: %v", c.Name, err)
		}
		if party.findInParty(c.Name) != nil {
			party.notifyChange()
		}
	}

	fields := strings.Fields(messageText)

	switch {
	case strings.HasPrefix(lower, "!grant "):
		if len(fields) != 4 {
			reply("usage: !grant <name> <exp|hp> <n>")
			return
		}
		field := strings.ToLower(fields[2])
		n, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			reply("!grant: %q isn't a number", fields[3])
			return
		}
		c, err := resolveForEdit(fields[1])
		if err != nil || c == nil {
			reply("!grant: no character named %s", fields[1])
			return
		}
		switch field {
		case "exp":
			c.applyExp(n)
			persist(c)
			reply("%s: exp now %d (level %d)", c.Name, c.Exp, c.Level)
		case "hp":
			c.HP += int(n)
			if c.HP > c.MaxHP {
				c.HP = c.MaxHP
			}
			if c.HP <= 0 {
				killCharacter(ctx, c, party, characters)
				reply("💀 %s has fallen", c.Name)
				return
			}
			persist(c)
			reply("%s: hp now %d/%d", c.Name, c.HP, c.MaxHP)
		default:
			reply("!grant: field must be exp or hp")
		}

	case strings.HasPrefix(lower, "!smite "):
		if len(fields) != 3 {
			reply("usage: !smite <name> <n>")
			return
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			reply("!smite: %q isn't a number", fields[2])
			return
		}
		c, err := resolveForEdit(fields[1])
		if err != nil || c == nil {
			reply("!smite: no character named %s", fields[1])
			return
		}
		c.HP -= n
		if c.HP <= 0 {
			killCharacter(ctx, c, party, characters)
			reply("💀 %s has fallen", c.Name)
			return
		}
		persist(c)
		reply("%s takes %d damage (%d/%d hp)", c.Name, n, c.HP, c.MaxHP)

	case strings.HasPrefix(lower, "!bless "):
		if len(fields) != 3 {
			reply("usage: !bless <name> <n>")
			return
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil || n < 0 {
			reply("!bless: %q isn't a positive number", fields[2])
			return
		}
		c, err := resolveForEdit(fields[1])
		if err != nil || c == nil {
			reply("!bless: no character named %s", fields[1])
			return
		}
		c.HP += n
		if c.HP > c.MaxHP {
			c.HP = c.MaxHP
		}
		persist(c)
		reply("%s healed to %d/%d hp", c.Name, c.HP, c.MaxHP)

	case strings.HasPrefix(lower, "!give "):
		if len(fields) != 3 {
			reply("usage: !give <name> <amount|cosmetic>")
			return
		}
		c, err := resolveForEdit(fields[1])
		if err != nil || c == nil {
			reply("!give: no character named %s", fields[1])
			return
		}
		if n, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			c.Money += n
			persist(c)
			reply("%s: money now %d", c.Name, c.Money)
		} else {
			c.Cosmetics = append(c.Cosmetics, fields[2])
			persist(c)
			reply("%s: granted cosmetic %q", c.Name, fields[2])
		}

	case lower == "!newparty":
		ejected := party.newParty()
		for _, c := range ejected {
			opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = characters.save(opCtx, c)
			cancel()
		}
		reply("party cleared — all 4 slots open")

	case strings.HasPrefix(lower, "!kick "):
		if len(fields) != 2 {
			reply("usage: !kick <name>")
			return
		}
		name := trimName(fields[1])
		c := party.kick(name)
		if c == nil {
			reply("!kick: %s isn't in the party", name)
			return
		}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = characters.save(opCtx, c)
		cancel()
		reply("%s returns to the tavern", c.Name)

	case strings.HasPrefix(lower, "!sheet "):
		if len(fields) != 2 {
			reply("usage: !sheet <name>")
			return
		}
		c, err := resolveForEdit(fields[1])
		if err != nil || c == nil {
			reply("!sheet: no character named %s", fields[1])
			return
		}
		sheetHTML := fmt.Sprintf(
			"<h2>%s</h2><p>Level %d — %d/%d HP — %d exp — %d gold</p>",
			html.EscapeString(c.Name), c.Level, c.HP, c.MaxHP, c.Exp, c.Money,
		)
		other.startAnnouncement(sheetHTML, 30*time.Second)
	}
}

// handlePossessionRedeem processes a "join the party" channel-points
// redemption: load-or-create the character, refuse if dead or the party is
// full (manually refunded by the streamer — no auto-refund plumbing exists
// yet), otherwise seat them with no expiry.
func handlePossessionRedeem(ctx context.Context, event map[string]any, characters *characterStore, party *partyManager, commands *commandPublisher) {
	userID := firstString(event["user_id"], "")
	userLogin := firstString(event["user_login"], event["user_name"], userID)
	if userID == "" {
		log.Print("join the party redemption missing user_id")
		return
	}
	broadcasterID := firstString(event["broadcaster_user_id"], "")

	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	c, err := characters.getOrCreate(opCtx, userID, userLogin)
	if err != nil {
		log.Printf("join the party: failed to load character for %s: %v", userLogin, err)
		return
	}

	reason := party.join(c)

	if broadcasterID == "" {
		return
	}
	var message string
	if reason == "" {
		message = fmt.Sprintf("@%s's character joins the party!", userLogin)
	} else {
		message = fmt.Sprintf("@%s can't join — %s", userLogin, reason)
	}
	if err := commands.publish(opCtx, "channel.command.send_chat", map[string]any{
		"channel_id": broadcasterID,
		"message":    message,
	}); err != nil {
		log.Printf("failed to publish join-the-party chat command: %v", err)
	}
}

func handleRedeemEvent(ctx context.Context, event map[string]any, other *otherManager, store *loginStore, characters *characterStore, party *partyManager, commands *commandPublisher) {
	if event == nil {
		return
	}

	reward, _ := event["reward"].(map[string]any)
	title := strings.TrimSpace(firstString(reward["title"], ""))

	switch {
	case strings.EqualFold(title, "announcement"):
		userInput := firstString(event["user_input"], "")
		other.startAnnouncement(markdownToHTML(normalizeMarkdownInput(userInput)), 5*time.Minute)
	case strings.EqualFold(title, joinPartyRewardTitle):
		handlePossessionRedeem(ctx, event, characters, party, commands)
	case strings.EqualFold(title, dailyLoginRewardTitle),
		strings.EqualFold(title, "general_test"):
		userID := firstString(event["user_id"], "")
		userLogin := firstString(event["user_login"], event["user_name"], userID)
		if userID == "" {
			log.Print("daily login bonus redemption missing user_id")
			return
		}
		
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		
		count, err := store.increment(opCtx, userID, userLogin)
		if err != nil {
			log.Printf("failed to increment login count for %s: %v", userID, err)
			return
		}
		log.Printf("daily login incremented: user=%s count=%d", userLogin, count)

		message := fmt.Sprintf("@%s your daily login count is now %d!", userLogin, count)
		broadcasterID := firstString(event["broadcaster_user_id"], "")
		if broadcasterID == "" {
			log.Print("daily login bonus redemption missing broadcaster_user_id")
			return
		}
		if err := commands.publish(opCtx, "channel.command.send_chat", map[string]any{
			"channel_id": broadcasterID,
			"message":    message,
		}); err != nil {
			log.Printf("failed to publish daily login chat command: %v", err)
		}
	default:
		return
	}
}

func isAuthorizedForOther(event map[string]any) bool {
	chatterID := firstString(event["chatter_user_id"], "")
	broadcasterID := firstString(event["broadcaster_user_id"], "")
	if chatterID != "" && chatterID == broadcasterID {
		return true
	}

	badgesVal, ok := event["badges"].([]any)
	if !ok {
		return false
	}
	for _, raw := range badgesVal {
		badge, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if setID := firstString(badge["set_id"], ""); setID == "moderator" || setID == "broadcaster" {
			return true
		}
	}
	return false
}