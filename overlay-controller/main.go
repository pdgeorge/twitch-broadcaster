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

type otherBoxController struct {
	mu                sync.Mutex
	hub               *overlayHub
	markdown          goldmark.Markdown
	baseHTML          string
	announcementTimer *time.Timer
	pongCancel        context.CancelFunc
	pongRunning       bool
}

func newOtherBoxController(hub *overlayHub) *otherBoxController {
	return &otherBoxController{hub: hub, markdown: goldmark.New()}
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

func (o *otherBoxController) broadcast(msg any) {
	encoded, err := json.Marshal(msg)
	if err != nil {
		log.Printf("failed to encode overlay message: %v", err)
		return
	}
	select {
	case o.hub.broadcast <- encoded:
	default:
		log.Print("dropping overlay message: broadcast channel full")
	}
}

func (o *otherBoxController) renderMarkdown(md string) string {
	var buf bytes.Buffer
	if err := o.markdown.Convert([]byte(strings.TrimSpace(md)), &buf); err != nil {
		return html.EscapeString(md)
	}
	return buf.String()
}

func (o *otherBoxController) setBase(md string) {
	htmlContent := o.renderMarkdown(md)

	o.mu.Lock()
	o.baseHTML = htmlContent
	hasAnnouncement := o.announcementTimer != nil
	pongRunning := o.pongRunning
	o.mu.Unlock()

	if hasAnnouncement || pongRunning {
		return
	}

	o.broadcast(map[string]any{
		"type": "other.update",
		"mode": "base",
		"html": htmlContent,
	})
}

func (o *otherBoxController) startAnnouncement(md string, duration time.Duration) {
	htmlContent := o.renderMarkdown(md)

	o.mu.Lock()
	if o.announcementTimer != nil {
		o.announcementTimer.Stop()
	}
	o.announcementTimer = time.AfterFunc(duration, func() {
		o.restoreBase("base_restore")
	})
	pongRunning := o.pongRunning
	o.mu.Unlock()

	if pongRunning {
		return
	}

	o.broadcast(map[string]any{
		"type":             "other.update",
		"mode":             "announcement",
		"html":             htmlContent,
		"duration_seconds": int(duration.Seconds()),
	})
}

func (o *otherBoxController) restoreBase(mode string) {
	o.mu.Lock()
	if o.announcementTimer != nil {
		o.announcementTimer.Stop()
		o.announcementTimer = nil
	}
	htmlContent := o.baseHTML
	pongRunning := o.pongRunning
	o.mu.Unlock()

	if pongRunning {
		return
	}

	o.broadcast(map[string]any{
		"type": "other.update",
		"mode": mode,
		"html": htmlContent,
	})
}

func (o *otherBoxController) cancelAnnouncement() {
	o.mu.Lock()
	if o.announcementTimer != nil {
		o.announcementTimer.Stop()
		o.announcementTimer = nil
	}
	pongRunning := o.pongRunning
	o.mu.Unlock()

	if pongRunning {
		return
	}

	o.broadcast(map[string]any{
		"type": "other.update",
		"mode": "force_restore",
		"html": o.baseHTML,
	})
}

func (o *otherBoxController) startPong(duration time.Duration) {
	o.mu.Lock()
	if o.pongRunning {
		o.mu.Unlock()
		return
	}
	if o.announcementTimer != nil {
		o.announcementTimer.Stop()
		o.announcementTimer = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.pongCancel = cancel
	o.pongRunning = true
	o.mu.Unlock()

	o.broadcast(map[string]any{
		"type":             "other.pong_start",
		"duration_seconds": int(duration.Seconds()),
	})

	go func() {
		defer o.endPong()

		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()

		end := time.After(duration)
		width := 40
		pos := 0
		dir := 1

		for {
			select {
			case <-ticker.C:
				pos += dir
				if pos <= 0 {
					pos = 0
					dir = 1
				} else if pos >= width {
					pos = width
					dir = -1
				}

				var sb strings.Builder
				sb.WriteString("<pre>")
				sb.WriteString(strings.Repeat(" ", pos))
				sb.WriteString("o")
				sb.WriteString(strings.Repeat(" ", width-pos))
				sb.WriteString("\n")
				sb.WriteString(strings.Repeat("-", width+1))
				sb.WriteString("</pre>")

				o.broadcast(map[string]any{
					"type": "other.pong_frame",
					"html": sb.String(),
				})
			case <-end:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (o *otherBoxController) endPong() {
	o.mu.Lock()
	if o.pongCancel != nil {
		o.pongCancel()
	}
	o.pongCancel = nil
	o.pongRunning = false
	htmlContent := o.baseHTML
	o.mu.Unlock()

	o.broadcast(map[string]any{
		"type": "other.pong_end",
	})

	o.broadcast(map[string]any{
		"type": "other.update",
		"mode": "base_restore",
		"html": htmlContent,
	})
}

func main() {
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hub := newOverlayHub()
	other := newOtherBoxController(hub)
	go hub.run(ctx)
	go func() {
		if err := consumeChat(ctx, cfg, hub, other); err != nil {
			log.Fatalf("rabbitmq consumer stopped: %v", err)
		}
	}()

	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				other.startPong(1 * time.Minute)
			case <-ctx.Done():
				return
			}
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

func consumeChat(ctx context.Context, cfg config, hub *overlayHub, other *otherBoxController) error {
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

func handleDelivery(d amqp.Delivery, hub *overlayHub, other *otherBoxController) {
	defer d.Ack(false)

	var payload eventPayload
	if err := json.Unmarshal(d.Body, &payload); err != nil {
		log.Printf("failed to decode payload: %v", err)
		return
	}

	switch payload.EventType {
	case "channel.chat.message":
		msg := formatChat(payload.Event)
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

		if other != nil {
			handleChatControls(payload.Event, msg, other)
		}
	case "channel.channel_points_custom_reward_redemption.add":
		if other != nil {
			handleRedemption(payload.Event, other)
		}
	default:
		return
	}
}

func formatChat(event map[string]any) *chatMessage {
	if event == nil {
		return nil
	}

	username := firstString(event["chatter_user_name"], event["chatter_user_login"], event["broadcaster_user_name"], "chat")
	messageText := ""
	if msg, ok := event["message"].(map[string]any); ok {
		messageText = firstString(msg["text"], msg["message"], "")
	}

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

func handleChatControls(event map[string]any, msg *chatMessage, other *otherBoxController) {
	if msg == nil || other == nil {
		return
	}

	text := strings.TrimSpace(msg.Message)
	lower := strings.ToLower(text)

	if strings.Contains(lower, "ping") {
		other.startPong(1 * time.Minute)
	}

	if !isPrivileged(event) {
		return
	}

	if strings.HasPrefix(text, "!other ") {
		content := strings.TrimSpace(strings.TrimPrefix(text, "!other"))
		other.setBase(content)
		return
	}

	if lower == "!fire" {
		other.cancelAnnouncement()
	}
}

func handleRedemption(event map[string]any, other *otherBoxController) {
	if other == nil {
		return
	}

	reward, _ := event["reward"].(map[string]any)
	title := strings.ToLower(firstString(reward["title"], ""))
	if title != "announcement" {
		return
	}

	markdown := firstString(event["user_input"], "")
	other.startAnnouncement(markdown, 5*time.Minute)
}

func isPrivileged(event map[string]any) bool {
	chatterID := firstString(event["chatter_user_id"], "")
	broadcasterID := firstString(event["broadcaster_user_id"], "")
	if chatterID != "" && broadcasterID != "" && chatterID == broadcasterID {
		return true
	}

	badges, ok := event["badges"].([]any)
	if !ok {
		return false
	}
	for _, b := range badges {
		badgeMap, ok := b.(map[string]any)
		if !ok {
			continue
		}
		setID := firstString(badgeMap["set_id"], "")
		if setID == "broadcaster" || setID == "moderator" {
			return true
		}
	}
	return false
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
