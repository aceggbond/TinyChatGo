package server

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("files", "uploaded.txt")
	_, _ = fw.Write([]byte("uploaded"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/"+filepath.Base(dir), &body)
	req.SetBasicAuth("hfs", "secret")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
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
