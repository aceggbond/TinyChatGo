package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"tinychatgo/internal/clawbot"
)

const clawBotMaxHistory = 200

type clawBotManager struct {
	mu          sync.RWMutex
	client      *clawbot.Client
	logger      *log.Logger
	store       ClawBotPersistence
	persistence Persistence
	bindings    map[string]*ClawBotBinding
	cancel      map[string]context.CancelFunc
}

type clawBotPublicState struct {
	Status         string           `json:"status"`
	QRCodeURL      string           `json:"qrCodeUrl,omitempty"`
	QRExpiresAt    time.Time        `json:"qrExpiresAt,omitempty"`
	ForwardEnabled bool             `json:"forwardEnabled"`
	BoundAt        time.Time        `json:"boundAt,omitempty"`
	LastMessageAt  time.Time        `json:"lastMessageAt,omitempty"`
	LastError      string           `json:"lastError,omitempty"`
	Messages       []ClawBotMessage `json:"messages"`
}

func newClawBotManager(logger *log.Logger) *clawBotManager {
	return &clawBotManager{client: &clawbot.Client{}, logger: logger, bindings: make(map[string]*ClawBotBinding), cancel: make(map[string]context.CancelFunc)}
}

func (m *clawBotManager) setPersistence(persistence Persistence) error {
	m.mu.Lock()
	for _, cancel := range m.cancel {
		cancel()
	}
	m.cancel = make(map[string]context.CancelFunc)
	m.bindings = make(map[string]*ClawBotBinding)
	m.persistence = persistence
	store, ok := persistence.(ClawBotPersistence)
	if !ok || store == nil {
		m.store = nil
		m.mu.Unlock()
		return nil
	}
	m.store = store
	items, err := store.LoadClawBotBindings()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	var start []string
	for index := range items {
		binding := items[index]
		if binding.AccountID == "" {
			continue
		}
		copy := binding
		m.bindings[binding.AccountID] = &copy
		if binding.Status == "bound" && binding.BotToken != "" {
			start = append(start, binding.AccountID)
		}
	}
	m.mu.Unlock()
	for _, accountID := range start {
		m.startMonitor(accountID)
	}
	return nil
}

func (m *clawBotManager) publicState(accountID string) clawBotPublicState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	binding := m.bindings[accountID]
	if binding == nil {
		return clawBotPublicState{Status: "unbound", Messages: []ClawBotMessage{}}
	}
	status, qrURL := binding.Status, ""
	if status == "waiting" && !binding.QRExpiresAt.IsZero() && time.Now().After(binding.QRExpiresAt) {
		status = "unbound"
	}
	if status == "waiting" && binding.QRCodeURL != "" {
		// qrcode_img_content is QR payload text, not an image URL. Expose a
		// same-origin PNG endpoint so desktop WebView and browsers never depend
		// on a temporary remote image address.
		qrURL = "/__hfs/clawbot/qr-image?v=" + strconv.FormatInt(binding.UpdatedAt.UnixNano(), 10)
	}
	messages := append([]ClawBotMessage(nil), binding.Messages...)
	for index := range messages {
		if (messages[index].Kind == "image" || messages[index].Kind == "file") && messages[index].FileURL == "" {
			messages[index].Kind = "text"
			if messages[index].Mine {
				messages[index].Text = "[历史附件已发送，但旧版本没有保存本地预览]"
			} else {
				messages[index].Text = "[历史微信附件接收失败，请在微信中重新发送]"
			}
		}
	}
	return clawBotPublicState{Status: status, QRCodeURL: qrURL, QRExpiresAt: binding.QRExpiresAt, ForwardEnabled: binding.ForwardEnabled, BoundAt: binding.BoundAt, LastMessageAt: binding.LastMessageAt, LastError: binding.LastError, Messages: messages}
}

func (m *clawBotManager) qrImage(accountID string) ([]byte, error) {
	m.mu.RLock()
	binding := m.bindings[accountID]
	if binding == nil || binding.Status != "waiting" || binding.QRCodeURL == "" || (!binding.QRExpiresAt.IsZero() && time.Now().After(binding.QRExpiresAt)) {
		m.mu.RUnlock()
		return nil, errors.New("微信绑定二维码已过期")
	}
	content := binding.QRCodeURL
	m.mu.RUnlock()
	return qrcode.Encode(content, qrcode.Medium, 320)
}

func (m *clawBotManager) saveLocked(binding *ClawBotBinding) error {
	binding.UpdatedAt = time.Now().UTC()
	if len(binding.Messages) > clawBotMaxHistory {
		binding.Messages = append([]ClawBotMessage(nil), binding.Messages[len(binding.Messages)-clawBotMaxHistory:]...)
	}
	if m.store == nil {
		return nil
	}
	return m.store.SaveClawBotBinding(*binding)
}

func (m *clawBotManager) startQR(accountID string) (clawBotPublicState, error) {
	qr, err := m.client.StartQR(context.Background())
	if err != nil {
		return clawBotPublicState{}, err
	}
	m.mu.Lock()
	if cancel := m.cancel[accountID]; cancel != nil {
		cancel()
		delete(m.cancel, accountID)
	}
	binding := m.bindings[accountID]
	if binding == nil {
		binding = &ClawBotBinding{AccountID: accountID}
		m.bindings[accountID] = binding
	}
	binding.Status, binding.QRCode, binding.QRCodeURL = "waiting", qr.Code, qr.URL
	binding.QRExpiresAt, binding.LastError = time.Now().UTC().Add(5*time.Minute), ""
	err = m.saveLocked(binding)
	m.mu.Unlock()
	if err != nil {
		return clawBotPublicState{}, err
	}
	go m.pollQR(accountID, qr.Code)
	return m.publicState(accountID), nil
}

func (m *clawBotManager) pollQR(accountID, code string) {
	baseURL := clawbot.DefaultBaseURL
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		status, err := m.client.PollQR(context.Background(), baseURL, code)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if status.RedirectHost != "" {
			baseURL = "https://" + strings.TrimPrefix(status.RedirectHost, "https://")
		}
		if status.Status == "wait" || status.Status == "scaned" || status.Status == "scaned_but_redirect" {
			continue
		}
		if status.Status == "confirmed" && status.BotToken != "" && status.BotID != "" {
			m.mu.Lock()
			binding := m.bindings[accountID]
			if binding == nil || binding.QRCode != code {
				m.mu.Unlock()
				return
			}
			binding.Status, binding.BotToken, binding.BotID, binding.WeixinUserID = "bound", status.BotToken, status.BotID, status.WeixinUserID
			binding.BaseURL = strings.TrimSpace(status.BaseURL)
			if binding.BaseURL == "" {
				binding.BaseURL = baseURL
			}
			binding.QRCode, binding.QRCodeURL, binding.LastError = "", "", ""
			binding.BoundAt, binding.QRExpiresAt = time.Now().UTC(), time.Time{}
			_ = m.saveLocked(binding)
			m.mu.Unlock()
			m.startMonitor(accountID)
			return
		}
		m.setQRError(accountID, code, "微信绑定未完成："+status.Status)
		return
	}
	m.setQRError(accountID, code, "绑定二维码已过期，正在刷新")
}

func (m *clawBotManager) setQRError(accountID, code, message string) {
	m.mu.Lock()
	if binding := m.bindings[accountID]; binding != nil && binding.QRCode == code {
		binding.Status, binding.QRCode, binding.QRCodeURL = "unbound", "", ""
		binding.QRExpiresAt, binding.LastError = time.Time{}, message
		_ = m.saveLocked(binding)
	}
	m.mu.Unlock()
}

func (m *clawBotManager) startMonitor(accountID string) {
	m.mu.Lock()
	if old := m.cancel[accountID]; old != nil {
		old()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel[accountID] = cancel
	m.mu.Unlock()
	go m.monitor(ctx, accountID)
}

func (m *clawBotManager) monitor(ctx context.Context, accountID string) {
	backoff := time.Second
	for ctx.Err() == nil {
		m.mu.RLock()
		source := m.bindings[accountID]
		if source == nil || source.Status != "bound" {
			m.mu.RUnlock()
			return
		}
		binding := *source
		m.mu.RUnlock()
		updates, err := m.client.GetUpdates(ctx, clawbot.Credentials{BaseURL: binding.BaseURL, Token: binding.BotToken}, binding.UpdatesBuffer)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.setError(accountID, err.Error(), "bound")
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		m.mu.Lock()
		current := m.bindings[accountID]
		if current == nil || current.BotToken != binding.BotToken {
			m.mu.Unlock()
			return
		}
		if updates.Buffer != "" {
			current.UpdatesBuffer = updates.Buffer
		}
		for _, incoming := range updates.Messages {
			updateClawBotReplyRoute(current, incoming)
			if incoming.MessageType != 1 {
				continue
			}
			for itemIndex, item := range incoming.Items {
				message := ClawBotMessage{ID: fmt.Sprintf("wx-%d-%d-%s", incoming.MessageID, itemIndex, item.MessageID), SentAt: time.UnixMilli(incoming.CreateTime).UTC()}
				if incoming.CreateTime == 0 {
					message.SentAt = time.Now().UTC()
				}
				switch item.Type {
				case 1:
					message.Kind = "text"
					if item.Text != nil {
						message.Text = item.Text.Text
					}
				case 2:
					message.Kind, message.Text, message.FileName, message.MIME = "image", "[微信图片]", "微信图片.jpg", "image/jpeg"
					if item.Image != nil {
						media := item.Image.Media
						if media.AESKey == "" {
							media.AESKey = item.Image.AESKey
						}
						thumb := item.Image.ThumbMedia
						if thumb.AESKey == "" {
							thumb.AESKey = item.Image.AESKey
						}
						direct := clawbot.Media{FullURL: item.Image.URL}
						m.cacheIncomingMedia(ctx, accountID, &message, media, thumb, direct)
					}
				case 4:
					message.Kind, message.Text = "file", "[微信文件]"
					if item.File != nil {
						message.FileName = item.File.FileName
						message.MIME = "application/octet-stream"
						m.cacheIncomingMedia(ctx, accountID, &message, item.File.Media)
					}
				default:
					continue
				}
				if !clawMessageExists(current.Messages, message.ID) {
					current.Messages = append(current.Messages, message)
					current.LastMessageAt = message.SentAt
				}
			}
		}
		current.LastError = ""
		_ = m.saveLocked(current)
		m.mu.Unlock()
	}
}

// updateClawBotReplyRoute learns the actual Weixin peer from delivered
// messages. A small number of accounts receive a provisional ilink_user_id at
// QR binding time; sendmessage may return ret=0 for that stale ID while Weixin
// silently delivers nothing. The sender and context token from getupdates are
// the authoritative route for replies and must be persisted together.
func updateClawBotReplyRoute(binding *ClawBotBinding, incoming clawbot.Message) {
	if binding == nil {
		return
	}
	if peer := strings.TrimSpace(incoming.FromUserID); peer != "" && peer != binding.BotID {
		binding.WeixinUserID = peer
	}
	if token := strings.TrimSpace(incoming.ContextToken); token != "" {
		binding.ContextToken = token
	}
}

func (m *clawBotManager) cacheIncomingMedia(ctx context.Context, accountID string, message *ClawBotMessage, candidates ...clawbot.Media) {
	if message == nil || m.persistence == nil {
		if message != nil {
			message.Kind, message.Text = "text", message.Text+"（服务器未配置附件存储）"
		}
		return
	}
	var data []byte
	var err error
	for _, media := range candidates {
		if strings.TrimSpace(media.FullURL) == "" && strings.TrimSpace(media.EncryptQueryParam) == "" {
			continue
		}
		for attempt := 0; attempt < 3; attempt++ {
			data, err = m.client.DownloadMedia(ctx, media)
			if err == nil && len(data) != 0 {
				break
			}
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
			}
		}
		if err == nil && len(data) != 0 {
			break
		}
	}
	if err != nil || len(data) == 0 {
		if err == nil {
			err = errors.New("微信没有返回可用的图片地址")
		}
		message.Kind, message.Text = "text", message.Text+"（接收失败："+err.Error()+"）"
		return
	}
	stored, err := m.persistence.SaveChatAttachment("clawbot:"+accountID, ChatMessage{
		ID: message.ID, Kind: message.Kind, Sender: "clawbot", ClientID: "__clawbot__",
		FileName: message.FileName, Mime: message.MIME, FileSize: int64(len(data)), SentAt: message.SentAt,
	}, bytes.NewReader(data), 1<<30)
	if err != nil {
		message.Kind, message.Text = "text", message.Text+"（保存失败："+err.Error()+"）"
		return
	}
	message.FileURL, message.FileSize = chatAttachmentURL(stored), int64(len(data))
}

func clawMessageExists(items []ClawBotMessage, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func (m *clawBotManager) setError(accountID, message, status string) {
	m.mu.Lock()
	if binding := m.bindings[accountID]; binding != nil {
		binding.LastError = message
		if status != "" {
			binding.Status = status
		}
		_ = m.saveLocked(binding)
	}
	m.mu.Unlock()
}

func (m *clawBotManager) sendText(accountID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("消息不能为空")
	}
	m.mu.RLock()
	source := m.bindings[accountID]
	if source == nil || source.Status != "bound" {
		m.mu.RUnlock()
		return errors.New("请先绑定微信 ClawBot")
	}
	binding := *source
	m.mu.RUnlock()
	err := m.client.SendText(context.Background(), clawbot.Credentials{BaseURL: binding.BaseURL, Token: binding.BotToken}, binding.WeixinUserID, binding.ContextToken, text)
	if err != nil {
		m.setError(accountID, err.Error(), "bound")
		return err
	}
	m.mu.Lock()
	if current := m.bindings[accountID]; current != nil {
		now := time.Now().UTC()
		current.Messages = append(current.Messages, ClawBotMessage{ID: fmt.Sprintf("local-%d", now.UnixNano()), Kind: "text", Text: text, Mine: true, SentAt: now})
		current.LastMessageAt, current.LastError = now, ""
		_ = m.saveLocked(current)
	}
	m.mu.Unlock()
	return nil
}

func (m *clawBotManager) sendMedia(accountID, fileName, mimeType string, data []byte) error {
	m.mu.RLock()
	source := m.bindings[accountID]
	if source == nil || source.Status != "bound" {
		m.mu.RUnlock()
		return errors.New("请先绑定微信 ClawBot")
	}
	binding := *source
	m.mu.RUnlock()
	image := strings.HasPrefix(strings.ToLower(mimeType), "image/")
	err := m.client.SendMedia(context.Background(), clawbot.Credentials{BaseURL: binding.BaseURL, Token: binding.BotToken}, binding.WeixinUserID, binding.ContextToken, fileName, data, image)
	if err != nil {
		m.setError(accountID, err.Error(), "bound")
		return err
	}
	now := time.Now().UTC()
	kind := "file"
	if image {
		kind = "image"
	}
	message := ClawBotMessage{ID: fmt.Sprintf("local-%d", now.UnixNano()), Kind: kind, Text: "[已发送到微信]", FileName: fileName, MIME: mimeType, FileSize: int64(len(data)), Mine: true, SentAt: now}
	if m.persistence != nil {
		stored, storeErr := m.persistence.SaveChatAttachment("clawbot:"+accountID, ChatMessage{
			ID: message.ID, Kind: kind, Sender: "user", ClientID: accountID, TargetID: "__clawbot__", Private: true,
			FileName: fileName, Mime: mimeType, FileSize: int64(len(data)), SentAt: now,
		}, bytes.NewReader(data), 1<<30)
		if storeErr == nil {
			message.FileURL, message.FileSize = chatAttachmentURL(stored), int64(len(data))
		} else {
			message.Kind, message.Text = "text", "文件已发送到微信，但本地预览保存失败："+storeErr.Error()
		}
	} else {
		message.Kind, message.Text = "text", "文件已发送到微信，但服务器未配置附件存储"
	}
	m.mu.Lock()
	if current := m.bindings[accountID]; current != nil {
		current.Messages = append(current.Messages, message)
		current.LastMessageAt, current.LastError = now, ""
		_ = m.saveLocked(current)
	}
	m.mu.Unlock()
	return nil
}

func (m *clawBotManager) forwardIncoming(accountID string, message ChatMessage, persistence Persistence, recipientOffline bool) {
	m.mu.RLock()
	source := m.bindings[accountID]
	if source == nil || source.Status != "bound" || !source.ForwardEnabled && !recipientOffline {
		m.mu.RUnlock()
		return
	}
	binding := *source
	m.mu.RUnlock()
	name := strings.TrimSpace(message.Name)
	if name == "" {
		name = "TinyChatGo 用户"
	}
	credentials := clawbot.Credentials{BaseURL: binding.BaseURL, Token: binding.BotToken}
	prefix := name + " 发来消息："
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var err error
	switch message.Kind {
	case ChatMessageKindImage, ChatMessageKindFile:
		var data []byte
		if len(message.Data) != 0 {
			data = append([]byte(nil), message.Data...)
		} else if persistence != nil && message.ID != "" {
			attachment, openErr := persistence.OpenChatAttachment(message.ID)
			if openErr == nil {
				data, openErr = io.ReadAll(io.LimitReader(attachment.Reader, 1<<30))
				_ = attachment.Reader.Close()
			}
			if openErr != nil {
				err = openErr
			}
		}
		if err == nil && len(data) != 0 {
			err = m.client.SendText(ctx, credentials, binding.WeixinUserID, binding.ContextToken, prefix)
			if err == nil {
				fileName := message.FileName
				if fileName == "" && message.Kind == ChatMessageKindImage {
					fileName = "TinyChatGo-image.png"
				}
				err = m.client.SendMedia(ctx, credentials, binding.WeixinUserID, binding.ContextToken, fileName, bytes.Clone(data), message.Kind == ChatMessageKindImage)
			}
		} else if err == nil {
			err = errors.New("聊天附件内容不可用")
		}
	default:
		content := message.Text
		if message.Kind == ChatMessageKindCode {
			content = "[代码]\n" + content
		}
		err = m.client.SendText(ctx, credentials, binding.WeixinUserID, binding.ContextToken, prefix+content)
	}
	if err != nil {
		m.logger.Printf("微信 ClawBot 转发失败 account=%s: %v", accountID, err)
		m.setError(accountID, "最近一次转发失败："+err.Error(), "bound")
	}
}

func (m *clawBotManager) setForward(accountID string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	binding := m.bindings[accountID]
	if binding == nil || binding.Status != "bound" {
		return errors.New("请先绑定微信 ClawBot")
	}
	binding.ForwardEnabled = enabled
	return m.saveLocked(binding)
}

func (m *clawBotManager) unbind(accountID string) error {
	m.mu.Lock()
	if cancel := m.cancel[accountID]; cancel != nil {
		cancel()
		delete(m.cancel, accountID)
	}
	delete(m.bindings, accountID)
	store := m.store
	m.mu.Unlock()
	if store != nil {
		return store.DeleteClawBotBinding(accountID)
	}
	return nil
}

func (s *Server) serveClawBotAPI(w http.ResponseWriter, r *http.Request, account Account) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	write := func(status int, value any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	if r.Method != http.MethodGet && !sameWriteOrigin(r) {
		write(http.StatusForbidden, map[string]any{"ok": false, "message": "来源校验失败"})
		return
	}
	switch r.URL.Path {
	case "/__hfs/clawbot/qr-image":
		if r.Method != http.MethodGet {
			write(http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		png, err := s.clawbot.qrImage(account.ID)
		if err != nil {
			write(http.StatusGone, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(png)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(png)
	case "/__hfs/clawbot/state":
		if r.Method != http.MethodGet {
			write(http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		write(http.StatusOK, map[string]any{"ok": true, "clawbot": s.clawbot.publicState(account.ID)})
	case "/__hfs/clawbot/qr":
		if r.Method != http.MethodPost {
			write(http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		state, err := s.clawbot.startQR(account.ID)
		if err != nil {
			write(http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		write(http.StatusOK, map[string]any{"ok": true, "clawbot": state})
	case "/__hfs/clawbot/send":
		var input struct {
			Text string `json:"text"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil {
			write(http.StatusBadRequest, map[string]any{"ok": false, "message": "请求无效"})
			return
		}
		if err := s.clawbot.sendText(account.ID, input.Text); err != nil {
			write(http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		write(http.StatusOK, map[string]any{"ok": true})
	case "/__hfs/clawbot/send-media":
		if r.Method != http.MethodPost {
			write(http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
		file, header, err := r.FormFile("attachment")
		if err != nil {
			write(http.StatusBadRequest, map[string]any{"ok": false, "message": "请选择图片或文件"})
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 1<<30))
		if err != nil || len(data) == 0 {
			write(http.StatusBadRequest, map[string]any{"ok": false, "message": "读取文件失败"})
			return
		}
		mimeType := header.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		if err = s.clawbot.sendMedia(account.ID, header.Filename, mimeType, data); err != nil {
			write(http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		write(http.StatusOK, map[string]any{"ok": true})
	case "/__hfs/clawbot/forward":
		var input struct {
			Enabled bool `json:"enabled"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil {
			write(http.StatusBadRequest, map[string]any{"ok": false})
			return
		}
		if err := s.clawbot.setForward(account.ID, input.Enabled); err != nil {
			write(http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		write(http.StatusOK, map[string]any{"ok": true})
	case "/__hfs/clawbot/unbind":
		if r.Method != http.MethodPost {
			write(http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if err := s.clawbot.unbind(account.ID); err != nil {
			write(http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		write(http.StatusOK, map[string]any{"ok": true})
	default:
		write(http.StatusNotFound, map[string]any{"ok": false})
	}
}
