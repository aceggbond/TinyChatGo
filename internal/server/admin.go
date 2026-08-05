package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const adminCookieName = "tinychatgo_admin"

type adminManager struct {
	mu           sync.Mutex
	password     [32]byte
	enabled      bool
	sessions     map[[32]byte]time.Time
	saveSettings func(AdminSettings) error
}

type AdminSettings struct {
	RequireApproval bool `json:"requireApproval"`
	AllowGroups     bool `json:"allowGroups"`
	ShowUsers       bool `json:"showUsers"`
	PrivateChat     bool `json:"privateChat"`
}

func newAdminManager() *adminManager { return &adminManager{sessions: make(map[[32]byte]time.Time)} }

// SetAdminPassword enables the separate web administration surface. An empty
// password disables it and invalidates every administrator session.
func (s *Server) SetAdminPassword(password string) {
	digest := sha256.Sum256([]byte(password))
	s.admin.mu.Lock()
	s.admin.password = digest
	s.admin.enabled = password != ""
	s.admin.sessions = make(map[[32]byte]time.Time)
	s.admin.mu.Unlock()
}

func (s *Server) SetAdminSettingsSaver(save func(AdminSettings) error) {
	s.admin.mu.Lock()
	s.admin.saveSettings = save
	s.admin.mu.Unlock()
}

// AdminHandler exposes only the administrator page and APIs. It is intended
// for a dedicated listener so public chat ports never serve management paths.
func (s *Server) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin" && r.URL.Path != "/admin/" && !strings.HasPrefix(r.URL.Path, "/__admin/") {
			http.NotFound(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		s.serveAdmin(w, r, host)
	})
}

func (s *Server) adminEnabled() bool {
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	return s.admin.enabled
}

func (s *Server) adminAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	now := time.Now()
	s.admin.mu.Lock()
	defer s.admin.mu.Unlock()
	expires, ok := s.admin.sessions[digest]
	if !ok || !expires.After(now) {
		delete(s.admin.sessions, digest)
		return false
	}
	return true
}

func (s *Server) createAdminSession(w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	expires := time.Now().Add(12 * time.Hour)
	s.admin.mu.Lock()
	for key, expiry := range s.admin.sessions {
		if !expiry.After(time.Now()) {
			delete(s.admin.sessions, key)
		}
	}
	s.admin.sessions[digest] = expires
	s.admin.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 12 * 60 * 60})
	return nil
}

func (s *Server) clearAdminSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(adminCookieName); err == nil {
		digest := sha256.Sum256([]byte(cookie.Value))
		s.admin.mu.Lock()
		delete(s.admin.sessions, digest)
		s.admin.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func adminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请求内容无效"})
		return false
	}
	return true
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request, clientIP string) {
	if !s.adminEnabled() {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
		return
	}
	if r.URL.Path == "/admin/" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(adminFullPageHTML))
		}
		return
	}
	if r.Method != http.MethodGet && !sameWriteOrigin(r) {
		adminJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "来源校验失败"})
		return
	}
	if r.URL.Path == "/__admin/login" {
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !s.auth.allowAttempt(clientIP) {
			adminJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "message": "尝试次数过多，请稍后再试"})
			return
		}
		var input struct {
			Password string `json:"password"`
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		candidate := sha256.Sum256([]byte(input.Password))
		s.admin.mu.Lock()
		valid := s.admin.enabled && subtle.ConstantTimeCompare(candidate[:], s.admin.password[:]) == 1
		s.admin.mu.Unlock()
		if !valid {
			adminJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "管理员密码错误"})
			return
		}
		if err := s.createAdminSession(w, r); err != nil {
			adminJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "无法建立管理会话"})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if !s.adminAuthenticated(r) {
		adminJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "authenticated": false})
		return
	}
	switch r.URL.Path {
	case "/__admin/status":
		if r.Method != http.MethodGet {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true, "online": s.ChatOnlineCount(), "accounts": len(s.Accounts()), "approvalRequired": s.AccountApprovalRequired()})
	case "/__admin/accounts":
		if r.Method != http.MethodGet {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		type row struct {
			AccountSummary
			Online bool `json:"online"`
		}
		accounts := s.Accounts()
		rows := make([]row, 0, len(accounts))
		for _, account := range accounts {
			rows = append(rows, row{AccountSummary: account, Online: s.ChatIdentityOnline(account.ID)})
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": rows})
	case "/__admin/state":
		if r.Method != http.MethodGet {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		adminJSON(w, http.StatusOK, s.adminState())
	case "/__admin/settings":
		var input AdminSettings
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		s.SetAccountApprovalRequired(input.RequireApproval)
		s.SetUserGroupCreationEnabled(input.AllowGroups)
		s.SetUserListEnabled(input.ShowUsers)
		s.SetPrivateMessagesEnabled(input.PrivateChat && input.ShowUsers)
		s.admin.mu.Lock()
		save := s.admin.saveSettings
		s.admin.mu.Unlock()
		if save != nil {
			if err := save(input); err != nil {
				adminJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "保存设置失败：" + err.Error()})
				return
			}
		}
		adminJSON(w, http.StatusOK, s.adminState())
	case "/__admin/user/name":
		var input struct{ ID, Name string }
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.SetChatUserName(input.ID, input.Name); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/user/blacklist":
		var input struct {
			ID          string
			Blacklisted bool
		}
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.SetChatUserBlacklisted(input.ID, input.Blacklisted); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/user/delete":
		var input struct{ ID string }
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		for _, account := range s.Accounts() {
			if account.ID == input.ID {
				if err := s.DeleteAccount(input.ID); err != nil {
					adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
					return
				}
				break
			}
		}
		_ = s.RemoveChatVisitor(input.ID)
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/group/rename":
		var input struct{ ID, Name string }
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.RenameChatGroup(input.ID, input.Name); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/group/member/remove":
		var input struct{ ID, Member string }
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.RemoveChatGroupMember(input.ID, input.Member); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/group/delete":
		var input struct{ ID string }
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.DeleteChatGroup(input.ID); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/archives":
		if r.Method != http.MethodGet {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		persistence := s.Persistence()
		if persistence == nil {
			adminJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "message": "归档数据库未启用"})
			return
		}
		page, err := persistence.ListChatAttachments(adminArchiveQuery(r.URL.Query()))
		if err != nil {
			adminJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, page)
	case "/__admin/archive/file":
		if r.Method != http.MethodGet {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		persistence := s.Persistence()
		if persistence == nil {
			http.NotFound(w, r)
			return
		}
		attachment, err := persistence.OpenChatAttachment(strings.TrimSpace(r.URL.Query().Get("id")))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer attachment.Reader.Close()
		contentType := attachment.MIME
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.FormatInt(attachment.Size, 10))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": attachment.Name}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.Copy(w, attachment.Reader)
	case "/__admin/archive/delete":
		var input struct{ ID string }
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.DeleteChatMessage(input.ID); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/data/clear-history":
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if err := s.ClearChatHistory(); err != nil {
			adminJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/data/clear-users":
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if err := s.ClearChatUsers(); err != nil {
			adminJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/logs":
		if r.Method != http.MethodGet {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if persistence := s.Persistence(); persistence != nil {
			records, err := persistence.ListAccessRecords(500)
			if err != nil {
				adminJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
				return
			}
			adminJSON(w, http.StatusOK, map[string]any{"ok": true, "records": records})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true, "records": []AccessRecord{}})
	case "/__admin/logs/clear":
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if persistence := s.Persistence(); persistence != nil {
			if err := persistence.ClearAccessRecords(); err != nil {
				adminJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": err.Error()})
				return
			}
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/account/status":
		var input struct {
			ID     string        `json:"id"`
			Status AccountStatus `json:"status"`
		}
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.SetAccountStatus(input.ID, input.Status); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/account/password":
		var input struct{ ID, Password string }
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.SetAccountPassword(input.ID, input.Password); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/account/delete":
		var input struct {
			ID string `json:"id"`
		}
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		if !decodeAdminJSON(w, r, &input) {
			return
		}
		if err := s.DeleteAccount(input.ID); err != nil {
			adminJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/__admin/logout":
		if r.Method != http.MethodPost {
			adminJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
			return
		}
		s.clearAdminSession(w, r)
		adminJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

type adminUserRow struct {
	ID          string         `json:"id"`
	Username    string         `json:"username,omitempty"`
	Name        string         `json:"name,omitempty"`
	Status      AccountStatus  `json:"status,omitempty"`
	Online      bool           `json:"online"`
	Blacklisted bool           `json:"blacklisted"`
	FirstSeen   time.Time      `json:"firstSeen,omitempty"`
	LastSeen    time.Time      `json:"lastSeen,omitempty"`
	LastIP      string         `json:"lastIp,omitempty"`
	Client      ChatClientInfo `json:"client"`
}

func (s *Server) adminState() map[string]any {
	profiles := make(map[string]ChatUser)
	for _, profile := range s.ChatUsers() {
		profiles[profile.IP] = profile
	}
	accounts := s.Accounts()
	users := make([]adminUserRow, 0, len(accounts)+len(profiles))
	seen := make(map[string]bool)
	for _, account := range accounts {
		profile, found := profiles[account.ID]
		row := adminUserRow{ID: account.ID, Username: account.Username, Status: account.Status, Online: s.ChatIdentityOnline(account.ID), FirstSeen: account.CreatedAt, LastSeen: account.LastLoginAt, LastIP: account.LastIP, Client: ChatClientInfo{IP: account.LastIP}}
		if found {
			row.Name, row.Blacklisted, row.FirstSeen, row.LastSeen, row.Client = profile.Name, profile.Blacklisted, profile.FirstSeen, profile.LastSeen, profile.Client
		}
		users = append(users, row)
		seen[account.ID] = true
	}
	for id, profile := range profiles {
		if seen[id] {
			continue
		}
		users = append(users, adminUserRow{ID: id, Name: profile.Name, Online: s.ChatIdentityOnline(id), Blacklisted: profile.Blacklisted, FirstSeen: profile.FirstSeen, LastSeen: profile.LastSeen, LastIP: profile.Client.IP, Client: profile.Client})
	}
	addresses := s.listenAddresses()
	return map[string]any{
		"ok": true, "running": addresses.HTTP != "", "httpAddress": addresses.HTTP, "httpsAddress": addresses.HTTPS,
		"onlineCount": s.ChatOnlineCount(), "users": users, "groups": s.ChatGroups(),
		"settings": map[string]any{"requireApproval": s.AccountApprovalRequired(), "allowGroups": s.UserGroupCreationEnabled(), "showUsers": s.UserListEnabled(), "privateChat": s.PrivateMessagesEnabled(), "clientDownload": s.ClientDownloadEnabled()},
	}
}

func adminArchiveQuery(values url.Values) ChatArchiveQuery {
	parseDate := func(raw string, end bool) time.Time {
		value, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), time.Local)
		if err != nil {
			return time.Time{}
		}
		if end {
			value = value.Add(24*time.Hour - time.Nanosecond)
		}
		return value.UTC()
	}
	page, _ := strconv.Atoi(values.Get("page"))
	return ChatArchiveQuery{Kind: strings.TrimSpace(values.Get("kind")), Query: strings.TrimSpace(values.Get("query")), From: parseDate(values.Get("from"), false), To: parseDate(values.Get("to"), true), Page: page, PageSize: 36}
}

const adminPageHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>TinyChatGo 管理后台</title><style>
*{box-sizing:border-box}body{margin:0;background:#f3f5f9;color:#18212f;font:14px system-ui,sans-serif}.wrap{max-width:1100px;margin:auto;padding:28px}.card{background:#fff;border-radius:14px;padding:20px;box-shadow:0 6px 25px #17233a12;margin-bottom:18px}h1{margin:0 0 6px}input,button,select{font:inherit;padding:9px 12px;border:1px solid #ccd3df;border-radius:8px}button{cursor:pointer;background:#1565d8;color:#fff;border:0}.danger{background:#c62828}.muted{color:#667085}.top{display:flex;justify-content:space-between;gap:12px;align-items:center}.stats{display:flex;gap:24px;margin-top:14px}.num{font-size:24px;font-weight:700}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:11px 8px;border-bottom:1px solid #edf0f4}.actions{display:flex;flex-wrap:wrap;gap:6px}.actions button{padding:6px 9px}.online{color:#138a4b}.error{color:#c62828}#panel,#login{display:none}@media(max-width:720px){.wrap{padding:12px}.table{overflow:auto}th,td{white-space:nowrap}}
</style></head><body><div class="wrap"><div id="login" class="card"><h1>TinyChatGo 管理后台</h1><p class="muted">请输入独立的管理员密码</p><form id="loginForm"><input id="password" type="password" autocomplete="current-password" required><button>登录</button></form><p id="loginError" class="error"></p></div><div id="panel"><div class="card"><div class="top"><div><h1>管理后台</h1><span class="muted">账号与在线状态</span></div><button id="logout">退出</button></div><div class="stats"><div><div id="online" class="num">0</div><span class="muted">在线</span></div><div><div id="total" class="num">0</div><span class="muted">账号</span></div></div></div><div class="card table"><table><thead><tr><th>用户</th><th>状态</th><th>在线</th><th>最近 IP</th><th>操作</th></tr></thead><tbody id="rows"></tbody></table><p id="error" class="error"></p></div></div></div><script>
const login=document.querySelector('#login'),panel=document.querySelector('#panel'),rows=document.querySelector('#rows'),online=document.querySelector('#online'),total=document.querySelector('#total'),errorBox=document.querySelector('#error'),password=document.querySelector('#password'),loginError=document.querySelector('#loginError'),logout=document.querySelector('#logout');async function api(path,body){let r=await fetch(path,{method:body?'POST':'GET',headers:body?{'Content-Type':'application/json'}:{},body:body?JSON.stringify(body):null});let d=await r.json().catch(()=>({}));if(!r.ok)throw Object.assign(new Error(d.message||'请求失败'),{status:r.status});return d}function esc(v){return String(v||'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}function accountRow(x){return '<tr><td>'+esc(x.username)+'</td><td>'+esc(x.status)+'</td><td class="'+(x.online?'online':'')+'">'+(x.online?'在线':'离线')+'</td><td>'+esc(x.lastIp)+'</td><td><div class="actions" data-id="'+esc(x.id)+'"><button data-status="active">批准/启用</button><button data-status="disabled">禁用</button><button data-status="rejected">拒绝</button><button data-reset="1">重置密码</button><button class="danger" data-delete="1">删除</button></div></td></tr>'}async function load(){try{let [s,a]=await Promise.all([api('/__admin/status'),api('/__admin/accounts')]);login.style.display='none';panel.style.display='block';online.textContent=s.online;total.textContent=s.accounts;rows.innerHTML=a.accounts.map(accountRow).join('');errorBox.textContent=''}catch(e){if(e.status===401){panel.style.display='none';login.style.display='block'}else errorBox.textContent=e.message}}document.querySelector('#loginForm').onsubmit=async e=>{e.preventDefault();try{await api('/__admin/login',{password:password.value});password.value='';loginError.textContent='';await load()}catch(e){loginError.textContent=e.message}};rows.onclick=async e=>{let box=e.target.closest('[data-id]');if(!box)return;let id=box.dataset.id;try{if(e.target.dataset.status)await api('/__admin/account/status',{id,status:e.target.dataset.status});else if(e.target.dataset.reset){let p=prompt('输入新密码（至少 8 个字符）');if(p===null)return;await api('/__admin/account/password',{id,password:p})}else if(e.target.dataset.delete){if(!confirm('确定永久删除这个账号？'))return;await api('/__admin/account/delete',{id})}await load()}catch(x){errorBox.textContent=x.message}};logout.onclick=async()=>{await api('/__admin/logout',{});location.reload()};load();setInterval(()=>{if(panel.style.display==='block')load()},10000);
</script></body></html>`
