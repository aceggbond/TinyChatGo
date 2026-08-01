package server

import (
	"archive/tar"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"tinychatgo/internal/appinfo"
)

func TestBrowseDownloadAndRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello world.txt"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(io.Discard)
	if err := s.Add(dir); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	assertBodyContains(t, ts.URL+"/", filepath.Base(dir))
	assertBodyContains(t, ts.URL+"/"+filepath.Base(dir), "hello world.txt")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/"+filepath.Base(dir)+"/hello%20world.txt", nil)
	req.Header.Set("Range", "bytes=2-5")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("range response = %d %q", res.StatusCode, body)
	}
}

func TestFilePanelJSONNavigationAndNoReloadClient(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "资料")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "说明.txt"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(io.Discard)
	if err := s.Add(dir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, false)

	rootRequest := httptest.NewRequest(http.MethodGet, "http://example.test/?__hfs_files=1", nil)
	rootResponse := httptest.NewRecorder()
	s.ServeHTTP(rootResponse, rootRequest)
	if rootResponse.Code != http.StatusOK {
		t.Fatalf("root file JSON status = %d: %s", rootResponse.Code, rootResponse.Body.String())
	}
	if contentType := rootResponse.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("root file JSON content type = %q", contentType)
	}
	var root fileListResponse
	if err := json.NewDecoder(rootResponse.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root.Path != "/" || len(root.Entries) != 1 || root.Entries[0].Name != filepath.Base(dir) || !root.Entries[0].Dir {
		t.Fatalf("unexpected root file JSON: %#v", root)
	}

	sharePath := root.Entries[0].URL
	dirRequest := httptest.NewRequest(http.MethodGet, "http://example.test"+sharePath+"?__hfs_files=1", nil)
	dirResponse := httptest.NewRecorder()
	s.ServeHTTP(dirResponse, dirRequest)
	var listing fileListResponse
	if err := json.NewDecoder(dirResponse.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if dirResponse.Code != http.StatusOK || listing.Path != sharePath || listing.Parent != "/" || !listing.Upload {
		t.Fatalf("unexpected directory file JSON: status=%d body=%#v", dirResponse.Code, listing)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "资料" || !listing.Entries[0].Dir {
		t.Fatalf("unexpected directory entries: %#v", listing.Entries)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "http://example.test"+sharePath, nil)
	pageResponse := httptest.NewRecorder()
	s.ServeHTTP(pageResponse, pageRequest)
	page := pageResponse.Body.String()
	for _, marker := range []string{
		`file-toggle-label">展开文件`,
		`fetch(fileDataURL(path,query)`,
		`event.preventDefault();loadFileList`,
		`loadFileList(fileCurrentPath,fileSearchInput.value.trim(),false)`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("file panel client missing %q", marker)
		}
	}
	if strings.Contains(page, "location.reload()") {
		t.Fatal("file panel client still performs a full page reload")
	}
}

func TestRemoveManyShares(t *testing.T) {
	s := New(io.Discard)
	s.shares = []Share{
		{Name: "A", Path: "a"},
		{Name: "B", Path: "b"},
		{Name: "C", Path: "c"},
		{Name: "D", Path: "d"},
		{Name: "E", Path: "e"},
	}

	if removed := s.RemoveMany([]int{3, 1, 3, -1, 99}); removed != 2 {
		t.Fatalf("removed %d shares, want 2", removed)
	}
	got := s.Shares()
	if len(got) != 3 || got[0].Name != "A" || got[1].Name != "C" || got[2].Name != "E" {
		t.Fatalf("remaining shares = %#v", got)
	}
	if removed := s.RemoveMany(nil); removed != 0 {
		t.Fatalf("empty removal removed %d shares", removed)
	}
	s.Remove(1)
	got = s.Shares()
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "E" {
		t.Fatalf("single removal remaining shares = %#v", got)
	}
}

func TestPageVisitorNotifierDeduplicatesIPAddresses(t *testing.T) {
	s := New(io.Discard)
	var notifications atomic.Int32
	s.SetVisitorNotifier(func(info ChatClientInfo) {
		if info.IP == "" || info.Port == "" || info.ConnectedAt.IsZero() {
			t.Errorf("visitor info = %#v", info)
		}
		notifications.Add(1)
	})
	ts := httptest.NewServer(s)
	defer ts.Close()

	newClient := func() *http.Client {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Client{Jar: jar}
	}
	get := func(client *http.Client, target string) {
		response, err := client.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}

	first := newClient()
	get(first, ts.URL+"/")
	if got := notifications.Load(); got != 1 {
		t.Fatalf("first page visit notifications = %d", got)
	}
	get(first, ts.URL+"/?q=again")
	if got := notifications.Load(); got != 1 {
		t.Fatalf("repeat page visit notifications = %d", got)
	}

	second := newClient()
	get(second, ts.URL+"/__hfs/chat/status")
	if got := notifications.Load(); got != 1 {
		t.Fatalf("status polling triggered notification, count = %d", got)
	}
	get(second, ts.URL+"/")
	if got := notifications.Load(); got != 1 {
		t.Fatalf("same IP with a new browser session triggered notification, count = %d", got)
	}

	request, _ := http.NewRequest(http.MethodHead, ts.URL+"/", nil)
	response, err := newClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	get(newClient(), ts.URL+"/missing")
	if got := notifications.Load(); got != 1 {
		t.Fatalf("HEAD/404 triggered visitor notification, count = %d", got)
	}

	var simultaneous sync.WaitGroup
	for i := 0; i < 12; i++ {
		simultaneous.Add(1)
		go func(index int) {
			defer simultaneous.Done()
			request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
			request.RemoteAddr = fmt.Sprintf("192.0.2.44:%d", 41000+index)
			request.AddCookie(&http.Cookie{Name: chatCookieName, Value: strings.Repeat("f", 32)})
			s.ServeHTTP(httptest.NewRecorder(), request)
		}(i)
	}
	simultaneous.Wait()
	if got := notifications.Load(); got != 2 {
		t.Fatalf("concurrent requests for one IP notifications = %d", got)
	}

	otherIP := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	otherIP.RemoteAddr = "192.0.2.45:42000"
	s.ServeHTTP(httptest.NewRecorder(), otherIP)
	if got := notifications.Load(); got != 3 {
		t.Fatalf("new IP notifications = %d", got)
	}

	s.resetVisitorSessions()
	get(first, ts.URL+"/")
	if got := notifications.Load(); got != 4 {
		t.Fatalf("new server generation notifications = %d", got)
	}
}

func TestFileOperationLogsUseIPIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "download.txt"), []byte("download"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := New(&logs)
	if err := s.Add(dir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, true)
	shareURL := "http://example.test/" + filepath.Base(dir)

	download := httptest.NewRequest(http.MethodGet, shareURL+"/download.txt", nil)
	download.RemoteAddr = "198.51.100.20:45123"
	downloadRecorder := httptest.NewRecorder()
	s.ServeHTTP(downloadRecorder, download)
	if downloadRecorder.Code != http.StatusOK {
		t.Fatalf("download status = %d", downloadRecorder.Code)
	}

	var uploadBody bytes.Buffer
	writer := multipart.NewWriter(&uploadBody)
	file, err := writer.CreateFormFile("files", "upload.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("upload")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(http.MethodPost, shareURL, &uploadBody)
	upload.RemoteAddr = "203.0.113.44:46234"
	upload.Header.Set("Content-Type", writer.FormDataContentType())
	uploadRecorder := httptest.NewRecorder()
	s.ServeHTTP(uploadRecorder, upload)
	if uploadRecorder.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d", uploadRecorder.Code)
	}

	output := logs.String()
	for _, marker := range []string{
		`IP=198.51.100.20 操作=下载文件`,
		`IP=203.0.113.44 操作=上传文件`,
	} {
		if !strings.Contains(output, marker) {
			t.Fatalf("operation log missing %q:\n%s", marker, output)
		}
	}
	if strings.Contains(output, "198.51.100.20:45123") || strings.Contains(output, "203.0.113.44:46234") {
		t.Fatalf("operation log used connection port as visitor identity:\n%s", output)
	}
}

func TestRequestIsMultipartIgnoresMediaTypeCase(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://example.test/share", nil)
	request.Header.Set("Content-Type", `Multipart/Form-Data; Boundary="example"`)
	if !requestIsMultipart(request) {
		t.Fatal("mixed-case multipart/form-data was not recognized as an upload")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if requestIsMultipart(request) {
		t.Fatal("form management request was misclassified as an upload")
	}
}

func TestWriteOperationsRejectCrossOriginBrowserRequests(t *testing.T) {
	dir := t.TempDir()
	s := New(io.Discard)
	if err := s.Add(dir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, true)
	targetURL := "http://hfs.test/" + filepath.Base(dir)
	body := "action=mkdir&name=blocked"
	request := httptest.NewRequest(http.MethodPost, targetURL, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.test")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin write status = %d", response.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "blocked")); !os.IsNotExist(err) {
		t.Fatalf("cross-origin write changed the share: %v", err)
	}

	request = httptest.NewRequest(http.MethodPost, targetURL, strings.NewReader("action=mkdir&name=allowed"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://hfs.test")
	response = httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("same-origin write status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "allowed")); err != nil {
		t.Fatalf("same-origin write failed: %v", err)
	}
}

func TestSearchArchiveAndManage(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("a"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "beta.txt"), []byte("b"), 0600)
	s := New(io.Discard)
	_ = s.Add(dir)
	s.SetAccess("", false, true, true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	base := ts.URL + "/" + filepath.Base(dir)

	res, _ := http.Get(base + "?q=alpha")
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if !strings.Contains(string(body), "alpha.txt") || strings.Contains(string(body), "beta.txt") {
		t.Fatalf("bad search result")
	}

	res, _ = http.Get(base + "?archive=1")
	tr := tar.NewReader(res.Body)
	h, err := tr.Next()
	_ = res.Body.Close()
	if err != nil || h.Name == "" {
		t.Fatalf("archive invalid: %v", err)
	}

	form := strings.NewReader("action=mkdir&name=new-folder")
	req, _ := http.NewRequest(http.MethodPost, base, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if _, err = os.Stat(filepath.Join(dir, "new-folder")); err != nil {
		t.Fatal("mkdir failed", err)
	}

	form = strings.NewReader("action=rename&name=new-folder&new_name=renamed-folder")
	req, _ = http.NewRequest(http.MethodPost, base, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if _, err = os.Stat(filepath.Join(dir, "renamed-folder")); err != nil {
		t.Fatal("rename failed", err)
	}

	form = strings.NewReader("action=delete&name=renamed-folder")
	req, _ = http.NewRequest(http.MethodPost, base, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if _, err = os.Stat(filepath.Join(dir, "renamed-folder")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("delete failed")
	}
}

func TestPortOccupiedAndRepeatedStartStop(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(io.Discard)
	if _, err = s.Start(occupied.Addr().String()); err == nil {
		t.Fatal("expected occupied port error")
	}
	_ = occupied.Close()
	for i := 0; i < 20; i++ {
		if _, err = s.Start("127.0.0.1:0"); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if err = s.Stop(); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
	}
}

func TestRejectsUnknownShare(t *testing.T) {
	s := New(io.Discard)
	r := httptest.NewRequest(http.MethodGet, "/not-shared/file", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestHTTPRedirectToConfiguredHTTPSAddress(t *testing.T) {
	s := New(io.Discard)
	s.SetHTTPSRedirect(true, "192.0.2.18", "1443")

	request := httptest.NewRequest(http.MethodPost, "http://attacker.invalid/shared?q=one", strings.NewReader("body"))
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("redirect status = %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "https://192.0.2.18:1443/shared?q=one" {
		t.Fatalf("redirect location = %q", location)
	}

	request = httptest.NewRequest(http.MethodGet, "https://192.0.2.18:1443/", nil)
	request.TLS = &tls.ConnectionState{}
	response = httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code == http.StatusTemporaryRedirect {
		t.Fatal("HTTPS request was redirected again")
	}

	s.SetHTTPSRedirect(false, "192.0.2.18", "1443")
	request = httptest.NewRequest(http.MethodGet, "http://192.0.2.18:1122/", nil)
	response = httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code == http.StatusTemporaryRedirect {
		t.Fatal("disabled HTTP redirect was still active")
	}
}

func TestSharedDirectoryRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "outside-link.txt")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	s := New(io.Discard)
	if err := s.Add(root); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://hfs.test/"+filepath.Base(root)+"/outside-link.txt", nil)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("symlink escape status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestPasswordAndUploadControls(t *testing.T) {
	dir := t.TempDir()
	s := New(io.Discard)
	if err := s.Add(dir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("secret", true, false, true)
	ts := httptest.NewServer(s)
	defer ts.Close()

	res, _ := http.Get(ts.URL + "/")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", res.StatusCode)
	}
	_ = res.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/"+filepath.Base(dir), nil)
	req.SetBasicAuth("hfs", "secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	for _, marker := range []string{`id="upload-drop"`, `id="upload-progress"`, `X-HFS-Upload`, `request.upload.onprogress`} {
		if !strings.Contains(string(pageBody), marker) {
			t.Fatalf("drag upload page is missing %q", marker)
		}
	}
	if strings.Contains(string(pageBody), `>上传到此目录</button>`) {
		t.Fatal("drag upload page still renders a separate upload button")
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("files", "uploaded.txt")
	_, _ = fw.Write([]byte("uploaded"))
	_ = mw.Close()
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/"+filepath.Base(dir), &body)
	req.SetBasicAuth("hfs", "secret")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload status = %d", res.StatusCode)
	}
	data, err := os.ReadFile(filepath.Join(dir, "uploaded.txt"))
	if err != nil || string(data) != "uploaded" {
		t.Fatalf("uploaded data = %q, %v", data, err)
	}

	body.Reset()
	mw = multipart.NewWriter(&body)
	for name, content := range map[string]string{"progress-a.txt": "a", "progress-b.txt": "b"} {
		fw, _ = mw.CreateFormFile("files", name)
		_, _ = fw.Write([]byte(content))
	}
	_ = mw.Close()
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/"+filepath.Base(dir), &body)
	req.SetBasicAuth("hfs", "secret")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-HFS-Upload", "1")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var uploadResult struct {
		Uploaded int `json:"uploaded"`
	}
	decodeErr := json.NewDecoder(res.Body).Decode(&uploadResult)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusCreated || decodeErr != nil || uploadResult.Uploaded != 2 {
		t.Fatalf("progress upload = status %d, result %#v, decode %v", res.StatusCode, uploadResult, decodeErr)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/"+filepath.Base(dir)+"/uploaded.txt", nil)
	req.SetBasicAuth("hfs", "secret")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("download status = %d", res.StatusCode)
	}
}

func TestBrowserPageHidesManagementAndIncludesImageViewer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(io.Discard)
	if err := s.Add(dir); err != nil {
		t.Fatal(err)
	}
	// Even if an older caller still passes manage=true, the browser template no
	// longer exposes create/rename/delete controls.
	s.SetAccess("", false, true, true)
	request := httptest.NewRequest(http.MethodGet, "http://hfs.test/"+filepath.Base(dir), nil)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	body := response.Body.String()
	if version := response.Header().Get("X-HFS-Go-Version"); version != appinfo.Version {
		t.Fatalf("server version header = %q", version)
	}
	if !strings.Contains(body, `<span class="brand-version">v`+appinfo.Version+`</span>`) {
		t.Fatalf("browser page is missing visible server version v%s", appinfo.Version)
	}
	for _, removed := range []string{"新建目录", `value="rename"`, `value="delete"`} {
		if strings.Contains(body, removed) {
			t.Fatalf("browser management control remains: %q", removed)
		}
	}
	for _, marker := range []string{`id="image-viewer"`, `id="image-viewer-prev"`, `id="image-viewer-next"`, `openImageViewer(image.src)`, `event.key==='ArrowDown'`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("browser image viewer is missing %q", marker)
		}
	}
}

func assertBodyContains(t *testing.T, u, want string) {
	t.Helper()
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
		t.Fatalf("GET %s = %d, missing %q", u, res.StatusCode, want)
	}
}
