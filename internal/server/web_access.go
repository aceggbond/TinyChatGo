package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const webAccessCookieName = "lanchatgo_web_access"

// webAccessRequired reports whether the request has passed the optional
// server-wide web password gate. The native client can use the same gate with
// a compile-time password header, while browsers receive a short HTML form.
func (s *Server) webAccessRequired(r *http.Request) bool {
	s.mu.RLock()
	password, token := s.password, s.accessToken
	s.mu.RUnlock()
	if password == "" {
		return false
	}
	// Keep the historical BasicAuth credential working for native embedders
	// and existing integrations. Browsers use the HTML form below and receive
	// a scoped HttpOnly cookie instead.
	if _, basicPassword, ok := r.BasicAuth(); ok &&
		subtle.ConstantTimeCompare([]byte(basicPassword), []byte(password)) == 1 {
		return false
	}
	if header := strings.TrimSpace(r.Header.Get("X-LanChatGo-Access-Password")); header != "" &&
		subtle.ConstantTimeCompare([]byte(header), []byte(password)) == 1 {
		return false
	}
	cookie, err := r.Cookie(webAccessCookieName)
	if err == nil && cookie.Value != "" &&
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) == 1 {
		return false
	}
	return true
}

func (s *Server) serveWebAccessAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAuthJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
		return
	}
	if !sameWriteOrigin(r) {
		writeAuthJSON(w, http.StatusForbidden, map[string]any{"ok": false, "message": "来源校验失败"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var input struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeAuthJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请求格式无效"})
		return
	}
	s.mu.RLock()
	password, token := s.password, s.accessToken
	s.mu.RUnlock()
	if password == "" || subtle.ConstantTimeCompare([]byte(input.Password), []byte(password)) != 1 {
		writeAuthJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "message": "访问密码错误"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: webAccessCookieName, Value: token, Path: "/",
		HttpOnly: true, Secure: requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 60 * 60,
		Expires: time.Now().Add(30 * 24 * time.Hour),
	})
	writeAuthJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) renderWebAccessPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'self'")
	w.WriteHeader(http.StatusUnauthorized)
	returnTo, _ := json.Marshal(r.URL.RequestURI())
	page := strings.ReplaceAll(webAccessPageHTML, "__RETURN_TO__", string(returnTo))
	_, _ = w.Write([]byte(page))
}

const webAccessPageHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>访问验证 · LanChatGo</title><style>
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px;font-family:Inter,"Segoe UI","Microsoft YaHei",sans-serif;color:#172033;background:linear-gradient(145deg,#eef5ff,#f8fbff 48%,#edf9f5)}.shell{width:min(760px,100%);display:grid;grid-template-columns:1fr 1fr;overflow:hidden;border:1px solid #dce6f3;border-radius:26px;background:#fff;box-shadow:0 24px 70px rgba(38,76,125,.14)}.intro{padding:48px;background:linear-gradient(150deg,#1769e0,#3a83ee);color:#fff}.brand{display:flex;align-items:center;gap:13px}.brand img{width:54px;height:54px;border-radius:14px}.brand b{font-size:25px}.intro h1{font-size:30px;line-height:1.25;margin:75px 0 15px}.intro p{font-size:14px;line-height:1.8;opacity:.88}.panel{padding:46px 42px}.panel h2{margin:0 0 9px;font-size:25px}.sub{margin:0 0 26px;color:#8491a6;font-size:13px;line-height:1.6}.field{display:grid;gap:8px;margin:16px 0}.field label{font-size:13px;font-weight:700;color:#526178}.field input{width:100%;height:48px;padding:0 14px;border:1px solid #d5dfeb;border-radius:11px;outline:0;font-size:15px}.field input:focus{border-color:#3b82f6;box-shadow:0 0 0 3px #e8f1ff}.submit{width:100%;height:48px;margin-top:10px;border:0;border-radius:11px;background:#2675e8;color:#fff;font-size:15px;font-weight:800;cursor:pointer}.submit:disabled{opacity:.55}.message{min-height:22px;margin-top:15px;color:#d64242;font-size:13px;line-height:1.55}.message.ok{color:#16865c}@media(max-width:680px){.shell{grid-template-columns:1fr}.intro{display:none}.panel{padding:36px 27px}}</style></head><body><main class="shell"><section class="intro"><div class="brand"><img src="/__hfs/logo.png"><b>LanChatGo</b></div><h1>先验证，再进入局域网空间</h1><p>这是服务器的第一层访问保护。验证通过后，仍需使用你的 LanChatGo 账号登录。</p></section><section class="panel"><h2>访问验证</h2><p class="sub">请输入服务器管理员设置的 Web 访问密码。桌面客户端会使用编译时内置的密码自动验证。</p><form id="form"><div class="field"><label for="password">Web 访问密码</label><input id="password" type="password" autocomplete="current-password" autofocus required></div><button id="submit" class="submit">继续</button><div id="message" class="message"></div></form></section></main><script>
(()=>{const returnTo=__RETURN_TO__,q=id=>document.getElementById(id);async function submit(){const b=q("submit"),m=q("message"),p=q("password").value;b.disabled=true;m.className="message";m.textContent="正在验证…";try{const res=await fetch("/__auth/access",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({password:p})});const data=await res.json();if(!res.ok)throw new Error(data.message||"访问密码错误");m.className="message ok";m.textContent="验证成功，正在进入…";location.replace(returnTo&&returnTo.startsWith("/")?returnTo:"/")}catch(err){m.textContent=err.message||"验证失败"}finally{b.disabled=false}}q("form").onsubmit=e=>{e.preventDefault();submit()};async function embedded(){if(typeof window.clientGetAccessPassword!=="function")return;try{const p=await window.clientGetAccessPassword();if(p){q("password").value=p;await submit()}}catch(e){}}embedded()})();
</script></body></html>`
