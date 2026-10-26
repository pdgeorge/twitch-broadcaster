package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Dabi's shop: a market stall with three boxes, each selling one cosmetic for
// channel points. The streamer picks what goes in the boxes, and what each
// item costs, on /admin's Shop tab. The /shop page, the browser source for the
// Dabi-Shop scene, draws the stall with them and redraws on every change.
// What's in stock lives in shop.json in the data dir.

const shopFile = "shop.json"

// shopBoxes is how many boxes the stall has, numbered 1 to 3 on the art.
const shopBoxes = 3

// defaultShopPrice is what an item costs until it's given a price: 300
// channel points, the same as Twitch gives for following a channel.
const defaultShopPrice = 300

// maxShopPrice only catches typos.
const maxShopPrice = 1_000_000

// shopStock is shop.json. Prices are kept per item, so one that leaves the
// shop comes back at the price it had.
type shopStock struct {
	Boxes  []string       `json:"boxes"`  // cosmetic ids, one per box; "" leaves it empty
	Prices map[string]int `json:"prices"` // by id; an item without one costs defaultShopPrice
}

// shopBox is one box as the pages draw it. Item is "" for an empty box.
type shopBox struct {
	Item  string `json:"item"`
	URL   string `json:"url,omitempty"`
	Price int    `json:"price,omitempty"`
}

type shopStore struct {
	path      string
	hub       *overlayHub
	cosmetics *cosmeticCatalogStore

	mu    sync.Mutex
	stock shopStock
}

// newShopStore loads shop.json. With no file the shop starts empty, and a
// broken one is logged and treated the same rather than stopping startup.
func newShopStore(path string, hub *overlayHub, cosmetics *cosmeticCatalogStore) *shopStore {
	s := &shopStore{path: path, hub: hub, cosmetics: cosmetics}
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

// adminState is what the Shop tab draws: the boxes, every item it could put
// in one, and the price each would go on sale at.
func (s *shopStore) adminState() map[string]any {
	boxes := s.boxes()
	cat := s.cosmetics.get()
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]map[string]any, 0, len(cat.Items))
	for _, id := range cat.ids() {
		items = append(items, map[string]any{"id": id, "url": overlayURL(cat.Items[id].Src), "price": s.priceLocked(id)})
	}
	return map[string]any{"boxes": boxes, "items": items, "default_price": defaultShopPrice}
}

// set restocks the stall: one entry per box, each an item (or "" for empty)
// and its price. It saves, then redraws /shop.
func (s *shopStore) set(boxes []shopBox) error {
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
	for id, price := range s.stock.Prices {
		if _, set := next.Prices[id]; !set {
			next.Prices[id] = price
		}
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err == nil {
		err = writeFileAtomic(s.path, data)
	}
	if err == nil {
		s.stock = next
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.broadcast()
	return nil
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
