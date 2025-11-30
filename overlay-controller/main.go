package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/yuin/goldmark"
)

type config struct {
	rabbitURL      string
	rabbitExchange string
	queueName      string
	httpPort       string
	staticDir      string
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

func main() {
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hub := newOverlayHub()
	other := newOtherManager(hub)
	go hub.run(ctx)
	go startPongTicker(ctx, other)
	go func() {
		if err := consumeChat(ctx, cfg, hub, other); err != nil {
			log.Fatalf("rabbitmq consumer stopped: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/overlay", hub.handleWS)
	mux.Handle("/", http.FileServer(http.Dir(cfg.staticDir)))

	srv := &http.Server{Addr: ":" + cfg.httpPort, Handler: mux}

	go func() {
		log.Printf("overlay-controller listening on :%s", cfg.httpPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Print("overlay-controller shutdown complete")
}

func loadConfig() config {
	return config{
		rabbitURL:      env("RABBITMQ_URL", "amqp://guest:guest@twitch_broadcaster:5672/"),
		rabbitExchange: env("RABBITMQ_EXCHANGE", "twitch_events"),
		queueName:      env("OVERLAY_QUEUE", "overlay_chat"),
		httpPort:       env("OVERLAY_HTTP_PORT", "8080"),
		staticDir:      env("OVERLAY_STATIC_DIR", "./overlay"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func consumeChat(ctx context.Context, cfg config, hub *overlayHub, other *otherManager) error {
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

		log.Print("overlay-controller consuming chat messages from RabbitMQ")
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
				handleDelivery(d, hub, other)
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

func handleDelivery(d amqp.Delivery, hub *overlayHub, other *otherManager) {
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
		handleChatEvent(payload.Event, hub, other)
	case "channel.channel_points_custom_reward_redemption.add":
		handleRedeemEvent(payload.Event, other)
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

func handleChatEvent(event map[string]any, hub *overlayHub, other *otherManager) {
	if event == nil {
		return
	}

	messageText := messageTextFromEvent(event)
	lower := strings.ToLower(messageText)

	if strings.Contains(lower, "ping") {
		other.startPong(time.Minute)
	}

	if isAuthorizedForOther(event) {
		if strings.HasPrefix(lower, "!other ") {
			content := strings.TrimSpace(messageText[len("!other "):])
			other.setBase(markdownToHTML(normalizeMarkdownInput(content)))
		} else if strings.EqualFold(strings.TrimSpace(messageText), "!fire") {
			other.cancelAnnouncement()
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

func handleRedeemEvent(event map[string]any, other *otherManager) {
	if event == nil {
		return
	}

	reward, _ := event["reward"].(map[string]any)
	title := strings.TrimSpace(firstString(reward["title"], ""))
	if !strings.EqualFold(title, "announcement") {
		return
	}

	userInput := firstString(event["user_input"], "")
	other.startAnnouncement(markdownToHTML(normalizeMarkdownInput(userInput)), 5*time.Minute)
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
