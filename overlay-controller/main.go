package main

import (
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
)

type config struct {
	rabbitURL      string
	rabbitExchange string
	queueName      string
	httpPort       string
	staticDir      string
}

type chatMessage struct {
	Type        string   `json:"type"`
	Badges      []string `json:"badges"`
	Username    string   `json:"username"`
	Message     string   `json:"message"`
	MessageHTML string   `json:"message_html"`
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

func main() {
	cfg := loadConfig()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hub := newOverlayHub()
	go hub.run(ctx)
	go func() {
		if err := consumeChat(ctx, cfg, hub); err != nil {
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

func consumeChat(ctx context.Context, cfg config, hub *overlayHub) error {
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
				handleDelivery(d, hub)
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

func handleDelivery(d amqp.Delivery, hub *overlayHub) {
	defer d.Ack(false)

	if d.Type != "" && d.Type != "channel.chat.message" {
		return
	}

	var payload eventPayload
	if err := json.Unmarshal(d.Body, &payload); err != nil {
		log.Printf("failed to decode payload: %v", err)
		return
	}

	if payload.EventType != "" && payload.EventType != "channel.chat.message" {
		return
	}

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

	badges := extractBadges(event)
	badgeText := strings.Join(badges, " ")
	badgeHTML := ""
	if badgeText != "" {
		badgeHTML = fmt.Sprintf("<span class='badges'>[%s]</span> ", html.EscapeString(badgeText))
	}

	msg := &chatMessage{
		Type:        "chat.message",
		Badges:      badges,
		Username:    username,
		Message:     messageText,
		MessageHTML: fmt.Sprintf("%s<span class='username'>%s</span>: %s", badgeHTML, html.EscapeString(username), html.EscapeString(messageText)),
	}
	return msg
}

func extractBadges(event map[string]any) []string {
	badgesVal, ok := event["badges"]
	if !ok {
		return nil
	}
	badgesSlice, ok := badgesVal.([]any)
	if !ok {
		return nil
	}
	var badges []string
	for _, b := range badgesSlice {
		if badgeMap, ok := b.(map[string]any); ok {
			if setID, ok := badgeMap["set_id"].(string); ok && setID != "" {
				badges = append(badges, setID)
				continue
			}
			if id, ok := badgeMap["id"].(string); ok && id != "" {
				badges = append(badges, id)
			}
		}
	}
	return badges
}

func firstString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}
