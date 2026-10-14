package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/sipeed/picoclaw/pkg/channels/weixin"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	weixinFlowTTL   = 5 * time.Minute
	weixinFlowGCAge = 30 * time.Minute
	weixinBaseURL   = "https://ilinkai.weixin.qq.com/"
	weixinBotType   = "3"
)

const (
	weixinStatusWait      = "wait"
	weixinStatusScanned   = "scaned"
	weixinStatusConfirmed = "confirmed"
	weixinStatusExpired   = "expired"
	weixinStatusError     = "error"
)

type weixinFlow struct {
	ID          string
	AccountKey  string // config account key, "default" or "bot-2"
	Qrcode      string // qrcode token from WeChat API (used for status polling)
	QRDataURI   string // base64 PNG data URI for display
	PollBaseURL string
	AccountID   string // IlinkBotID returned on confirmed
	Status      string // wait / scaned / confirmed / expired / error
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
}

type weixinFlowResponse struct {
	FlowID    string `json:"flow_id"`
	Status    string `json:"status"`
	QRDataURI string `json:"qr_data_uri,omitempty"`
	Account   string `json:"account,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// registerWeixinRoutes binds WeChat QR login endpoints to the ServeMux.
func (h *Handler) registerWeixinRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/weixin/accounts", h.handleCreateWeixinAccount)
	mux.HandleFunc("DELETE /api/weixin/accounts/{account}", h.handleDeleteWeixinAccount)
	mux.HandleFunc("POST /api/weixin/flows", h.handleStartWeixinFlow)
	mux.HandleFunc("GET /api/weixin/flows/{id}", h.handlePollWeixinFlow)
}

// handleStartWeixinFlow starts a new WeChat QR login flow.
//
//	POST /api/weixin/flows
func (h *Handler) handleStartWeixinFlow(w http.ResponseWriter, r *http.Request) {
	accountKey, err := h.weixinFlowAccount(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	api, err := weixin.NewApiClient(weixinBaseURL, "", "")
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create weixin client: %v", err), http.StatusInternalServerError)
		return
	}

	qrResp, err := api.GetQRCode(ctx, weixinBotType)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get QR code: %v", err), http.StatusInternalServerError)
		return
	}

	dataURI, err := generateQRDataURI(qrResp.QrcodeImgContent)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate QR image: %v", err), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	flow := &weixinFlow{
		ID:          newWeixinFlowID(),
		AccountKey:  accountKey,
		Qrcode:      qrResp.Qrcode,
		QRDataURI:   dataURI,
		PollBaseURL: weixinBaseURL,
		Status:      weixinStatusWait,
		CreatedAt:   now,
		UpdatedAt:   now,
		ExpiresAt:   now.Add(weixinFlowTTL),
	}
	h.storeWeixinFlow(flow)

	logger.InfoCF("weixin", "QR flow started", map[string]any{"flow_id": flow.ID})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(weixinFlowResponse{
		FlowID:    flow.ID,
		Status:    flow.Status,
		QRDataURI: flow.QRDataURI,
		Account:   flow.AccountKey,
	})
}

// handlePollWeixinFlow polls the WeChat API for QR code status and updates the flow.
//
//	GET /api/weixin/flows/{id}
func (h *Handler) handlePollWeixinFlow(w http.ResponseWriter, r *http.Request) {
	flowID := strings.TrimSpace(r.PathValue("id"))
	if flowID == "" {
		http.Error(w, "missing flow id", http.StatusBadRequest)
		return
	}

	flow, ok := h.getWeixinFlow(flowID)
	if !ok {
		http.Error(w, "flow not found", http.StatusNotFound)
		return
	}

	// Return terminal states directly without polling WeChat again
	if flow.Status == weixinStatusConfirmed ||
		flow.Status == weixinStatusExpired ||
		flow.Status == weixinStatusError {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(weixinFlowResponse{
			FlowID:    flow.ID,
			Account:   flow.AccountKey,
			Status:    flow.Status,
			AccountID: flow.AccountID,
			Error:     flow.Error,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pollBase := flow.PollBaseURL
	if pollBase == "" {
		pollBase = weixinBaseURL
	}
	api, err := weixin.NewApiClient(pollBase, "", "")
	if err != nil {
		h.setWeixinFlowError(flowID, fmt.Sprintf("client error: %v", err))
		flow, _ = h.getWeixinFlow(flowID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(weixinFlowResponse{FlowID: flow.ID, Status: flow.Status, Error: flow.Error})
		return
	}

	statusResp, err := api.GetQRCodeStatus(ctx, flow.Qrcode)
	if err != nil {
		// Transient error — keep current status, return it
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(weixinFlowResponse{
			FlowID:    flow.ID,
			Account:   flow.AccountKey,
			Status:    flow.Status,
			QRDataURI: flow.QRDataURI,
		})
		return
	}

	switch statusResp.Status {
	case weixinStatusWait:
		// no change

	case weixinStatusScanned:
		h.updateWeixinFlowStatus(flowID, weixinStatusScanned)

	case "scaned_but_redirect":
		if statusResp.RedirectHost != "" {
			h.setWeixinFlowPollBase(flowID, "https://"+statusResp.RedirectHost+"/")
		}
		h.updateWeixinFlowStatus(flowID, weixinStatusScanned)

	case weixinStatusConfirmed:
		if statusResp.BotToken == "" {
			h.setWeixinFlowError(flowID, "login confirmed but missing bot_token")
			break
		}
		baseURL := statusResp.Baseurl
		if baseURL == "" {
			baseURL = flow.PollBaseURL
		}
		if saveErr := h.saveWeixinBinding(flow.AccountKey, statusResp.BotToken, statusResp.IlinkBotID, statusResp.IlinkUserID, baseURL); saveErr != nil {
			h.setWeixinFlowError(flowID, fmt.Sprintf("failed to save token: %v", saveErr))
			logger.ErrorCF("weixin", "failed to save token", map[string]any{"error": saveErr.Error()})
			break
		}
		h.setWeixinFlowConfirmed(flowID, statusResp.IlinkBotID)
		logger.InfoCF("weixin", "QR login confirmed, token saved", map[string]any{
			"flow_id":    flowID,
			"account":    flow.AccountKey,
			"account_id": statusResp.IlinkBotID,
		})

	case weixinStatusExpired:
		h.updateWeixinFlowStatus(flowID, weixinStatusExpired)

	default:
		// unknown status, keep as-is
	}

	flow, _ = h.getWeixinFlow(flowID)
	w.Header().Set("Content-Type", "application/json")
	resp := weixinFlowResponse{
		FlowID:    flow.ID,
		Account:   flow.AccountKey,
		Status:    flow.Status,
		AccountID: flow.AccountID,
		Error:     flow.Error,
	}
	if flow.Status == weixinStatusWait || flow.Status == weixinStatusScanned {
		resp.QRDataURI = flow.QRDataURI
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// saveWeixinBinding writes one bot's token, enables the Weixin channel,
// and best-effort restarts the gateway when it is currently running.
func (h *Handler) saveWeixinBinding(accountKey, token, accountID, userID, baseURL string) error {
	if err := h.updateWeixinConfig(func(cfg *config.Config) error {
		_, settings, err := weixin.EnsureWeixinChannel(cfg, config.ChannelWeixin)
		if err != nil {
			return err
		}
		return weixin.UpsertCredentials(settings, accountKey, "", token, userID, accountID, baseURL, "")
	}); err != nil {
		return err
	}
	h.restartGatewayIfRunning("saving binding")
	return nil
}

func (h *Handler) updateWeixinConfig(update func(*config.Config) error) error {
	unlock, err := weixin.LockConfig(h.configPath)
	if err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer unlock()
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := update(cfg); err != nil {
		return err
	}
	if err := config.SaveConfig(h.configPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

func (h *Handler) handleCreateWeixinAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	var key string
	if err := h.updateWeixinConfig(func(cfg *config.Config) error {
		_, settings, err := weixin.EnsureWeixinChannel(cfg, config.ChannelWeixin)
		if err != nil {
			return err
		}
		key, err = weixin.EnsureAccount(settings, req.Name)
		return err
	}); err != nil {
		status := http.StatusInternalServerError
		if !strings.HasPrefix(err.Error(), "lock config:") &&
			!strings.HasPrefix(err.Error(), "load config:") &&
			!strings.HasPrefix(err.Error(), "save config:") {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"account": key,
		"name":    strings.TrimSpace(req.Name),
	})
}

func (h *Handler) handleDeleteWeixinAccount(w http.ResponseWriter, r *http.Request) {
	accountKey := strings.TrimSpace(r.PathValue("account"))
	if accountKey == "" {
		http.Error(w, "missing account", http.StatusBadRequest)
		return
	}
	if err := h.updateWeixinConfig(func(cfg *config.Config) error {
		bc := cfg.Channels.Get(config.ChannelWeixin)
		if bc == nil {
			return fmt.Errorf("weixin channel is not configured")
		}
		settings, err := decodeWeixinSettings(bc)
		if err != nil {
			return err
		}
		return weixin.RemoveAccount(settings, accountKey)
	}); err != nil {
		status := http.StatusInternalServerError
		if strings.HasPrefix(err.Error(), "lock config:") ||
			strings.HasPrefix(err.Error(), "load config:") ||
			strings.HasPrefix(err.Error(), "save config:") {
			status = http.StatusInternalServerError
		} else if strings.Contains(err.Error(), "not configured") {
			status = http.StatusNotFound
		} else {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	h.restartGatewayIfRunning("removing weixin account")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func decodeWeixinSettings(bc *config.Channel) (*config.WeixinSettings, error) {
	decoded, err := bc.GetDecoded()
	if err != nil {
		return nil, err
	}
	settings, ok := decoded.(*config.WeixinSettings)
	if !ok || settings == nil {
		return nil, fmt.Errorf("weixin settings are invalid")
	}
	return settings, nil
}

func (h *Handler) weixinFlowAccount(r *http.Request) (string, error) {
	var req struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("invalid json")
	}
	accountKey := strings.TrimSpace(req.Account)
	if accountKey == "" {
		return weixin.DefaultAccountKey, nil
	}
	if strings.EqualFold(accountKey, weixin.DefaultAccountKey) {
		return weixin.DefaultAccountKey, nil
	}
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	bc := cfg.Channels.Get(config.ChannelWeixin)
	if bc == nil {
		return "", fmt.Errorf("weixin account %q not found", accountKey)
	}
	settings, err := decodeWeixinSettings(bc)
	if err != nil {
		return "", err
	}
	specs, err := weixin.ListAccounts(settings)
	if err != nil {
		return "", err
	}
	for _, spec := range specs {
		if spec.Key == accountKey || strings.EqualFold(spec.Key, accountKey) {
			return spec.Key, nil
		}
	}
	return "", fmt.Errorf("weixin account %q not found", accountKey)
}

func (h *Handler) restartGatewayIfRunning(reason string) {
	status := h.gatewayStatusData()
	gatewayStatus, _ := status["gateway_status"].(string)
	if gatewayStatus != "running" {
		return
	}
	if _, err := h.RestartGateway(); err != nil {
		logger.ErrorCF("weixin", "failed to restart gateway", map[string]any{
			"reason": reason,
			"error":  err.Error(),
		})
	}
}

func (h *Handler) setWeixinFlowPollBase(flowID, baseURL string) {
	h.weixinMu.Lock()
	defer h.weixinMu.Unlock()
	if flow, ok := h.weixinFlows[flowID]; ok {
		flow.PollBaseURL = baseURL
		flow.UpdatedAt = time.Now()
	}
}

// generateQRDataURI encodes content as a QR code PNG and returns a data URI.
func generateQRDataURI(content string) (string, error) {
	code, err := qr.Encode(content, qr.L)
	if err != nil {
		return "", fmt.Errorf("qr encode: %w", err)
	}
	pngBytes := code.PNG()
	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	return "data:image/png;base64," + encoded, nil
}

func newWeixinFlowID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("wx_%d", time.Now().UnixNano())
	}
	return "wx_" + hex.EncodeToString(buf)
}

func (h *Handler) storeWeixinFlow(flow *weixinFlow) {
	h.weixinMu.Lock()
	defer h.weixinMu.Unlock()
	h.gcWeixinFlowsLocked(time.Now())
	h.weixinFlows[flow.ID] = flow
}

func (h *Handler) getWeixinFlow(flowID string) (*weixinFlow, bool) {
	h.weixinMu.Lock()
	defer h.weixinMu.Unlock()
	h.gcWeixinFlowsLocked(time.Now())
	flow, ok := h.weixinFlows[flowID]
	if !ok {
		return nil, false
	}
	cp := *flow
	return &cp, true
}

func (h *Handler) updateWeixinFlowStatus(flowID, status string) {
	h.weixinMu.Lock()
	defer h.weixinMu.Unlock()
	if flow, ok := h.weixinFlows[flowID]; ok {
		flow.Status = status
		flow.UpdatedAt = time.Now()
	}
}

func (h *Handler) setWeixinFlowConfirmed(flowID, accountID string) {
	h.weixinMu.Lock()
	defer h.weixinMu.Unlock()
	if flow, ok := h.weixinFlows[flowID]; ok {
		flow.Status = weixinStatusConfirmed
		flow.AccountID = accountID
		flow.UpdatedAt = time.Now()
	}
}

func (h *Handler) setWeixinFlowError(flowID, errMsg string) {
	h.weixinMu.Lock()
	defer h.weixinMu.Unlock()
	if flow, ok := h.weixinFlows[flowID]; ok {
		flow.Status = weixinStatusError
		flow.Error = errMsg
		flow.UpdatedAt = time.Now()
	}
}

func (h *Handler) gcWeixinFlowsLocked(now time.Time) {
	for id, flow := range h.weixinFlows {
		if flow.Status == weixinStatusWait || flow.Status == weixinStatusScanned {
			if !flow.ExpiresAt.IsZero() && now.After(flow.ExpiresAt) {
				flow.Status = weixinStatusExpired
				flow.UpdatedAt = now
			}
		}
		if flow.Status != weixinStatusWait &&
			flow.Status != weixinStatusScanned &&
			now.Sub(flow.UpdatedAt) > weixinFlowGCAge {
			delete(h.weixinFlows, id)
		}
	}
}
