package sources

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tokens/internal/testkit"
)

func TestCoinGeckoPlatformsKeepsServedChains(t *testing.T) {
	body := testkit.Read(t, "coingecko/coins_list.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/coins/list" || r.URL.Query().Get("include_platform") != "true" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	got, err := NewCoinGecko(srv.URL, "", time.Millisecond).WithHTTPClient(srv.Client()).Platforms(t.Context(), "ethereum", "monad")
	if err != nil {
		t.Fatal(err)
	}
	usdc := got["usd-coin"]
	if len(usdc) != 2 || usdc["ethereum"] != "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48" || usdc["monad"] != "0x754704bc059f8c67012fed69bc8a327a5aafb603" {
		t.Errorf("usd-coin: %v", usdc)
	}
	if m := got["wrapped-monad"]; len(m) != 1 || m["monad"] == "" {
		t.Errorf("wrapped-monad: %v", m)
	}
	// an empty address is no address
	if _, ok := got["cuberium"]; ok {
		t.Error("cuberium has an empty Ethereum address")
	}
}

func TestMarketSourcesPauseAfter429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cg := NewCoinGecko(srv.URL, "", time.Millisecond).WithHTTPClient(srv.Client())
	if _, err := cg.Markets(t.Context()); err == nil {
		t.Fatal("429 was no error")
	}
	if d := cg.pacer.PausedFor(); d < 80*time.Second {
		t.Errorf("paused for %s", d)
	}
	if _, err := cg.Markets(t.Context()); err == nil {
		t.Error("a paused source was asked again")
	}
}
