package weixin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

func TestAPIClientsShareTransport(t *testing.T) {
	directA, err := NewApiClient("https://ilinkai.weixin.qq.com/", "token-a", "")
	if err != nil {
		t.Fatalf("NewApiClient() error = %v", err)
	}
	directB, err := NewApiClient("https://ilinkai.weixin.qq.com/", "token-b", "")
	if err != nil {
		t.Fatalf("NewApiClient() error = %v", err)
	}
	if directA.HttpClient.Transport != directB.HttpClient.Transport {
		t.Fatal("direct clients use different transports")
	}

	proxied, err := NewApiClient("https://ilinkai.weixin.qq.com/", "token-c", "http://127.0.0.1:7890")
	if err != nil {
		t.Fatalf("NewApiClient() proxy error = %v", err)
	}
	if proxied.HttpClient.Transport == directA.HttpClient.Transport {
		t.Fatal("proxy client reused the direct transport")
	}
}

func TestSharedPollerKeepsEveryBotInFlight(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":0,"errcode":0}`))
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		srv.Close()
	})

	newBot := func(token string) *WeixinChannel {
		dir := t.TempDir()
		return &WeixinChannel{
			api: &ApiClient{
				BaseURL:    srv.URL,
				Token:      token,
				HttpClient: srv.Client(),
			},
			config:            &config.WeixinSettings{},
			accountKey:        token,
			typingCache:       map[string]typingTicketCacheEntry{},
			syncBufPath:       filepath.Join(dir, "sync.json"),
			contextTokensPath: filepath.Join(dir, "tokens.json"),
		}
	}
	bots := []*WeixinChannel{newBot("token-default"), newBot("token-shop")}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSharedPoller(ctx, bots)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		ready := seen["Bearer token-default"] > 0 && seen["Bearer token-shop"] > 0
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("polls in flight = %#v", seen)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shared poller did not stop")
	}
}
