package weixin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"rsc.io/qr"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	bindFlowTTL       = 5 * time.Minute
	bindFlowGCAge     = 30 * time.Minute
	bindBotType       = "3"
	bindPathPrefix    = "/api/weixin/binds"
	maxExternalIDRune = 128
)

const (
	bindStatusWait      = "wait"
	bindStatusScanned   = "scanned"
	bindStatusConfirmed = "confirmed"
	bindStatusExpired   = "expired"
	bindStatusError     = "error"
)

type qrAPI interface {
	GetQRCode(ctx context.Context, botType string) (*QRCodeResponse, error)
	GetQRCodeStatus(ctx context.Context, qrcode string) (*StatusResponse, error)
}

type handlerMux interface {
	Handle(pattern string, handler http.Handler)
}

// BindAPI serves WeChat login QR codes to another service.
// That service displays the image; PicoClaw keeps the token after the scan.
type BindAPI struct {
	configPath string
	reload     func()
	newClient  func(baseURL, proxy string) (qrAPI, error)

	mu       sync.Mutex
	configMu sync.Mutex
	flows    map[string]*bindFlow
}

type bindFlow struct {
	ID          string
	Channel     string
	AccountKey  string
	Name        string
	ExternalID  string
	CreatedSlot bool
	Qrcode      string
	QRDataURI   string
	PNG         []byte
	PollBaseURL string
	Proxy       string
	WeixinBotID string
	Status      string
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
	released    bool
}

type bindResponse struct {
	FlowID      string `json:"flow_id"`
	Channel     string `json:"channel"`
	Account     string `json:"account"`
	Name        string `json:"name,omitempty"`
	ExternalID  string `json:"external_id,omitempty"`
	Status      string `json:"status"`
	WeixinBotID string `json:"weixin_bot_id,omitempty"`
	QRDataURI   string `json:"qr_data_uri,omitempty"`
	QRPath      string `json:"qr_path,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
	Error       string `json:"error,omitempty"`
}

type bindStartRequest struct {
	Channel    string `json:"channel"`
	Account    string `json:"account"`
	Name       string `json:"name"`
	ExternalID string `json:"external_id"`
}

// RegisterBindAPI mounts the WeChat bind API on the gateway HTTP server.
func RegisterBindAPI(mux handlerMux, configPath string, reload func()) *BindAPI {
	api := NewBindAPI(configPath, reload)
	api.Register(mux)
	return api
}

// NewBindAPI builds a bind API without mounting it.
func NewBindAPI(configPath string, reload func()) *BindAPI {
	return &BindAPI{
		configPath: configPath,
		reload:     reload,
		flows:      map[string]*bindFlow{},
	}
}

// Register mounts POST /api/weixin/binds and the status/image paths under it.
func (a *BindAPI) Register(mux handlerMux) {
	if mux == nil || a == nil {
		return
	}
	mux.Handle(bindPathPrefix, a)
	mux.Handle(bindPathPrefix+"/", a)
}

func (a *BindAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, bindPathPrefix), "/")
	if rest == "" {
		if r.Method != http.MethodPost {
			writeBindError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.start(w, r)
		return
	}

	channelName := bindChannelName(r, "")
	if !a.authorize(w, r, channelName) {
		return
	}
	parts := strings.Split(rest, "/")
	if r.Method != http.MethodGet {
		writeBindError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(parts) == 1 {
		a.poll(w, r, channelName, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "qr.png" {
		a.png(w, r, channelName, parts[0])
		return
	}
	http.NotFound(w, r)
}

func (a *BindAPI) authorize(w http.ResponseWriter, r *http.Request, channelName string) bool {
	token, err := a.bindToken(channelName)
	if err != nil {
		writeBindError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if token == "" {
		writeBindError(w, http.StatusServiceUnavailable, "weixin bind token is not configured")
		return false
	}
	if !bindAuthorized(token, bearerToken(r.Header.Get("Authorization"))) {
		writeBindError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (a *BindAPI) start(w http.ResponseWriter, r *http.Request) {
	var req bindStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeBindError(w, http.StatusBadRequest, "invalid json")
		return
	}
	channelName := bindChannelName(r, req.Channel)
	if !a.authorize(w, r, channelName) {
		return
	}
	externalID := strings.TrimSpace(req.ExternalID)
	if externalID != "" && utf8.RuneCountInString(externalID) > maxExternalIDRune {
		writeBindError(w, http.StatusBadRequest, "external_id is too long")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" && externalID != "" && utf8.RuneCountInString(externalID) <= maxAccountName {
		name = externalID
	}
	if name == "" {
		name = "微信用户"
	}
	if err := validateAccountName(name); err != nil {
		writeBindError(w, http.StatusBadRequest, err.Error())
		return
	}

	accountKey, created, baseURL, proxy, err := a.reserveAccount(channelName, req.Account, name)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "at most") {
			status = http.StatusConflict
		}
		writeBindError(w, status, err.Error())
		return
	}

	client, err := a.client(baseURL, proxy)
	if err != nil {
		if created {
			a.releaseAccount(channelName, accountKey)
		}
		writeBindError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	qrResp, err := client.GetQRCode(ctx, bindBotType)
	if err != nil || qrResp == nil || strings.TrimSpace(qrResp.Qrcode) == "" || strings.TrimSpace(qrResp.QrcodeImgContent) == "" {
		if created {
			a.releaseAccount(channelName, accountKey)
		}
		if err == nil {
			err = fmt.Errorf("weixin QR response was empty")
		}
		writeBindError(w, http.StatusBadGateway, err.Error())
		return
	}
	png, dataURI, err := encodeBindQR(qrResp.QrcodeImgContent)
	if err != nil {
		if created {
			a.releaseAccount(channelName, accountKey)
		}
		writeBindError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now()
	flow := &bindFlow{
		ID:          newBindFlowID(),
		Channel:     channelName,
		AccountKey:  accountKey,
		Name:        name,
		ExternalID:  externalID,
		CreatedSlot: created,
		Qrcode:      qrResp.Qrcode,
		QRDataURI:   dataURI,
		PNG:         png,
		PollBaseURL: baseURLOrDefault(baseURL),
		Proxy:       proxy,
		Status:      bindStatusWait,
		CreatedAt:   now,
		UpdatedAt:   now,
		ExpiresAt:   now.Add(bindFlowTTL),
	}
	a.storeFlow(flow)
	logger.InfoCF("weixin", "bind QR created", map[string]any{
		"flow_id": flow.ID,
		"channel": flow.Channel,
		"account": flow.AccountKey,
	})
	writeBindJSON(w, http.StatusOK, a.response(flow, true))
}

func (a *BindAPI) poll(w http.ResponseWriter, r *http.Request, channelName, flowID string) {
	flow := a.copyFlow(channelName, flowID)
	if flow == nil {
		writeBindError(w, http.StatusNotFound, "flow not found")
		return
	}
	if terminalBindStatus(flow.Status) {
		writeBindJSON(w, http.StatusOK, a.response(flow, false))
		return
	}
	if !flow.ExpiresAt.IsZero() && time.Now().After(flow.ExpiresAt) {
		a.finishExpired(flow)
		flow = a.copyFlow(channelName, flowID)
		writeBindJSON(w, http.StatusOK, a.response(flow, false))
		return
	}

	client, err := a.client(flow.PollBaseURL, flow.Proxy)
	if err != nil {
		a.failFlow(flow.ID, err.Error())
		flow = a.copyFlow(channelName, flowID)
		writeBindJSON(w, http.StatusOK, a.response(flow, false))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	statusResp, err := client.GetQRCodeStatus(ctx, flow.Qrcode)
	if err != nil || statusResp == nil {
		writeBindJSON(w, http.StatusOK, a.response(flow, true))
		return
	}

	switch statusResp.Status {
	case "wait":
	case "scaned", "scanned":
		a.updateFlowStatus(flow.ID, bindStatusScanned)
	case "scaned_but_redirect":
		if statusResp.RedirectHost != "" {
			a.setPollBase(flow.ID, "https://"+statusResp.RedirectHost+"/")
		}
		a.updateFlowStatus(flow.ID, bindStatusScanned)
	case "confirmed":
		a.confirmFlow(flow, statusResp)
	case "expired":
		a.finishExpired(flow)
	default:
	}

	flow = a.copyFlow(channelName, flowID)
	writeBindJSON(w, http.StatusOK, a.response(flow, flow.Status == bindStatusWait || flow.Status == bindStatusScanned))
}

func (a *BindAPI) png(w http.ResponseWriter, r *http.Request, channelName, flowID string) {
	flow := a.copyFlow(channelName, flowID)
	if flow == nil || len(flow.PNG) == 0 || terminalBindStatus(flow.Status) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(flow.PNG)
}

func (a *BindAPI) confirmFlow(flow *bindFlow, statusResp *StatusResponse) {
	if strings.TrimSpace(statusResp.BotToken) == "" {
		a.failFlow(flow.ID, "login confirmed but missing bot_token")
		return
	}
	baseURL := statusResp.Baseurl
	if baseURL == "" {
		baseURL = flow.PollBaseURL
	}
	if err := a.saveBinding(flow.Channel, flow.AccountKey, flow.Name, statusResp.BotToken, statusResp.IlinkUserID, statusResp.IlinkBotID, baseURL); err != nil {
		a.failFlow(flow.ID, err.Error())
		logger.ErrorCF("weixin", "failed to save bind result", map[string]any{
			"flow_id": flow.ID,
			"account": flow.AccountKey,
			"error":   err.Error(),
		})
		return
	}
	a.mu.Lock()
	if current, ok := a.flows[flow.ID]; ok {
		current.Status = bindStatusConfirmed
		current.WeixinBotID = statusResp.IlinkBotID
		current.CreatedSlot = false
		current.UpdatedAt = time.Now()
	}
	a.mu.Unlock()
	logger.InfoCF("weixin", "WeChat bind confirmed", map[string]any{
		"flow_id":       flow.ID,
		"channel":       flow.Channel,
		"account":       flow.AccountKey,
		"weixin_bot_id": statusResp.IlinkBotID,
	})
	if a.reload != nil {
		go a.reload()
	}
}

func (a *BindAPI) saveBinding(channelName, accountKey, name, token, userID, botID, baseURL string) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	cfg, err := config.LoadConfig(a.configPath)
	if err != nil {
		return err
	}
	_, settings, err := EnsureWeixinChannel(cfg, channelName)
	if err != nil {
		return err
	}
	if err := UpsertCredentials(settings, accountKey, name, token, userID, botID, baseURL, ""); err != nil {
		return err
	}
	return config.SaveConfig(a.configPath, cfg)
}

func (a *BindAPI) reserveAccount(channelName, account, name string) (string, bool, string, string, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	cfg, err := config.LoadConfig(a.configPath)
	if err != nil {
		return "", false, "", "", err
	}
	_, settings, err := EnsureWeixinChannel(cfg, channelName)
	if err != nil {
		return "", false, "", "", err
	}
	key, created, err := reserveBindAccount(settings, account, name)
	if err != nil {
		return "", false, "", "", err
	}
	if err := config.SaveConfig(a.configPath, cfg); err != nil {
		return "", false, "", "", err
	}
	return key, created, settings.BaseURL, settings.Proxy, nil
}

func reserveBindAccount(settings *config.WeixinSettings, account, name string) (string, bool, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		key, err := EnsureAccount(settings, name)
		return key, err == nil, err
	}
	key, err := canonicalAccountKey(account)
	if err != nil {
		return "", false, err
	}
	specs, err := ListAccounts(settings)
	if err != nil {
		return "", false, err
	}
	for _, spec := range specs {
		if spec.Key == key {
			if name != "" {
				if err := UpsertCredentials(settings, key, name, "", "", "", "", ""); err != nil {
					return "", false, err
				}
			}
			return key, false, nil
		}
	}
	if len(specs) >= MaxAccounts {
		return "", false, fmt.Errorf("at most %d WeChat bots are supported", MaxAccounts)
	}
	enabled := true
	putAccount(settings, key, config.WeixinAccountSettings{
		Name:    name,
		Enabled: &enabled,
	})
	return key, true, nil
}

func (a *BindAPI) releaseAccount(channelName, accountKey string) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	cfg, err := config.LoadConfig(a.configPath)
	if err != nil {
		return
	}
	bc := cfg.Channels.Get(channelName)
	if bc == nil {
		return
	}
	settings, err := settingsFromChannel(bc)
	if err != nil {
		return
	}
	spec := accountSpecByKey(settings, accountKey)
	if spec == nil || strings.TrimSpace(spec.Settings.Token.String()) != "" {
		return
	}
	if accountKey == DefaultAccountKey {
		return
	}
	if err := RemoveAccount(settings, accountKey); err != nil {
		return
	}
	_ = config.SaveConfig(a.configPath, cfg)
}

func (a *BindAPI) finishExpired(flow *bindFlow) {
	a.mu.Lock()
	current, ok := a.flows[flow.ID]
	if ok && current.Status != bindStatusConfirmed {
		current.Status = bindStatusExpired
		current.UpdatedAt = time.Now()
	}
	release := ok && current.CreatedSlot && current.Status == bindStatusExpired
	if release {
		current.released = true
		current.CreatedSlot = false
	}
	channelName, accountKey := "", ""
	if release {
		channelName = current.Channel
		accountKey = current.AccountKey
	}
	a.mu.Unlock()
	if release {
		a.releaseAccount(channelName, accountKey)
	}
}

func accountSpecByKey(settings *config.WeixinSettings, accountKey string) *AccountSpec {
	specs, err := ListAccounts(settings)
	if err != nil {
		return nil
	}
	for i := range specs {
		if specs[i].Key == accountKey {
			return &specs[i]
		}
	}
	return nil
}

func (a *BindAPI) bindToken(channelName string) (string, error) {
	cfg, err := config.LoadConfig(a.configPath)
	if err != nil {
		return "", err
	}
	bc := cfg.Channels.Get(channelName)
	if bc == nil {
		return "", nil
	}
	if bc.Type != "" && bc.Type != config.ChannelWeixin {
		return "", fmt.Errorf("channel %q is not weixin", channelName)
	}
	settings, err := settingsFromChannel(bc)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(settings.BindToken.String()), nil
}

func (a *BindAPI) client(baseURL, proxy string) (qrAPI, error) {
	if a.newClient != nil {
		return a.newClient(baseURL, proxy)
	}
	return NewApiClient(baseURLOrDefault(baseURL), "", proxy)
}

func (a *BindAPI) storeFlow(flow *bindFlow) {
	a.mu.Lock()
	release := a.gcFlowsLocked(time.Now())
	a.flows[flow.ID] = flow
	a.mu.Unlock()
	a.releaseExpired(release)
}

func (a *BindAPI) copyFlow(channelName, flowID string) *bindFlow {
	a.mu.Lock()
	release := a.gcFlowsLocked(time.Now())
	flow, ok := a.flows[flowID]
	var cp *bindFlow
	if ok && flow.Channel == channelName {
		copied := *flow
		if flow.PNG != nil {
			copied.PNG = append([]byte(nil), flow.PNG...)
		}
		cp = &copied
	}
	a.mu.Unlock()
	a.releaseExpired(release)
	return cp
}

func (a *BindAPI) updateFlowStatus(flowID, status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if flow, ok := a.flows[flowID]; ok && !terminalBindStatus(flow.Status) {
		flow.Status = status
		flow.UpdatedAt = time.Now()
	}
}

func (a *BindAPI) setPollBase(flowID, baseURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if flow, ok := a.flows[flowID]; ok {
		flow.PollBaseURL = baseURL
		flow.UpdatedAt = time.Now()
	}
}

func (a *BindAPI) failFlow(flowID, message string) {
	a.mu.Lock()
	current, ok := a.flows[flowID]
	if ok && !terminalBindStatus(current.Status) {
		current.Status = bindStatusError
		current.Error = message
		current.UpdatedAt = time.Now()
	}
	release := ok && current.CreatedSlot
	if release {
		current.CreatedSlot = false
		current.released = true
	}
	channelName, accountKey := "", ""
	if release {
		channelName = current.Channel
		accountKey = current.AccountKey
	}
	a.mu.Unlock()
	if release {
		a.releaseAccount(channelName, accountKey)
	}
}

func (a *BindAPI) gcFlowsLocked(now time.Time) [][2]string {
	var release [][2]string
	for id, flow := range a.flows {
		if (flow.Status == bindStatusWait || flow.Status == bindStatusScanned) &&
			!flow.ExpiresAt.IsZero() && now.After(flow.ExpiresAt) {
			flow.Status = bindStatusExpired
			flow.UpdatedAt = now
			if flow.CreatedSlot && !flow.released {
				flow.released = true
				flow.CreatedSlot = false
				release = append(release, [2]string{flow.Channel, flow.AccountKey})
			}
		}
		if terminalBindStatus(flow.Status) && now.Sub(flow.UpdatedAt) > bindFlowGCAge {
			delete(a.flows, id)
		}
	}
	return release
}

func (a *BindAPI) releaseExpired(slots [][2]string) {
	for _, slot := range slots {
		a.releaseAccount(slot[0], slot[1])
	}
}

func (a *BindAPI) response(flow *bindFlow, includeQR bool) bindResponse {
	if flow == nil {
		return bindResponse{Status: bindStatusError, Error: "flow not found"}
	}
	resp := bindResponse{
		FlowID:      flow.ID,
		Channel:     flow.Channel,
		Account:     flow.AccountKey,
		Name:        flow.Name,
		ExternalID:  flow.ExternalID,
		Status:      flow.Status,
		WeixinBotID: flow.WeixinBotID,
		Error:       flow.Error,
	}
	if !flow.ExpiresAt.IsZero() && (flow.Status == bindStatusWait || flow.Status == bindStatusScanned) {
		resp.ExpiresAt = flow.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if includeQR && (flow.Status == bindStatusWait || flow.Status == bindStatusScanned) {
		resp.QRDataURI = flow.QRDataURI
		resp.QRPath = bindPathPrefix + "/" + flow.ID + "/qr.png"
	}
	return resp
}

func terminalBindStatus(status string) bool {
	switch status {
	case bindStatusConfirmed, bindStatusExpired, bindStatusError:
		return true
	default:
		return false
	}
}

func bindChannelName(r *http.Request, explicit string) string {
	name := strings.TrimSpace(explicit)
	if name == "" && r != nil {
		name = strings.TrimSpace(r.URL.Query().Get("channel"))
	}
	if name == "" {
		return config.ChannelWeixin
	}
	return name
}

func bindAuthorized(want, given string) bool {
	if want == "" || given == "" || len(want) != len(given) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(given)) == 1
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func baseURLOrDefault(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return defaultBaseURL
	}
	return baseURL
}

func encodeBindQR(content string) ([]byte, string, error) {
	code, err := qr.Encode(content, qr.L)
	if err != nil {
		return nil, "", err
	}
	png := code.PNG()
	return png, "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

func newBindFlowID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("wx_%d", time.Now().UnixNano())
	}
	return "wx_" + hex.EncodeToString(buf)
}

func writeBindJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeBindError(w http.ResponseWriter, status int, message string) {
	writeBindJSON(w, status, map[string]string{"error": message})
}
