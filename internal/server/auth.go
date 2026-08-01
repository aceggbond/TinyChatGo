package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	accountCookieName    = "tinychatgo_auth"
	accountSessionMaxAge = 30 * 24 * time.Hour
	accountMinPassword   = 8
	accountMaxPassword   = 1024
	accountMinUsername   = 3
	accountMaxUsername   = 32
)

// Argon2id intentionally uses a memory-hard configuration. Limit concurrent
// derivations so a burst of login attempts cannot exhaust the server's RAM.
var accountKDFSlots = make(chan struct{}, 2)

type accountContextKey struct{}

type authAttempt struct {
	window time.Time
	count  int
}

type accountManager struct {
	mu               sync.RWMutex
	accounts         map[string]*Account
	byUsername       map[string]string
	sessions         map[string]*AccountSession
	attempts         map[string]authAttempt
	persistence      AccountPersistence
	approvalRequired bool
}

func newAccountManager() *accountManager {
	return &accountManager{
		accounts:   make(map[string]*Account),
		byUsername: make(map[string]string),
		sessions:   make(map[string]*AccountSession),
		attempts:   make(map[string]authAttempt),
	}
}

func (m *accountManager) setPersistence(p Persistence) error {
	store, _ := p.(AccountPersistence)
	if store == nil {
		m.mu.Lock()
		m.persistence = nil
		m.mu.Unlock()
		return nil
	}
	accounts, err := store.LoadAccounts()
	if err != nil {
		return fmt.Errorf("load accounts: %w", err)
	}
	sessions, err := store.LoadAccountSessions()
	if err != nil {
		return fmt.Errorf("load account sessions: %w", err)
	}
	now := time.Now().UTC()
	m.mu.Lock()
	m.persistence = store
	m.accounts = make(map[string]*Account, len(accounts))
	m.byUsername = make(map[string]string, len(accounts))
	m.sessions = make(map[string]*AccountSession, len(sessions))
	for index := range accounts {
		item := accounts[index]
		if normalizeAccountID(item.ID) == "" || normalizeUsername(item.Username) == "" {
			continue
		}
		copy := item
		m.accounts[item.ID] = &copy
		m.byUsername[normalizeUsername(item.Username)] = item.ID
	}
	var expired []string
	for index := range sessions {
		item := sessions[index]
		account := m.accounts[item.AccountID]
		if account == nil || account.Status != AccountStatusActive || item.ExpiresAt.Before(now) {
			expired = append(expired, item.TokenHash)
			continue
		}
		copy := item
		m.sessions[item.TokenHash] = &copy
	}
	m.mu.Unlock()
	for _, tokenHash := range expired {
		_ = store.DeleteAccountSession(tokenHash)
	}
	return nil
}

func (m *accountManager) enabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persistence != nil
}

func (s *Server) SetAccountApprovalRequired(required bool) {
	s.auth.mu.Lock()
	s.auth.approvalRequired = required
	s.auth.mu.Unlock()
}

func (s *Server) AccountApprovalRequired() bool {
	s.auth.mu.RLock()
	defer s.auth.mu.RUnlock()
	return s.auth.approvalRequired
}

func (s *Server) Accounts() []AccountSummary {
	s.auth.mu.RLock()
	items := make([]AccountSummary, 0, len(s.auth.accounts))
	for _, account := range s.auth.accounts {
		items = append(items, accountSummary(*account))
	}
	s.auth.mu.RUnlock()
	sortAccountSummaries(items)
	return items
}

func sortAccountSummaries(items []AccountSummary) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			left, right := items[j-1], items[j]
			pendingLeft := left.Status == AccountStatusPending
			pendingRight := right.Status == AccountStatusPending
			if pendingLeft != pendingRight {
				if pendingLeft {
					break
				}
			} else if !right.CreatedAt.Before(left.CreatedAt) {
				break
			}
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

func accountSummary(account Account) AccountSummary {
	return AccountSummary{
		ID: account.ID, Username: account.Username, Status: account.Status,
		CreatedAt: account.CreatedAt, ApprovedAt: account.ApprovedAt,
		LastLoginAt: account.LastLoginAt, LastIP: account.LastIP,
	}
}

func (s *Server) SetAccountStatus(id string, status AccountStatus) error {
	id = normalizeAccountID(id)
	if id == "" {
		return errors.New("账号 ID 无效")
	}
	switch status {
	case AccountStatusActive, AccountStatusRejected, AccountStatusDisabled:
	default:
		return errors.New("账号状态无效")
	}
	s.auth.mu.Lock()
	account := s.auth.accounts[id]
	if account == nil {
		s.auth.mu.Unlock()
		return errors.New("账号不存在")
	}
	now := time.Now().UTC()
	next := *account
	next.Status = status
	next.UpdatedAt = now
	if status == AccountStatusActive {
		next.ApprovedAt = now
	}
	store := s.auth.persistence
	if store != nil {
		if err := store.SaveAccount(next); err != nil {
			s.auth.mu.Unlock()
			return err
		}
	}
	*account = next
	if status != AccountStatusActive {
		for token, session := range s.auth.sessions {
			if session.AccountID == id {
				delete(s.auth.sessions, token)
			}
		}
	}
	s.auth.mu.Unlock()
	if store != nil {
		if status != AccountStatusActive {
			_ = store.DeleteAccountSessions(id)
		}
	}
	if status != AccountStatusActive {
		s.chat.disconnectIdentity(id, "account access changed")
	}
	return nil
}

// SetAccountPassword replaces one registered account's password and revokes
// every existing login session for that account. Revoking sessions is
// intentional: an administrator password reset must also remove access from
// devices that still hold an old authentication cookie.
func (s *Server) SetAccountPassword(id, password string) error {
	id = normalizeAccountID(id)
	if id == "" {
		return errors.New("账号 ID 无效")
	}
	if err := validatePassword(password); err != nil {
		return err
	}
	passwordHash, err := hashAccountPassword(password)
	if err != nil {
		return fmt.Errorf("生成密码摘要失败：%w", err)
	}

	s.auth.mu.Lock()
	account := s.auth.accounts[id]
	if account == nil {
		s.auth.mu.Unlock()
		return errors.New("账号不存在")
	}
	next := *account
	next.PasswordHash = passwordHash
	next.UpdatedAt = time.Now().UTC()
	store := s.auth.persistence
	if store != nil {
		if err = store.SaveAccount(next); err != nil {
			s.auth.mu.Unlock()
			return err
		}
	}
	*account = next
	for token, session := range s.auth.sessions {
		if session.AccountID == id {
			delete(s.auth.sessions, token)
		}
	}
	s.auth.mu.Unlock()
	if store != nil {
		_ = store.DeleteAccountSessions(id)
	}
	s.chat.disconnectIdentity(id, "account password changed")
	return nil
}

func (s *Server) DeleteAccount(id string) error {
	id = normalizeAccountID(id)
	if id == "" {
		return errors.New("账号 ID 无效")
	}
	s.auth.mu.Lock()
	account := s.auth.accounts[id]
	if account == nil {
		s.auth.mu.Unlock()
		return errors.New("账号不存在")
	}
	delete(s.auth.accounts, id)
	delete(s.auth.byUsername, normalizeUsername(account.Username))
	for token, session := range s.auth.sessions {
		if session.AccountID == id {
			delete(s.auth.sessions, token)
		}
	}
	store := s.auth.persistence
	s.auth.mu.Unlock()
	s.chat.disconnectIdentity(id, "account deleted")
	if store != nil {
		return store.DeleteAccount(id)
	}
	return nil
}

func (h *chatHub) disconnectIdentity(id, reason string) {
	h.mu.Lock()
	var peers []*chatPeer
	for key, peer := range h.peers {
		if peer.ip == id {
			peers = append(peers, peer)
			delete(h.peers, key)
		}
	}
	slow := h.broadcastUserListLocked()
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	for _, peer := range peers {
		peer.shutdown(websocketClosePolicyViolation, reason)
	}
}

func normalizeAccountID(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if len(raw) != 34 || !strings.HasPrefix(raw, "u_") {
		return ""
	}
	if _, err := hex.DecodeString(raw[2:]); err != nil {
		return ""
	}
	return raw
}

func normalizeUsername(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func validateUsername(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	count := utf8.RuneCountInString(raw)
	if count < accountMinUsername || count > accountMaxUsername {
		return "", fmt.Errorf("用户名长度须为 %d–%d 个字符", accountMinUsername, accountMaxUsername)
	}
	for _, r := range raw {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		return "", errors.New("用户名只能包含中文、字母、数字、下划线或短横线")
	}
	return raw, nil
}

func validatePassword(password string) error {
	n := len([]byte(password))
	if n < accountMinPassword {
		return fmt.Errorf("密码至少需要 %d 个字符", accountMinPassword)
	}
	if n > accountMaxPassword {
		return errors.New("密码过长")
	}
	return nil
}

func newOpaqueToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashAccountPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	accountKDFSlots <- struct{}{}
	defer func() { <-accountKDFSlots }()
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyAccountPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	if memory < 8*1024 || memory > 256*1024 || iterations < 1 || iterations > 10 || parallelism < 1 || parallelism > 8 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) < 16 || len(expected) > 64 {
		return false
	}
	accountKDFSlots <- struct{}{}
	defer func() { <-accountKDFSlots }()
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func (m *accountManager) allowAttempt(ip string) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.attempts[ip]
	if item.window.IsZero() || now.Sub(item.window) >= time.Minute {
		item = authAttempt{window: now}
	}
	item.count++
	m.attempts[ip] = item
	if len(m.attempts) > 4096 {
		for key, attempt := range m.attempts {
			if now.Sub(attempt.window) >= time.Minute {
				delete(m.attempts, key)
			}
		}
	}
	return item.count <= 20
}

func (s *Server) authenticatedAccount(r *http.Request) (Account, bool) {
	cookie, err := r.Cookie(accountCookieName)
	if err != nil || cookie.Value == "" {
		return Account{}, false
	}
	tokenHash := hashSessionToken(cookie.Value)
	now := time.Now().UTC()
	s.auth.mu.Lock()
	session := s.auth.sessions[tokenHash]
	if session == nil || session.ExpiresAt.Before(now) {
		store := s.auth.persistence
		if session != nil {
			delete(s.auth.sessions, tokenHash)
		}
		s.auth.mu.Unlock()
		if session != nil && store != nil {
			_ = store.DeleteAccountSession(tokenHash)
		}
		return Account{}, false
	}
	account := s.auth.accounts[session.AccountID]
	if account == nil || account.Status != AccountStatusActive {
		delete(s.auth.sessions, tokenHash)
		s.auth.mu.Unlock()
		return Account{}, false
	}
	copy := *account
	if now.Sub(session.LastSeen) >= time.Hour {
		session.LastSeen = now
		sessionCopy := *session
		store := s.auth.persistence
		s.auth.mu.Unlock()
		if store != nil {
			_ = store.SaveAccountSession(sessionCopy)
		}
		return copy, true
	}
	s.auth.mu.Unlock()
	return copy, true
}

func accountFromContext(ctx context.Context) (Account, bool) {
	account, ok := ctx.Value(accountContextKey{}).(Account)
	return account, ok && account.ID != ""
}

func (s *Server) createAccountSession(w http.ResponseWriter, r *http.Request, accountID, clientIP string) error {
	token, err := newOpaqueToken(32)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	session := AccountSession{
		TokenHash: hashSessionToken(token), AccountID: accountID,
		CreatedAt: now, ExpiresAt: now.Add(accountSessionMaxAge), LastSeen: now, LastIP: clientIP,
	}
	s.auth.mu.Lock()
	s.auth.sessions[session.TokenHash] = &session
	store := s.auth.persistence
	s.auth.mu.Unlock()
	if store != nil {
		if err = store.SaveAccountSession(session); err != nil {
			s.auth.mu.Lock()
			delete(s.auth.sessions, session.TokenHash)
			s.auth.mu.Unlock()
			return err
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: accountCookieName, Value: token, Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
		MaxAge: int(accountSessionMaxAge.Seconds()), Expires: session.ExpiresAt,
	})
	return nil
}

func clearAccountCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: accountCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
		MaxAge: -1, Expires: time.Unix(1, 0),
	})
}

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func decodeAuthRequest(w http.ResponseWriter, r *http.Request) (authRequest, bool) {
	var input authRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请求格式无效"})
		return authRequest{}, false
	}
	return input, true
}

func writeAuthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) serveAuthAPI(w http.ResponseWriter, r *http.Request, clientIP string) {
	if r.Method != http.MethodGet && !sameWriteOrigin(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "来源校验失败"})
		return
	}
	switch r.URL.Path {
	case "/__auth/status":
		if r.Method != http.MethodGet {
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		account, ok := s.authenticatedAccount(r)
		writeAuthJSON(w, http.StatusOK, map[string]any{
			"ok": true, "authenticated": ok,
			"approvalRequired": s.AccountApprovalRequired(),
			"account": func() any {
				if !ok {
					return nil
				}
				return accountSummary(account)
			}(),
		})
	case "/__auth/register":
		if r.Method != http.MethodPost {
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !s.auth.allowAttempt(clientIP) {
			writeAuthJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "message": "尝试次数过多，请稍后再试"})
			return
		}
		input, ok := decodeAuthRequest(w, r)
		if !ok {
			return
		}
		username, err := validateUsername(input.Username)
		if err != nil {
			writeAuthJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		if err = validatePassword(input.Password); err != nil {
			writeAuthJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		passwordHash, err := hashAccountPassword(input.Password)
		if err != nil {
			writeAuthJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "无法创建账号"})
			return
		}
		idToken, err := newOpaqueToken(16)
		if err != nil {
			writeAuthJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "无法创建账号"})
			return
		}
		idBytes, err := base64.RawURLEncoding.DecodeString(idToken)
		if err != nil {
			writeAuthJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "无法创建账号"})
			return
		}
		now := time.Now().UTC()
		status := AccountStatusActive
		if s.AccountApprovalRequired() {
			status = AccountStatusPending
		}
		account := Account{
			ID: "u_" + hex.EncodeToString(idBytes), Username: username,
			PasswordHash: passwordHash, Status: status,
			CreatedAt: now, UpdatedAt: now, LastIP: clientIP,
		}
		if status == AccountStatusActive {
			account.ApprovedAt = now
			account.LastLoginAt = now
		}
		s.auth.mu.Lock()
		key := normalizeUsername(username)
		if _, exists := s.auth.byUsername[key]; exists {
			s.auth.mu.Unlock()
			writeAuthJSON(w, http.StatusConflict, map[string]any{"ok": false, "message": "用户名已被使用"})
			return
		}
		copy := account
		s.auth.accounts[account.ID] = &copy
		s.auth.byUsername[key] = account.ID
		store := s.auth.persistence
		s.auth.mu.Unlock()
		if store != nil {
			if err = store.SaveAccount(account); err != nil {
				s.auth.mu.Lock()
				delete(s.auth.accounts, account.ID)
				delete(s.auth.byUsername, key)
				s.auth.mu.Unlock()
				writeAuthJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "保存账号失败"})
				return
			}
		}
		if status == AccountStatusPending {
			writeAuthJSON(w, http.StatusAccepted, map[string]any{"ok": true, "pending": true, "message": "注册申请已提交，请等待管理员审批"})
			return
		}
		if err = s.createAccountSession(w, r, account.ID, clientIP); err != nil {
			writeAuthJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "账号已创建，请重新登录"})
			return
		}
		writeAuthJSON(w, http.StatusCreated, map[string]any{"ok": true, "authenticated": true})
	case "/__auth/login":
		if r.Method != http.MethodPost {
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !s.auth.allowAttempt(clientIP) {
			writeAuthJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "message": "尝试次数过多，请稍后再试"})
			return
		}
		input, ok := decodeAuthRequest(w, r)
		if !ok {
			return
		}
		s.auth.mu.RLock()
		accountID := s.auth.byUsername[normalizeUsername(input.Username)]
		account := s.auth.accounts[accountID]
		var copy Account
		if account != nil {
			copy = *account
		}
		s.auth.mu.RUnlock()
		if account == nil || !verifyAccountPassword(copy.PasswordHash, input.Password) {
			writeAuthJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "用户名或密码错误"})
			return
		}
		switch copy.Status {
		case AccountStatusPending:
			writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "pending": true, "message": "账号正在等待管理员审批"})
			return
		case AccountStatusRejected:
			writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "注册申请未获批准"})
			return
		case AccountStatusDisabled:
			writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "账号已被管理员停用"})
			return
		case AccountStatusActive:
		default:
			writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "账号不可用"})
			return
		}
		now := time.Now().UTC()
		copy.LastLoginAt, copy.LastIP, copy.UpdatedAt = now, clientIP, now
		s.auth.mu.Lock()
		if current := s.auth.accounts[copy.ID]; current != nil {
			*current = copy
		}
		store := s.auth.persistence
		s.auth.mu.Unlock()
		if store != nil {
			_ = store.SaveAccount(copy)
		}
		if err := s.createAccountSession(w, r, copy.ID, clientIP); err != nil {
			writeAuthJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "登录会话创建失败"})
			return
		}
		writeAuthJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true})
	case "/__auth/logout":
		if r.Method != http.MethodPost {
			writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if cookie, err := r.Cookie(accountCookieName); err == nil && cookie.Value != "" {
			tokenHash := hashSessionToken(cookie.Value)
			s.auth.mu.Lock()
			delete(s.auth.sessions, tokenHash)
			store := s.auth.persistence
			s.auth.mu.Unlock()
			if store != nil {
				_ = store.DeleteAccountSession(tokenHash)
			}
		}
		clearAccountCookie(w, r)
		writeAuthJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) renderAuthPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'self'")
	approval := "false"
	if s.AccountApprovalRequired() {
		approval = "true"
	}
	returnTo, _ := json.Marshal(r.URL.RequestURI())
	_, _ = fmt.Fprintf(w, authPageHTML, approval, returnTo)
}

const authPageHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>登录 · TinyChatGo</title><style>
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px;font-family:Inter,"Segoe UI","Microsoft YaHei",sans-serif;color:#172033;background:linear-gradient(145deg,#eef5ff,#f8fbff 48%%,#edf9f5)}.shell{width:min(940px,100%%);display:grid;grid-template-columns:1.08fr .92fr;overflow:hidden;border:1px solid #dce6f3;border-radius:26px;background:#fff;box-shadow:0 24px 70px rgba(38,76,125,.14)}.intro{padding:56px;background:linear-gradient(150deg,#1769e0,#3a83ee);color:#fff}.brand{display:flex;align-items:center;gap:13px}.brand img{width:58px;height:58px;border-radius:15px}.brand b{font-size:26px}.intro h1{font-size:38px;line-height:1.18;margin:72px 0 16px}.intro p{font-size:16px;line-height:1.8;opacity:.88}.secure{margin-top:42px;padding:14px 16px;border:1px solid rgba(255,255,255,.22);border-radius:14px;background:rgba(255,255,255,.1);font-size:13px;line-height:1.6}.panel{padding:46px 48px}.tabs{display:flex;gap:8px;padding:5px;border-radius:12px;background:#f1f5fa}.tab{flex:1;height:40px;border:0;border-radius:9px;background:transparent;color:#708099;font-weight:700;cursor:pointer}.tab.active{background:#fff;color:#1769e0;box-shadow:0 3px 10px rgba(52,80,118,.1)}h2{margin:30px 0 8px;font-size:25px}.sub{margin:0 0 25px;color:#8491a6;font-size:14px}.field{display:grid;gap:8px;margin:16px 0}.field label{font-size:13px;font-weight:700;color:#526178}.field input{width:100%%;height:48px;padding:0 14px;border:1px solid #d5dfeb;border-radius:11px;outline:0;font-size:15px}.field input:focus{border-color:#3b82f6;box-shadow:0 0 0 3px #e8f1ff}.submit{width:100%%;height:48px;margin-top:12px;border:0;border-radius:11px;background:#2675e8;color:#fff;font-size:15px;font-weight:800;cursor:pointer}.submit:disabled{opacity:.55}.message{min-height:22px;margin-top:15px;color:#d64242;font-size:13px;line-height:1.55}.message.ok{color:#16865c}.approval{padding:11px 13px;border-radius:10px;background:#fff7e6;color:#9a6615;font-size:12px;line-height:1.55}@media(max-width:760px){.shell{grid-template-columns:1fr}.intro{display:none}.panel{padding:32px 24px}}</style></head><body><main class="shell"><section class="intro"><div class="brand"><img src="/__hfs/logo.png"><b>TinyChatGo</b></div><h1>登录后，开始安全沟通</h1><p>账号身份替代 IP 身份。聊天、群组、文件和历史归档都会稳定关联到你的账号。</p><div class="secure">部署到外网时请启用 HTTPS。密码使用 Argon2id 单向哈希保存，登录会话使用 HttpOnly 安全 Cookie。</div></section><section class="panel"><div class="tabs"><button class="tab active" data-mode="login">登录</button><button class="tab" data-mode="register">注册</button></div><h2 id="title">欢迎回来</h2><p id="sub" class="sub">登录后方可使用聊天与文件功能</p><div id="approval" class="approval" hidden>新用户注册后需要管理员审批，审批通过后才能登录。</div><form id="form"><div class="field"><label>用户名</label><input id="username" autocomplete="username" minlength="3" maxlength="32" required></div><div class="field"><label>密码</label><input id="password" type="password" autocomplete="current-password" minlength="8" maxlength="1024" required></div><button id="submit" class="submit">登录</button><div id="message" class="message"></div></form></section></main><script>
(()=>{const approval=%s,returnTo=%s;let mode="login";const q=id=>document.getElementById(id),tabs=[...document.querySelectorAll(".tab")];function setMode(next){mode=next;tabs.forEach(x=>x.classList.toggle("active",x.dataset.mode===mode));q("title").textContent=mode==="login"?"欢迎回来":"创建账号";q("sub").textContent=mode==="login"?"登录后方可使用聊天与文件功能":"用户名注册后不可重复使用";q("submit").textContent=mode==="login"?"登录":"注册";q("password").autocomplete=mode==="login"?"current-password":"new-password";q("approval").hidden=!(approval&&mode==="register");q("message").textContent="";}tabs.forEach(x=>x.onclick=()=>setMode(x.dataset.mode));q("form").onsubmit=async e=>{e.preventDefault();const b=q("submit"),m=q("message");b.disabled=true;m.className="message";m.textContent=mode==="login"?"正在登录…":"正在注册…";try{const res=await fetch("/__auth/"+mode,{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({username:q("username").value.trim(),password:q("password").value})});const data=await res.json();if(!res.ok&&!data.pending)throw new Error(data.message||"操作失败");if(data.pending){m.className="message ok";m.textContent=data.message;setMode("login");return}location.replace(returnTo&&returnTo.startsWith("/")?returnTo:"/")}catch(err){m.textContent=err.message||"操作失败"}finally{b.disabled=false}};})();
</script></body></html>`
