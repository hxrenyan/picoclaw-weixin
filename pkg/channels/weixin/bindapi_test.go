package weixin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

type stubQR struct {
	mu     sync.Mutex
	qr     *QRCodeResponse
	status *StatusResponse
	qrErr  error
}

func (s *stubQR) GetQRCode(context.Context, string) (*QRCodeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qr, s.qrErr
}

func (s *stubQR) GetQRCodeStatus(context.Context, string) (*StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, nil
}

func testBindConfig(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	_, settings, err := EnsureWeixinChannel(loaded, config.ChannelWeixin)
	if err != nil {
		t.Fatalf("EnsureWeixinChannel() error = %v", err)
	}
	settings.BindToken = *config.NewSecureString(token)
	if err := config.SaveConfig(path, loaded); err != nil {
		t.Fatalf("SaveConfig(token) error = %v", err)
	}
	return path
}

func TestBindAPIRequiresToken(t *testing.T) {
	path := testBindConfig(t, "")
	api := NewBindAPI(path, nil)
	api.newClient = func(string, string) (qrAPI, error) {
		t.Fatal("QR client should not be called")
		return nil, nil
	}

	req := httptest.NewRequest(http.MethodPost, "/api/weixin/binds", strings.NewReader(`{"name":"张三"}`))
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

func TestBindAPIRejectsWrongToken(t *testing.T) {
	path := testBindConfig(t, "bind-secret")
	api := NewBindAPI(path, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/weixin/binds", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong-secret")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestBindAPICreatesBotAndConfirmsWithoutReturningToken(t *testing.T) {
	path := testBindConfig(t, "bind-secret")
	stub := &stubQR{
		qr: &QRCodeResponse{Qrcode: "qr-token", QrcodeImgContent: "https://weixin.example/qr"},
		status: &StatusResponse{
			Status:      "confirmed",
			BotToken:    "secret-bot-token",
			IlinkBotID:  "bot-id-1",
			IlinkUserID: "user-id-1",
		},
	}
	var reloaded atomic.Int32
	api := NewBindAPI(path, func() { reloaded.Add(1) })
	api.newClient = func(string, string) (qrAPI, error) { return stub, nil }

	req := httptest.NewRequest(http.MethodPost, "/api/weixin/binds", strings.NewReader(`{"name":"张三","external_id":"user-42"}`))
	req.Header.Set("Authorization", "Bearer bind-secret")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var started bindResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if started.Status != bindStatusWait || started.Account != "bot-2" || started.ExternalID != "user-42" {
		t.Fatalf("start = %#v", started)
	}
	if !strings.HasPrefix(started.QRDataURI, "data:image/png;base64,") || started.QRPath == "" {
		t.Fatalf("missing QR in %#v", started)
	}
	if strings.Contains(rec.Body.String(), "secret-bot-token") {
		t.Fatal("start response leaked bot token")
	}

	pngReq := httptest.NewRequest(http.MethodGet, started.QRPath, nil)
	pngReq.Header.Set("Authorization", "Bearer bind-secret")
	pngRec := httptest.NewRecorder()
	api.ServeHTTP(pngRec, pngReq)
	if pngRec.Code != http.StatusOK || pngRec.Header().Get("Content-Type") != "image/png" || pngRec.Body.Len() == 0 {
		t.Fatalf("png status=%d type=%s len=%d", pngRec.Code, pngRec.Header().Get("Content-Type"), pngRec.Body.Len())
	}

	pollReq := httptest.NewRequest(http.MethodGet, bindPathPrefix+"/"+started.FlowID, nil)
	pollReq.Header.Set("Authorization", "Bearer bind-secret")
	pollRec := httptest.NewRecorder()
	api.ServeHTTP(pollRec, pollReq)
	if pollRec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, body=%s", pollRec.Code, pollRec.Body.String())
	}
	var polled bindResponse
	if err := json.Unmarshal(pollRec.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decode poll: %v", err)
	}
	if polled.Status != bindStatusConfirmed || polled.WeixinBotID != "bot-id-1" || polled.QRDataURI != "" {
		t.Fatalf("poll = %#v", polled)
	}
	if strings.Contains(pollRec.Body.String(), "secret-bot-token") {
		t.Fatal("poll response leaked bot token")
	}
	deadline := time.Now().Add(time.Second)
	for reloaded.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if reloaded.Load() != 1 {
		t.Fatalf("reload calls = %d, want 1", reloaded.Load())
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	settings, err := settingsFromChannel(loaded.Channels.Get(config.ChannelWeixin))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	spec := accountSpecByKey(settings, "bot-2")
	if !settings.IsolateAccounts {
		t.Fatal("外部绑定的用户必须自动开启渠道隔离")
	}
	if spec == nil || spec.Settings.Token.String() != "secret-bot-token" || spec.Name != "张三" {
		t.Fatalf("saved bot = %#v", spec)
	}
}

func TestBindAPIReleasesSlotWhenQRFails(t *testing.T) {
	path := testBindConfig(t, "bind-secret")
	api := NewBindAPI(path, nil)
	api.newClient = func(string, string) (qrAPI, error) {
		return &stubQR{qrErr: context.Canceled}, nil
	}
	req := httptest.NewRequest(http.MethodPost, "/api/weixin/binds", strings.NewReader(`{"name":"张三"}`))
	req.Header.Set("Authorization", "Bearer bind-secret")
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	settings, err := settingsFromChannel(loaded.Channels.Get(config.ChannelWeixin))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if _, ok := settings.Accounts["bot-2"]; ok {
		t.Fatal("failed QR kept an empty bot slot")
	}
}
