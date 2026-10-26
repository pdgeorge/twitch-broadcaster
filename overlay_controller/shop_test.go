package main

// Tests for Dabi's shop. What's in stock has to survive a restart, never offer
// something the catalog doesn't have, and reach /shop as soon as it changes.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testShop(t *testing.T) (*shopStore, string, *overlayHub) {
	t.Helper()
	path := filepath.Join(t.TempDir(), shopFile)
	hub := newOverlayHub()
	return newShopStore(path, hub, testCatalogStore(t, testCatalogJSON)), path, hub
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
	if err := s.set([]shopBox{{Item: "SHIRTA", Price: 450}, {}, {Item: "tophat", Price: 300}}); err != nil {
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
	again := newShopStore(path, newOverlayHub(), s.cosmetics)
	if got := again.boxes(); !reflect.DeepEqual(got, want) {
		t.Errorf("after a restart: %v, want %v", got, want)
	}
}

// An item keeps its price after it leaves the shop, so it comes back at it.
func TestShopRemembersPrices(t *testing.T) {
	s, _, _ := testShop(t)
	if err := s.set([]shopBox{{Item: "shirtA", Price: 450}, {}, {}}); err != nil {
		t.Fatal(err)
	}
	if err := s.set([]shopBox{{}, {Item: "shirtB", Price: 300}, {}}); err != nil {
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
			err := s.set(tc.boxes)
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
	if err := s.set([]shopBox{{Item: "shirtA", Price: 300}, {Item: "tophat", Price: 300}, {}}); err != nil {
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
	if got := newShopStore(path, newOverlayHub(), cosmetics).boxes(); !reflect.DeepEqual(got, make([]shopBox, shopBoxes)) {
		t.Errorf("broken shop.json gave %v, want an empty shop", got)
	}

	writeTestFile(t, path, []byte(`{"boxes": ["tophat"], "prices": {"tophat": 120}}`))
	want := []shopBox{{Item: "tophat", URL: "/hat.png", Price: 120}, {}, {}}
	if got := newShopStore(path, newOverlayHub(), cosmetics).boxes(); !reflect.DeepEqual(got, want) {
		t.Errorf("one-box shop.json gave %v, want %v", got, want)
	}
}

func TestShopStockEndpoint(t *testing.T) {
	s, _, _ := testShop(t)
	if err := s.set([]shopBox{{}, {Item: "shirtB", Price: 250}, {}}); err != nil {
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
}
