package server

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Share struct{ Name, Path string }

type Server struct {
	mu            sync.RWMutex
	shares        []Share
	http          *http.Server
	ln            net.Listener
	logger        *log.Logger
	password      string
	allowUpload   bool
	allowDownload bool
	allowManage   bool
}

func New(logWriter io.Writer) *Server {
	return &Server{logger: log.New(logWriter, "", log.LstdFlags), allowDownload: true}
}

func (s *Server) SetAccess(password string, upload, download, manage bool) {
	s.mu.Lock()
	s.password, s.allowUpload, s.allowDownload, s.allowManage = password, upload, download, manage
	s.mu.Unlock()
}

func (s *Server) Shares() []Share {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Share(nil), s.shares...)
}

func (s *Server) Add(realPath string) error {
	abs, err := filepath.Abs(realPath)
	if err != nil {
		return err
	}
	if _, err = os.Stat(abs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := filepath.Base(abs)
	base := name
	for n := 2; s.nameExists(name); n++ {
		name = fmt.Sprintf("%s (%d)", base, n)
	}
	s.shares = append(s.shares, Share{Name: name, Path: abs})
	return nil
}

func (s *Server) nameExists(name string) bool {
	for _, x := range s.shares {
		if strings.EqualFold(x.Name, name) {
			return true
		}
	}
	return false
}

func (s *Server) Remove(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= 0 && i < len(s.shares) {
		s.shares = append(s.shares[:i], s.shares[i+1:]...)
	}
}

func (s *Server) Rename(i int, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return errors.New("无效的虚拟名称")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.shares) {
		return errors.New("项目不存在")
	}
	for n, x := range s.shares {
		if n != i && strings.EqualFold(x.Name, name) {
			return errors.New("名称已存在")
		}
	}
	s.shares[i].Name = name
	return nil
}

func (s *Server) Save(filename string) error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.shares, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0600)
}

func (s *Server) Load(filename string) error {
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []Share
	if err = json.Unmarshal(data, &items); err != nil {
		return err
	}
	valid := items[:0]
	for _, x := range items {
		if _, e := os.Stat(x.Path); e == nil {
			valid = append(valid, x)
		}
	}
	s.mu.Lock()
	s.shares = valid
	s.mu.Unlock()
	return nil
}

func (s *Server) Start(addr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.http != nil {
		return s.ln.Addr().String(), nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	s.ln = ln
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func(h *http.Server) {
		if e := h.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			s.logger.Printf("服务器错误: %v", e)
		}
	}(s.http)
	port := ln.Addr().(*net.TCPAddr).Port
	s.logger.Printf("服务器已启动，端口 %d", port)
	return ln.Addr().String(), nil
}

func (s *Server) Stop() error {
	s.mu.Lock()
	h := s.http
	s.http = nil
	s.ln = nil
	s.mu.Unlock()
	if h == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := h.Shutdown(ctx)
	s.logger.Print("服务器已停止")
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := 200
	lw := &statusWriter{ResponseWriter: w, status: &status}
	defer func() {
		s.logger.Printf("%s %s %s %d %s", r.RemoteAddr, r.Method, r.URL.Path, status, time.Since(start).Round(time.Millisecond))
	}()
	s.mu.RLock()
	password, upload, download, manage := s.password, s.allowUpload, s.allowDownload, s.allowManage
	s.mu.RUnlock()
	if password != "" {
		_, pass, ok := r.BasicAuth()
		if !ok || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="HFS Go"`)
			http.Error(lw, "需要访问密码", http.StatusUnauthorized)
			return
		}
	}
	if r.Method == http.MethodPost && !upload {
		if !manage {
			http.Error(lw, "写入功能未启用", http.StatusForbidden)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		http.Error(lw, "method not allowed", 405)
		return
	}
	clean, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil {
		http.Error(lw, "bad path", 400)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path.Clean("/"+clean), "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		s.renderRoot(lw, r)
		return
	}
	s.mu.RLock()
	var sh *Share
	for _, x := range s.shares {
		if x.Name == parts[0] {
			y := x
			sh = &y
			break
		}
	}
	s.mu.RUnlock()
	if sh == nil {
		http.NotFound(lw, r)
		return
	}
	base, _ := filepath.Abs(sh.Path)
	target := base
	if len(parts) > 1 {
		target = filepath.Join(append([]string{base}, parts[1:]...)...)
	}
	resolved, _ := filepath.Abs(target)
	if resolved != base && !strings.HasPrefix(strings.ToLower(resolved), strings.ToLower(base)+string(os.PathSeparator)) {
		http.Error(lw, "forbidden", 403)
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		http.NotFound(lw, r)
		return
	}
	if r.Method == http.MethodPost {
		if !info.IsDir() {
			http.Error(lw, "只能上传到目录", http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if !upload {
				http.Error(lw, "上传功能未启用", http.StatusForbidden)
				return
			}
			s.handleUpload(lw, r, resolved)
		} else {
			if !manage {
				http.Error(lw, "管理功能未启用", http.StatusForbidden)
				return
			}
			s.handleManage(lw, r, resolved)
		}
		return
	}
	if info.IsDir() {
		if r.URL.Query().Get("archive") == "1" {
			if !download {
				http.Error(lw, "下载功能已关闭", http.StatusForbidden)
				return
			}
			s.serveArchive(lw, resolved, info.Name())
			return
		}
		s.renderDir(lw, r, resolved, info.Name())
		return
	}
	if !download {
		http.Error(lw, "下载功能已关闭", http.StatusForbidden)
		return
	}
	disposition := "attachment"
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(info.Name())))
	if strings.HasPrefix(mediaType, "image/") && mediaType != "image/svg+xml" || strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/") || mediaType == "application/pdf" || mediaType == "text/plain; charset=utf-8" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, url.PathEscape(info.Name())))
	http.ServeFile(lw, r, resolved)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, dir string) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "上传请求无效", http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "没有选择文件", http.StatusBadRequest)
		return
	}
	for _, header := range files {
		name := filepath.Base(filepath.ToSlash(header.Filename))
		if !validLeafName(name) {
			continue
		}
		src, err := header.Open()
		if err != nil {
			http.Error(w, "无法读取上传内容", 500)
			return
		}
		target := uniquePath(dir, name)
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err == nil {
			_, err = io.Copy(dst, src)
			_ = dst.Close()
		}
		_ = src.Close()
		if err != nil {
			http.Error(w, "保存上传文件失败", 500)
			return
		}
	}
	http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
}

func uniquePath(dir, name string) string {
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return target
	}
	ext, base := filepath.Ext(name), strings.TrimSuffix(name, filepath.Ext(name))
	for i := 2; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func (s *Server) handleManage(w http.ResponseWriter, r *http.Request, dir string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求无效", 400)
		return
	}
	switch r.Form.Get("action") {
	case "mkdir":
		name := filepath.Base(strings.TrimSpace(r.Form.Get("name")))
		if !validLeafName(name) {
			http.Error(w, "目录名称无效", 400)
			return
		}
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
			http.Error(w, "创建目录失败", 400)
			return
		}
	case "delete":
		name := filepath.Base(r.Form.Get("name"))
		if !validLeafName(name) {
			http.Error(w, "名称无效", 400)
			return
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			http.Error(w, "删除失败", 500)
			return
		}
	case "rename":
		oldName, newName := filepath.Base(r.Form.Get("name")), strings.TrimSpace(r.Form.Get("new_name"))
		if !validLeafName(oldName) || !validLeafName(newName) {
			http.Error(w, "名称无效", 400)
			return
		}
		oldPath, newPath := filepath.Join(dir, oldName), filepath.Join(dir, newName)
		if _, err := os.Stat(newPath); err == nil {
			http.Error(w, "目标名称已存在", http.StatusConflict)
			return
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			http.Error(w, "重命名失败", 500)
			return
		}
	default:
		http.Error(w, "未知操作", 400)
		return
	}
	http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
}

func validLeafName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

func (s *Server) serveArchive(w http.ResponseWriter, dir, name string) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s.tar", url.PathEscape(name)))
	tw := tar.NewWriter(w)
	defer tw.Close()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil
		}
		h.Name = filepath.ToSlash(rel)
		if err = tw.WriteHeader(h); err != nil || info.IsDir() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

type statusWriter struct {
	http.ResponseWriter
	status *int
}

func (w *statusWriter) WriteHeader(n int) { *w.status = n; w.ResponseWriter.WriteHeader(n) }

type entry struct {
	Name, URL, Size, Modified string
	Dir                       bool
}
type pageData struct {
	Title, Parent string
	Entries       []entry
	Upload        bool
	Manage        bool
	Query         string
	ArchiveURL    string
	CanUpload     bool
	UploadHint    string
}

func (s *Server) renderRoot(w http.ResponseWriter, r *http.Request) {
	items := s.Shares()
	es := make([]entry, 0, len(items))
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	for _, x := range items {
		if query != "" && !strings.Contains(strings.ToLower(x.Name), query) {
			continue
		}
		st, e := os.Stat(x.Path)
		if e == nil {
			es = append(es, entry{Name: x.Name, URL: "/" + url.PathEscape(x.Name), Size: formatSize(st.Size()), Modified: st.ModTime().Format("2006-01-02 15:04"), Dir: st.IsDir()})
		}
	}
	s.mu.RLock()
	upload := s.allowUpload
	s.mu.RUnlock()
	hint := ""
	if upload {
		hint = "上传已开启：请点击共享文件夹右侧的“上传”，文件不能作为上传目标。"
	}
	s.render(w, pageData{Title: "HFS Go - 文件分享", Entries: es, Query: r.URL.Query().Get("q"), CanUpload: upload, UploadHint: hint})
}
func (s *Server) renderDir(w http.ResponseWriter, r *http.Request, dir, title string) {
	list, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, "cannot read directory", 403)
		return
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].IsDir() != list[j].IsDir() {
			return list[i].IsDir()
		}
		return strings.ToLower(list[i].Name()) < strings.ToLower(list[j].Name())
	})
	es := make([]entry, 0, len(list))
	base := strings.TrimSuffix(r.URL.Path, "/")
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	for _, x := range list {
		if query != "" && !strings.Contains(strings.ToLower(x.Name()), query) {
			continue
		}
		st, e := x.Info()
		if e != nil {
			continue
		}
		es = append(es, entry{Name: x.Name(), URL: base + "/" + url.PathEscape(x.Name()), Size: formatSize(st.Size()), Modified: st.ModTime().Format("2006-01-02 15:04"), Dir: x.IsDir()})
	}
	parent := path.Dir(base)
	if parent == "." {
		parent = "/"
	}
	s.mu.RLock()
	upload, manage := s.allowUpload, s.allowManage
	s.mu.RUnlock()
	s.render(w, pageData{Title: title, Parent: parent, Entries: es, Upload: upload, CanUpload: upload, Manage: manage, Query: r.URL.Query().Get("q"), ArchiveURL: base + "?archive=1"})
}
func (s *Server) render(w http.ResponseWriter, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, d); err != nil {
		s.logger.Print(err)
	}
}
func formatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1<<20 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	if n < 1<<30 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>{{.Title}}</title><style>
body{font:15px system-ui;margin:0;background:#f4f6f8;color:#253044}.top{background:linear-gradient(135deg,#1769aa,#2385d1);color:white;padding:22px max(5vw,20px)}main{max-width:1000px;margin:24px auto;background:white;border-radius:12px;box-shadow:0 3px 18px #0001;overflow:hidden}a{color:#1769aa;text-decoration:none}.tools,.upload{display:flex;gap:10px;align-items:center;padding:14px 20px;background:#f7fbff;border-bottom:1px solid #e5edf4}.notice{padding:12px 20px;background:#fff8dd;color:#735c16;border-bottom:1px solid #f1e3a6}.tools form{display:flex;gap:8px}.tools input{padding:8px;border:1px solid #ccd6e0;border-radius:6px}.btn,button{background:#1769aa;color:white;border:0;border-radius:6px;padding:9px 14px;cursor:pointer}.row{display:grid;grid-template-columns:1fr 100px 150px auto;gap:10px;padding:13px 20px;border-bottom:1px solid #edf0f2;align-items:center}.row:hover{background:#f7fbff}.meta{color:#748092;text-align:right}.danger{background:none;color:#c43b3b;padding:4px 8px}@media(max-width:600px){.tools{flex-wrap:wrap}.row{grid-template-columns:1fr}.meta{text-align:left;font-size:12px}}
</style></head><body><div class="top"><h1>HFS Go</h1><div>{{.Title}}</div></div><main>
<div class="tools"><form method="get"><input name="q" value="{{.Query}}" placeholder="搜索当前目录"><button>搜索</button></form>{{if .ArchiveURL}}<a class="btn" href="{{.ArchiveURL}}">打包下载</a>{{end}}{{if .Manage}}<form method="post"><input type="hidden" name="action" value="mkdir"><input name="name" placeholder="新文件夹名称" required><button>新建目录</button></form>{{end}}</div>
{{if .UploadHint}}<div class="notice">{{.UploadHint}}</div>{{end}}
{{if .Upload}}<form id="upload" class="upload" method="post" enctype="multipart/form-data"><strong>上传文件</strong><input name="files" type="file" multiple required><button>上传到此目录</button><small>同名文件会自动编号，不覆盖原文件</small></form>{{end}}
{{if .Parent}}<a class="row" href="{{.Parent}}"><span>📁 ..</span></a>{{end}}
{{range .Entries}}<div class="row"><a href="{{.URL}}">{{if .Dir}}📁{{else}}📄{{end}} {{.Name}}</a><span class="meta">{{if not .Dir}}{{.Size}}{{end}}</span><span class="meta">{{.Modified}}</span><span>{{if and $.CanUpload .Dir}}<a class="btn" href="{{.URL}}#upload">上传</a>{{end}}{{if $.Manage}}<form style="display:inline" method="post" onsubmit="var n=prompt('输入新名称','{{.Name}}');if(!n)return false;this.new_name.value=n"><input type="hidden" name="action" value="rename"><input type="hidden" name="name" value="{{.Name}}"><input type="hidden" name="new_name"><button class="danger">重命名</button></form><form style="display:inline" method="post" onsubmit="return confirm('确定删除？')"><input type="hidden" name="action" value="delete"><input type="hidden" name="name" value="{{.Name}}"><button class="danger">删除</button></form>{{end}}</span></div>{{else}}<div class="row">此处为空</div>{{end}}</main></body></html>`))
