package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Dabi's shop: a market stall with three boxes, each selling one cosmetic for
// channel points. The streamer picks what goes in the boxes, and what each
// item costs, on /admin's Shop tab. The /shop page, the browser source for the
// Dabi-Shop scene, draws the stall with them and redraws on every change.
// What's in stock, and whether the shop is open, lives in shop.json in the
// data dir.
//
// Chat buys with the "Buy item 1/2/3" channel-point rewards. Twitch only lets
// the app that made a reward switch it on and off or refund it, so the bot
// makes them itself: whenever the shop opens, closes or is restocked while
// open, the controller sends the receiver the state they should be in
// (channel.command.sync_rewards) and the receiver makes Twitch match. A reward
// is only on while the shop is open and its box has something in it.

const shopFile = "shop.json"

// shopBoxes is how many boxes the stall has, numbered 1 to 3 on the art.
const shopBoxes = 3

// defaultShopPrice is what an item costs until it's given a price: 300
// channel points, the same as Twitch gives for following a channel.
const defaultShopPrice = 300

// maxShopPrice only catches typos.
const maxShopPrice = 1_000_000

// shopRewardTitle is each box's reward, by box number.
const shopRewardTitle = "Buy item %d"

// shopRewardColor is the rewards' background in chat's rewards menu: the pink
// of the stall's sign.
const shopRewardColor = "#E8338E"

// shopMaxPerStream is how many times a chatter can redeem each box's reward
// in one stream. Items can be doubled up (for trading, some day), and this
// keeps that from getting silly. Twitch counts it per reward, so per box.
const shopMaxPerStream = 3

// shopClosedRefund answers a redemption that got in while the shop was
// closed, which shouldn't happen: the rewards are switched off then.
const shopClosedRefund = "How on earth did you manage to redeem while the shop was closed?! Take your points back but seriously, tell me"

// shopStock is shop.json. Prices are kept per item, so one that leaves the
// shop comes back at the price it had.
type shopStock struct {
	Open   bool           `json:"open"`
	Boxes  []string       `json:"boxes"`            // cosmetic ids, one per box; "" leaves it empty
	Prices map[string]int `json:"prices"`           // by id; an item without one costs defaultShopPrice
	Before *shopBefore    `json:"before,omitempty"` // the scene and music the open shop replaced
}

// shopBox is one box as the pages draw it. Item is "" for an empty box.
type shopBox struct {
	Item  string `json:"item"`
	URL   string `json:"url,omitempty"`
	Price int    `json:"price,omitempty"`
}

// commandSender is the part of commandPublisher the shop uses, so tests can
// see what it sends.
type commandSender interface {
	publish(ctx context.Context, eventType string, payload any) error
}

type shopStore struct {
	path      string
	hub       *overlayHub
	cosmetics *cosmeticCatalogStore
	commands  commandSender // nil leaves the rewards alone
	stage     *shopStage    // nil leaves OBS and the music alone

	mu    sync.Mutex
	stock shopStock
}

// newShopStore loads shop.json. With no file the shop starts empty and
// closed, and a broken one is logged and treated the same rather than
// stopping startup.
func newShopStore(path string, hub *overlayHub, cosmetics *cosmeticCatalogStore, commands commandSender) *shopStore {
	s := &shopStore{path: path, hub: hub, cosmetics: cosmetics, commands: commands}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		log.Printf("shop: can't read %s, starting with an empty shop: %v", path, err)
	default:
		if err := json.Unmarshal(data, &s.stock); err != nil {
			log.Printf("shop: ignoring bad %s, starting with an empty shop: %v", path, err)
			s.stock = shopStock{}
		}
	}
	s.stock.Boxes = append(s.stock.Boxes, make([]string, shopBoxes)...)[:shopBoxes]
	if s.stock.Prices == nil {
		s.stock.Prices = map[string]int{}
	}
	return s
}

func (s *shopStore) priceLocked(id string) int {
	if price, ok := s.stock.Prices[id]; ok {
		return price
	}
	return defaultShopPrice
}

func (s *shopStore) isOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stock.Open
}

// boxes resolves the stock against the catalog. A box whose item has since
// been deleted shows as empty.
func (s *shopStore) boxes() []shopBox {
	cat := s.cosmetics.get()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]shopBox, shopBoxes)
	for i, name := range s.stock.Boxes {
		if name == "" {
			continue
		}
		if id, item, ok := cat.lookup(name); ok {
			out[i] = shopBox{Item: id, URL: overlayURL(item.Src), Price: s.priceLocked(id)}
		}
	}
	return out
}

// adminState is what the Shop tab draws: whether it's open, the boxes, every
// item it could put in one, and the price each would go on sale at.
func (s *shopStore) adminState() map[string]any {
	boxes := s.boxes()
	cat := s.cosmetics.get()
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]map[string]any, 0, len(cat.Items))
	for _, id := range cat.ids() {
		items = append(items, map[string]any{"id": id, "url": overlayURL(cat.Items[id].Src), "price": s.priceLocked(id)})
	}
	return map[string]any{
		"open":           s.stock.Open,
		"boxes":          boxes,
		"items":          items,
		"default_price":  defaultShopPrice,
		"max_per_stream": shopMaxPerStream,
	}
}

func (s *shopStore) saveLocked(next shopStock) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		return err
	}
	s.stock = next
	return nil
}

// set restocks the stall: one entry per box, each an item (or "" for empty)
// and its price. It saves, redraws /shop, and if the shop is open, updates
// the rewards to match.
func (s *shopStore) set(ctx context.Context, boxes []shopBox) error {
	if len(boxes) != shopBoxes {
		return badRequest("the stall has %d boxes, not %d", shopBoxes, len(boxes))
	}
	cat := s.cosmetics.get()
	next := shopStock{Boxes: make([]string, shopBoxes), Prices: map[string]int{}}
	seen := map[string]int{}
	for i, box := range boxes {
		name := strings.TrimSpace(box.Item)
		if name == "" {
			continue
		}
		id, _, ok := cat.lookup(name)
		if !ok {
			return badRequest("there's no cosmetic called %s", name)
		}
		if other, dup := seen[id]; dup {
			return badRequest("%s is already in box %d", id, other+1)
		}
		seen[id] = i
		if box.Price < 1 || box.Price > maxShopPrice {
			return badRequest("%s's price has to be from 1 to %d channel points", id, maxShopPrice)
		}
		next.Boxes[i] = id
		next.Prices[id] = box.Price
	}

	s.mu.Lock()
	next.Open, next.Before = s.stock.Open, s.stock.Before
	for id, price := range s.stock.Prices {
		if _, set := next.Prices[id]; !set {
			next.Prices[id] = price
		}
	}
	err := s.saveLocked(next)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.broadcast()
	if next.Open {
		s.syncRewards(ctx)
	}
	return nil
}

// setOpen opens or closes the shop, saves that, and switches the rewards to
// match.
func (s *shopStore) setOpen(ctx context.Context, open bool) error {
	s.mu.Lock()
	next := s.stock
	next.Open = open
	err := s.saveLocked(next)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.syncRewards(ctx)
	return nil
}

// setBefore remembers what the shop replaced in OBS, for !closeshop to put back.
func (s *shopStore) setBefore(before *shopBefore) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.stock
	next.Before = before
	return s.saveLocked(next)
}

// takeBefore hands back what the shop replaced in OBS, and forgets it.
func (s *shopStore) takeBefore() *shopBefore {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.stock.Before
	if before != nil {
		next := s.stock
		next.Before = nil
		if err := s.saveLocked(next); err != nil {
			log.Printf("shop: failed to save: %v", err)
		}
	}
	return before
}

// rewardSpecs is how the rewards should be, one per box.
func (s *shopStore) rewardSpecs() []map[string]any {
	boxes := s.boxes()
	open := s.isOpen()
	specs := make([]map[string]any, 0, len(boxes))
	for i, box := range boxes {
		cost, prompt := defaultShopPrice, "Nothing in this box right now."
		if box.Item != "" {
			cost = box.Price
			prompt = fmt.Sprintf("Buys %s from Dabi's shop. It goes in your !wardrobe: type !equip %s to wear it.", box.Item, box.Item)
		}
		specs = append(specs, map[string]any{
			"title":                   fmt.Sprintf(shopRewardTitle, i+1),
			"cost":                    cost,
			"prompt":                  prompt,
			"enabled":                 open && box.Item != "",
			"background_color":        shopRewardColor,
			"max_per_user_per_stream": shopMaxPerStream,
		})
	}
	return specs
}

// syncRewards sends the rewards' state to the receiver. seq lets it ignore a
// sync that arrives after a newer one.
func (s *shopStore) syncRewards(ctx context.Context) {
	if s.commands == nil {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.commands.publish(opCtx, "channel.command.sync_rewards", map[string]any{
		"seq":     time.Now().UnixNano(),
		"rewards": s.rewardSpecs(),
	}); err != nil {
		log.Printf("shop: failed to send the rewards' state: %v", err)
	}
}

// shopRewardBox is which box a reward title buys from, or 0 if it isn't one
// of the shop's.
func shopRewardBox(title string) int {
	for box := 1; box <= shopBoxes; box++ {
		if strings.EqualFold(strings.TrimSpace(title), fmt.Sprintf(shopRewardTitle, box)) {
			return box
		}
	}
	return 0
}

// sell settles a redemption of box (1 to 3) that cost the buyer cost points.
// On a sale the item goes in c's wardrobe, unworn (another copy if they have
// it already), and item names it; otherwise refusal is the chat reply saying
// why their points are going back.
func (s *shopStore) sell(c *character, box, cost int) (item, refusal string) {
	boxes := s.boxes()
	switch {
	case !s.isOpen():
		return "", shopClosedRefund
	case box < 1 || box > len(boxes) || boxes[box-1].Item == "":
		return "", fmt.Sprintf("box %d is empty, so your points are back.", box)
	case boxes[box-1].Price != cost:
		return "", fmt.Sprintf("the price of %s just changed, so your points are back.", boxes[box-1].Item)
	}
	id, err := s.cosmetics.get().addUnworn(c, boxes[box-1].Item)
	if err != nil {
		return "", err.Error() + ", so your points are back."
	}
	return id, ""
}

// openMessage tells chat what's for sale, and how to buy it.
func (s *shopStore) openMessage() string {
	var stock, rewards []string
	for i, box := range s.boxes() {
		if box.Item != "" {
			stock = append(stock, fmt.Sprintf("Box %d: %s, %d points.", i+1, box.Item, box.Price))
			rewards = append(rewards, fmt.Sprintf(shopRewardTitle, i+1))
		}
	}
	if len(stock) == 0 {
		return "Dabi's shop is open, but its boxes are empty."
	}
	which := rewards[len(rewards)-1]
	if len(rewards) > 1 {
		which = strings.Join(rewards[:len(rewards)-1], ", ") + " or " + which
	}
	return fmt.Sprintf("Dabi's shop is open! %s Redeem %s to buy.", strings.Join(stock, " "), which)
}

// handleShopCommand is !openshop and !closeshop, which only the broadcaster
// can use.
func handleShopCommand(ctx context.Context, event map[string]any, lower string, shop *shopStore, commands commandSender) {
	broadcasterID := firstString(event["broadcaster_user_id"], "")
	if broadcasterID == "" || firstString(event["chatter_user_id"], "") != broadcasterID {
		return
	}
	open := lower == "!openshop"
	if err := shop.setOpen(ctx, open); err != nil {
		log.Printf("shop: failed to save it as open=%v: %v", open, err)
		return
	}
	log.Printf("shop: open=%v", open)
	shop.stage.queue(open)
	message := "Dabi's shop is closed. Thanks for shopping!"
	if open {
		message = shop.openMessage()
	}
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := commands.publish(opCtx, "channel.command.send_chat", map[string]any{"channel_id": broadcasterID, "message": message}); err != nil {
		log.Printf("shop: failed to tell chat: %v", err)
	}
}

// handleShopRedeem settles a "Buy item" redemption: the item goes in the
// buyer's wardrobe and the redemption is marked fulfilled, or their points
// go back and chat hears why.
func handleShopRedeem(ctx context.Context, event map[string]any, box int, shop *shopStore, characters *characterStore, party *partyManager, commands commandSender) {
	userID := firstString(event["user_id"], "")
	userLogin := firstString(event["user_login"], event["user_name"], userID)
	broadcasterID := firstString(event["broadcaster_user_id"], "")
	reward, _ := event["reward"].(map[string]any)
	rewardID := firstString(reward["id"], "")
	redemptionID := firstString(event["id"], "")
	cost, _ := reward["cost"].(float64)
	if userID == "" || rewardID == "" || redemptionID == "" {
		log.Printf("shop: %s redemption is missing an id", fmt.Sprintf(shopRewardTitle, box))
		return
	}

	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	settle := func(status, message string) {
		if err := commands.publish(opCtx, "channel.command.redemption_status", map[string]any{
			"reward_id":     rewardID,
			"redemption_id": redemptionID,
			"status":        status,
		}); err != nil {
			log.Printf("shop: failed to mark %s's redemption %s: %v", userLogin, status, err)
		}
		if broadcasterID != "" {
			if err := commands.publish(opCtx, "channel.command.send_chat", map[string]any{"channel_id": broadcasterID, "message": "@" + userLogin + " " + message}); err != nil {
				log.Printf("shop: failed to tell %s: %v", userLogin, err)
			}
		}
	}
	refund := func(reply string) {
		log.Printf("shop: refunding %s for box %d: %s", userLogin, box, reply)
		settle("CANCELED", reply)
	}
	const broke = "something went wrong, so your points are back."

	c, err := characters.getOrCreate(opCtx, userID, userLogin)
	if err != nil {
		log.Printf("shop: failed to load %s: %v", userLogin, err)
		refund(broke)
		return
	}
	// A possessed character's live party copy is what the next exp tick
	// saves, so the purchase has to land there or it'd be overwritten.
	if live := party.findInParty(c.Name); live != nil {
		c = live
	}
	item, refusal := shop.sell(c, box, int(cost))
	if refusal != "" {
		refund(refusal)
		return
	}
	if err := characters.save(opCtx, c); err != nil {
		takeCosmetic(c, item) // they're getting their points back, so not the item
		log.Printf("shop: failed to save %s: %v", c.Name, err)
		refund(broke)
		return
	}
	copies := copiesOf(c, item)
	log.Printf("shop: %s bought %s for %d points (now has %d)", c.Name, item, int(cost), copies)
	if copies > 1 {
		settle("FULFILLED", fmt.Sprintf("bought another %s! You have %d now.", item, copies))
		return
	}
	settle("FULFILLED", fmt.Sprintf("bought %s! Type !equip %s to wear it.", item, item))
}

// broadcast redraws /shop.
func (s *shopStore) broadcast() {
	broadcastJSON(s.hub, map[string]any{"type": "shop.update", "boxes": s.boxes()})
}

// serveStock is GET /shop/stock: the boxes as they are now, which /shop loads
// on start and after a reconnect, so it never waits for the next change.
func (s *shopStore) serveStock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"boxes": s.boxes()})
}
