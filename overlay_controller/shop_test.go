package main

// Tests for Dabi's shop. What's in stock has to survive a restart, never offer
// something the catalog doesn't have, and reach /shop as soon as it changes.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testShop(t *testing.T) (*shopStore, string, *overlayHub) {
	t.Helper()
	path := filepath.Join(t.TempDir(), shopFile)
	hub := newOverlayHub()
	return newShopStore(path, hub, testCatalogStore(t, testCatalogJSON), nil), path, hub
}

func TestShopStartsEmpty(t *testing.T) {
	s, _, _ := testShop(t)
	if got, want := s.boxes(), make([]shopBox, shopBoxes); !reflect.DeepEqual(got, want) {
		t.Errorf("new shop has %v, want %d empty boxes", got, shopBoxes)
	}
	for _, item := range s.adminState()["items"].([]map[string]any) {
		if item["price"] != defaultShopPrice {
			t.Errorf("%s would go on sale at %v, want the default %d", item["id"], item["price"], defaultShopPrice)
		}
	}
}

func TestShopRestock(t *testing.T) {
	s, path, hub := testShop(t)
	if err := s.set(context.Background(), []shopBox{{Item: "SHIRTA", Price: 450}, {}, {Item: "tophat", Price: 300}}); err != nil {
		t.Fatal(err)
	}
	want := []shopBox{{Item: "shirtA", URL: "/a.png", Price: 450}, {}, {Item: "tophat", URL: "/hat.png", Price: 300}}
	if got := s.boxes(); !reflect.DeepEqual(got, want) {
		t.Errorf("boxes = %v, want %v", got, want)
	}

	// /shop redraws with it...
	update := lastBroadcast(t, hub, "shop.update")
	var sent []shopBox
	raw, _ := json.Marshal(update["boxes"])
	if err := json.Unmarshal(raw, &sent); err != nil || !reflect.DeepEqual(sent, want) {
		t.Errorf("shop.update sent %s, want %v", raw, want)
	}

	// ...and it's still there after a restart.
	again := newShopStore(path, newOverlayHub(), s.cosmetics, nil)
	if got := again.boxes(); !reflect.DeepEqual(got, want) {
		t.Errorf("after a restart: %v, want %v", got, want)
	}
}

// An item keeps its price after it leaves the shop, so it comes back at it.
func TestShopRemembersPrices(t *testing.T) {
	s, _, _ := testShop(t)
	if err := s.set(context.Background(), []shopBox{{Item: "shirtA", Price: 450}, {}, {}}); err != nil {
		t.Fatal(err)
	}
	if err := s.set(context.Background(), []shopBox{{}, {Item: "shirtB", Price: 300}, {}}); err != nil {
		t.Fatal(err)
	}
	prices := map[string]any{}
	for _, item := range s.adminState()["items"].([]map[string]any) {
		prices[item["id"].(string)] = item["price"]
	}
	if prices["shirtA"] != 450 || prices["shirtB"] != 300 || prices["tophat"] != defaultShopPrice {
		t.Errorf("prices = %v, want shirtA kept at 450, shirtB 300, tophat the default", prices)
	}
}

func TestShopRefusals(t *testing.T) {
	cases := []struct {
		name  string
		boxes []shopBox
		want  string
	}{
		{"too few boxes", []shopBox{{Item: "shirtA", Price: 300}}, "3 boxes"},
		{"not in the catalog", []shopBox{{Item: "cape", Price: 300}, {}, {}}, "no cosmetic called cape"},
		{"in two boxes", []shopBox{{Item: "shirtA", Price: 300}, {Item: "SHIRTA", Price: 300}, {}}, "already in box 1"},
		{"free", []shopBox{{Item: "shirtA"}, {}, {}}, "from 1 to"},
		{"a typo", []shopBox{{Item: "shirtA", Price: maxShopPrice + 1}, {}, {}}, "from 1 to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, path, _ := testShop(t)
			err := s.set(context.Background(), tc.boxes)
			var refusal *adminError
			if !errors.As(err, &refusal) || refusal.status != http.StatusBadRequest || !strings.Contains(refusal.msg, tc.want) {
				t.Fatalf("set(%v) = %v, want a 400 saying %q", tc.boxes, err, tc.want)
			}
			if got := s.boxes(); !reflect.DeepEqual(got, make([]shopBox, shopBoxes)) {
				t.Errorf("a refused restock changed the shop to %v", got)
			}
			if fileExists(path) {
				t.Error("a refused restock wrote shop.json")
			}
		})
	}
}

// Deleting a cosmetic empties its box rather than leaving a broken image.
func TestShopDropsDeletedItems(t *testing.T) {
	s, _, _ := testShop(t)
	if err := s.set(context.Background(), []shopBox{{Item: "shirtA", Price: 300}, {Item: "tophat", Price: 300}, {}}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.WriteFile(s.cosmetics.path, []byte(`{"slots":["head"],"items":{"tophat":{"slot":"head","src":"hat.png"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.cosmetics.path, later, later); err != nil {
		t.Fatal(err)
	}
	want := []shopBox{{}, {Item: "tophat", URL: "/hat.png", Price: 300}, {}}
	if got := s.boxes(); !reflect.DeepEqual(got, want) {
		t.Errorf("boxes = %v, want shirtA's box empty: %v", got, want)
	}
}

// A shop.json broken by hand, or with fewer boxes than the stall, never stops
// the controller.
func TestShopFileFromDisk(t *testing.T) {
	cosmetics := testCatalogStore(t, testCatalogJSON)
	path := filepath.Join(t.TempDir(), shopFile)

	writeTestFile(t, path, []byte(`{"boxes": [`))
	if got := newShopStore(path, newOverlayHub(), cosmetics, nil).boxes(); !reflect.DeepEqual(got, make([]shopBox, shopBoxes)) {
		t.Errorf("broken shop.json gave %v, want an empty shop", got)
	}

	writeTestFile(t, path, []byte(`{"boxes": ["tophat"], "prices": {"tophat": 120}}`))
	want := []shopBox{{Item: "tophat", URL: "/hat.png", Price: 120}, {}, {}}
	if got := newShopStore(path, newOverlayHub(), cosmetics, nil).boxes(); !reflect.DeepEqual(got, want) {
		t.Errorf("one-box shop.json gave %v, want %v", got, want)
	}
}

func TestShopStockEndpoint(t *testing.T) {
	s, _, _ := testShop(t)
	if err := s.set(context.Background(), []shopBox{{}, {Item: "shirtB", Price: 250}, {}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.serveStock(rec, httptest.NewRequest(http.MethodGet, "/shop/stock", nil))
	var got struct {
		Boxes []shopBox `json:"boxes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /shop/stock = %d %s", rec.Code, rec.Body)
	}
	if want := []shopBox{{}, {Item: "shirtB", URL: "/b.png", Price: 250}, {}}; !reflect.DeepEqual(got.Boxes, want) {
		t.Errorf("stock = %v, want %v", got.Boxes, want)
	}

	rec = httptest.NewRecorder()
	s.serveStock(rec, httptest.NewRequest(http.MethodPost, "/shop/stock", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /shop/stock = %d, want 405", rec.Code)
	}
}

func TestAdminShop(t *testing.T) {
	a, _ := testAdmin(t, "")
	code, state := call(t, a, http.MethodGet, "/admin/api/shop", nil, "")
	if code != http.StatusOK || state["default_price"] != float64(defaultShopPrice) || len(state["boxes"].([]any)) != shopBoxes {
		t.Fatalf("GET shop = %d %v", code, state)
	}

	code, state = call(t, a, http.MethodPut, "/admin/api/shop",
		strings.NewReader(`{"boxes":[{"item":"hat","price":250},{"item":"","price":0},{"item":"shirtA","price":300}]}`), "application/json")
	if code != http.StatusOK {
		t.Fatalf("PUT shop = %d %v", code, state)
	}
	boxes := state["boxes"].([]any)
	if first := boxes[0].(map[string]any); first["item"] != "hat" || first["price"] != float64(250) || first["url"] != "/assets/cosmetics/hat.png" {
		t.Errorf("box 1 = %v, want hat at 250", first)
	}
	if second := boxes[1].(map[string]any); second["item"] != "" {
		t.Errorf("box 2 = %v, want empty", second)
	}

	code, state = call(t, a, http.MethodPut, "/admin/api/shop",
		strings.NewReader(`{"boxes":[{"item":"cape","price":250},{},{}]}`), "application/json")
	if code != http.StatusBadRequest || !strings.Contains(state["error"].(string), "cape") {
		t.Errorf("PUT unknown item = %d %v, want a 400 naming it", code, state)
	}

	// The thank-yous switch on its own leaves the boxes alone.
	code, state = call(t, a, http.MethodPut, "/admin/api/shop", strings.NewReader(`{"thanks":false}`), "application/json")
	if code != http.StatusOK || state["thanks"] != false || state["boxes"].([]any)[0].(map[string]any)["item"] != "hat" {
		t.Errorf("PUT thanks off = %d %v, want thanks off and hat still in box 1", code, state)
	}
}

// fakeSender records what the shop sends the receiver instead of publishing it.
type fakeSender struct {
	mu   sync.Mutex
	sent map[string][]map[string]any // by command type
}

func (f *fakeSender) publish(_ context.Context, eventType string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[string][]map[string]any{}
	}
	f.sent[eventType] = append(f.sent[eventType], decoded)
	return nil
}

// take hands back, and forgets, everything sent of one type.
func (f *fakeSender) take(eventType string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.sent[eventType]
	delete(f.sent, eventType)
	return got
}

func testSellingShop(t *testing.T) (*shopStore, *fakeSender, string) {
	t.Helper()
	s, path, _ := testShop(t)
	f := &fakeSender{}
	s.commands = f
	return s, f, path
}

// rewardsSynced is the one sync_rewards the shop should have just sent, by title.
func rewardsSynced(t *testing.T, f *fakeSender) map[string]map[string]any {
	t.Helper()
	syncs := f.take("channel.command.sync_rewards")
	if len(syncs) != 1 {
		t.Fatalf("sent %d reward syncs, want 1", len(syncs))
	}
	if syncs[0]["seq"] == nil {
		t.Error("the sync has no seq for the receiver to order it by")
	}
	out := map[string]map[string]any{}
	for _, raw := range syncs[0]["rewards"].([]any) {
		reward := raw.(map[string]any)
		out[reward["title"].(string)] = reward
	}
	if len(out) != shopBoxes {
		t.Fatalf("synced %d rewards, want one per box: %v", len(out), out)
	}
	return out
}

// The rewards are only on while the shop is open, for boxes with something
// in them, at that item's price.
func TestShopRewardsFollowTheShop(t *testing.T) {
	s, f, path := testSellingShop(t)
	ctx := context.Background()
	if err := s.set(ctx, []shopBox{{Item: "shirtA", Price: 450}, {}, {Item: "tophat", Price: 300}}); err != nil {
		t.Fatal(err)
	}
	if got := f.take("channel.command.sync_rewards"); len(got) != 0 {
		t.Errorf("restocking a closed shop synced the rewards: %v", got)
	}

	if err := s.setOpen(ctx, true); err != nil {
		t.Fatal(err)
	}
	rewards := rewardsSynced(t, f)
	if r := rewards["Buy item 1"]; r["enabled"] != true || r["cost"] != float64(450) || !strings.Contains(r["prompt"].(string), "shirtA") {
		t.Errorf("Buy item 1 = %v, want it on at 450, naming shirtA", r)
	}
	for title, r := range rewards {
		if r["max_per_user_per_stream"] != float64(shopMaxPerStream) {
			t.Errorf("%s allows %v per chatter per stream, want %d", title, r["max_per_user_per_stream"], shopMaxPerStream)
		}
	}
	if r := rewards["Buy item 2"]; r["enabled"] != false {
		t.Errorf("Buy item 2 = %v, want it off: its box is empty", r)
	}
	if r := rewards["Buy item 3"]; r["enabled"] != true || r["cost"] != float64(300) {
		t.Errorf("Buy item 3 = %v, want it on at 300", r)
	}

	if err := s.set(ctx, []shopBox{{Item: "shirtA", Price: 500}, {Item: "shirtB", Price: 300}, {}}); err != nil {
		t.Fatal(err)
	}
	rewards = rewardsSynced(t, f)
	if rewards["Buy item 1"]["cost"] != float64(500) || rewards["Buy item 2"]["enabled"] != true || rewards["Buy item 3"]["enabled"] != false {
		t.Errorf("restocking an open shop synced %v, want item 1 at 500, item 2 on, item 3 off", rewards)
	}

	if err := s.setOpen(ctx, false); err != nil {
		t.Fatal(err)
	}
	for title, r := range rewardsSynced(t, f) {
		if r["enabled"] != false {
			t.Errorf("closed, but %s is still on", title)
		}
	}

	if err := s.setOpen(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !newShopStore(path, newOverlayHub(), s.cosmetics, nil).isOpen() {
		t.Error("the shop forgot it was open across a restart")
	}
}

func TestShopRewardBox(t *testing.T) {
	cases := map[string]int{"Buy item 1": 1, "buy ITEM 3": 3, " Buy item 2 ": 2, "Buy item 4": 0, "Buy item": 0, "join the party": 0}
	for title, want := range cases {
		if got := shopRewardBox(title); got != want {
			t.Errorf("shopRewardBox(%q) = %d, want %d", title, got, want)
		}
	}
}

func TestShopSell(t *testing.T) {
	s, _, _ := testSellingShop(t)
	ctx := context.Background()
	if err := s.set(ctx, []shopBox{{Item: "shirtA", Price: 450}, {}, {}}); err != nil {
		t.Fatal(err)
	}
	c := newChar("Hez", 0)

	if _, why := s.sell(c, 1, 450); why != shopClosedRefund {
		t.Errorf("selling from a closed shop: %q, want %q", why, shopClosedRefund)
	}
	if err := s.setOpen(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, why := s.sell(c, 2, defaultShopPrice); why != "box 2 is empty, so your points are back." {
		t.Errorf("selling from an empty box: %q", why)
	}
	if _, why := s.sell(c, 1, 300); !strings.Contains(why, "price") {
		t.Errorf("paying an old price: %q, want a refusal about the price", why)
	}

	item, why := s.sell(c, 1, 450)
	if item != "shirtA" || why != "" {
		t.Fatalf("sell = %q, %q, want shirtA", item, why)
	}
	if !ownsCosmetic(c, "shirtA") || c.Equipped["torso"] != "" {
		t.Errorf("after buying: owns %v, wearing %v; want shirtA in the wardrobe, not on", c.Cosmetics, c.Equipped)
	}
	// A second one is fine: items can be doubled up.
	if item, why := s.sell(c, 1, 450); item != "shirtA" || why != "" || copiesOf(c, "shirtA") != 2 {
		t.Errorf("buying it again: %q, %q, wardrobe %v; want a second shirtA", item, why, c.Cosmetics)
	}
}

// A sale asks Dabi to thank the buyer, unless the Shop tab switched that off,
// and the switch stays put through a restock and a restart.
func TestShopThanks(t *testing.T) {
	s, _, path := testSellingShop(t)
	ctx := context.Background()
	dabi := &fakeSender{}
	s.dabi = dabi
	if s.adminState()["thanks"] != true {
		t.Fatal("a new shop has Dabi's thank-yous off, want on")
	}

	s.thank(ctx, "Hez", "hez", "tophat", 2, 450)
	want := []map[string]any{{"user_name": "Hez", "user_login": "hez", "item": "tophat", "slot": "head", "copies": float64(2), "cost": float64(450)}}
	if got := dabi.take(shopThanksEvent); !reflect.DeepEqual(got, want) {
		t.Errorf("thanking Hez sent %v, want %v", got, want)
	}

	if err := s.setThanks(false); err != nil {
		t.Fatal(err)
	}
	if err := s.set(ctx, []shopBox{{Item: "shirtA", Price: 450}, {}, {}}); err != nil {
		t.Fatal(err)
	}
	s.thank(ctx, "Hez", "hez", "shirtA", 1, 450)
	if got := dabi.take(shopThanksEvent); len(got) != 0 {
		t.Errorf("with the thank-yous off, sent %v", got)
	}
	reloaded := newShopStore(path, newOverlayHub(), testCatalogStore(t, testCatalogJSON), nil)
	if reloaded.adminState()["thanks"] != false {
		t.Error("the thank-yous came back on after a restart")
	}

	if err := s.setThanks(true); err != nil {
		t.Fatal(err)
	}
	s.thank(ctx, "Hez", "hez", "shirtA", 1, 450)
	if got := dabi.take(shopThanksEvent); len(got) != 1 {
		t.Errorf("switched back on, sent %d thank-yous, want 1", len(got))
	}
}

// Only the broadcaster opens and closes the shop, and chat hears about it.
func TestShopCommands(t *testing.T) {
	s, f, _ := testSellingShop(t)
	ctx := context.Background()
	if err := s.set(ctx, []shopBox{{Item: "shirtA", Price: 450}, {}, {Item: "tophat", Price: 300}}); err != nil {
		t.Fatal(err)
	}
	from := func(chatter string) map[string]any {
		return map[string]any{"broadcaster_user_id": "100", "chatter_user_id": chatter, "chatter_user_login": "someone"}
	}
	said := func() string {
		chats := f.take("channel.command.send_chat")
		if len(chats) != 1 {
			t.Fatalf("sent %d chat messages, want 1", len(chats))
		}
		return chats[0]["message"].(string)
	}

	handleShopCommand(ctx, from("200"), "!openshop", s, f)
	if s.isOpen() || len(f.take("channel.command.sync_rewards"))+len(f.take("channel.command.send_chat")) != 0 {
		t.Fatal("a chatter who isn't the broadcaster opened the shop")
	}

	handleShopCommand(ctx, from("100"), "!openshop", s, f)
	if !s.isOpen() {
		t.Fatal("!openshop didn't open it")
	}
	rewardsSynced(t, f)
	if got, want := said(), "Dabi's shop is open! Box 1: shirtA, 450 points. Box 3: tophat, 300 points. Redeem Buy item 1 or Buy item 3 to buy."; got != want {
		t.Errorf("open message = %q, want %q", got, want)
	}

	handleShopCommand(ctx, from("100"), "!closeshop", s, f)
	if s.isOpen() {
		t.Fatal("!closeshop didn't close it")
	}
	rewardsSynced(t, f)
	if got := said(); got != "Dabi's shop is closed. Thanks for shopping!" {
		t.Errorf("close message = %q", got)
	}
}

func TestShopOpenMessage(t *testing.T) {
	s, _, _ := testSellingShop(t)
	ctx := context.Background()
	if got := s.openMessage(); got != "Dabi's shop is open, but its boxes are empty." {
		t.Errorf("empty shop: %q", got)
	}
	if err := s.set(ctx, []shopBox{{}, {Item: "tophat", Price: 300}, {}}); err != nil {
		t.Fatal(err)
	}
	if got := s.openMessage(); !strings.HasSuffix(got, "Redeem Buy item 2 to buy.") {
		t.Errorf("one box: %q", got)
	}
	if err := s.set(ctx, []shopBox{{Item: "shirtA", Price: 1}, {Item: "shirtB", Price: 2}, {Item: "tophat", Price: 3}}); err != nil {
		t.Fatal(err)
	}
	if got := s.openMessage(); !strings.HasSuffix(got, "Redeem Buy item 1, Buy item 2 or Buy item 3 to buy.") {
		t.Errorf("three boxes: %q", got)
	}
}

// !openshop sends Dabi to the stool and !closeshop sends him home; a chatter
// can't move him.
func TestShopWalksDabi(t *testing.T) {
	s, f, _ := testSellingShop(t)
	ctx := context.Background()
	dabi := &fakeSender{}
	s.dabi = dabi
	s.stool = &shopStool{X: 1682, Y: 859, Seat: 210}
	from := func(chatter string) map[string]any {
		return map[string]any{"broadcaster_user_id": "100", "chatter_user_id": chatter, "chatter_user_login": "someone"}
	}

	handleShopCommand(ctx, from("200"), "!openshop", s, f)
	if got := dabi.take(shopWalkEvent); len(got) != 0 {
		t.Errorf("a chatter's !openshop sent Dabi %v", got)
	}
	handleShopCommand(ctx, from("100"), "!openshop", s, f)
	if got, want := dabi.take(shopWalkEvent), []map[string]any{{"x": float64(1682), "y": float64(859), "seat": float64(210)}}; !reflect.DeepEqual(got, want) {
		t.Errorf("!openshop sent %v, want %v", got, want)
	}
	handleShopCommand(ctx, from("100"), "!closeshop", s, f)
	if got, want := dabi.take(shopWalkEvent), []map[string]any{{"home": true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("!closeshop sent %v, want %v", got, want)
	}

	s.stool = nil
	handleShopCommand(ctx, from("100"), "!openshop", s, f)
	if got := dabi.take(shopWalkEvent); len(got) != 0 {
		t.Errorf("with no stool spot, !openshop sent Dabi %v", got)
	}
}

// The stool's spot comes from the stall art's layout.json.
func TestLoadShopStool(t *testing.T) {
	if got, want := loadShopStool(filepath.Join("..", "overlay", "shop", "layout.json")), (&shopStool{X: 1682, Y: 859, Seat: 210}); !reflect.DeepEqual(got, want) {
		t.Errorf("the stall's stool = %+v, want %+v", got, want)
	}
	if got := loadShopStool(filepath.Join(t.TempDir(), "missing.json")); got != nil {
		t.Errorf("no layout.json gave %+v, want nil", got)
	}
	old := filepath.Join(t.TempDir(), "layout.json")
	if err := os.WriteFile(old, []byte(`{"size": [1920, 1080], "boxes": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadShopStool(old); got != nil {
		t.Errorf("a layout without the stool gave %+v, want nil", got)
	}
}
