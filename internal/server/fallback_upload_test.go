package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFallbackRootUploadCreatesIndividualShares(t *testing.T) {
	uploadDir := t.TempDir()
	s := New(io.Discard)
	notifications := 0
	s.SetShareChangeNotifier(func() { notifications++ })
	if err := s.SetFallbackUploadDir(uploadDir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, false)

	root := httptest.NewRecorder()
	s.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), `id="upload"`) {
		t.Fatalf("root upload area missing: status=%d", root.Code)
	}

	for index, content := range []string{"first", "second"} {
		request := fallbackUploadRequest(t, "report.txt", content)
		request.Header.Set("X-HFS-Upload", "1")
		response := httptest.NewRecorder()
		s.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %d status = %d: %s", index, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"uploaded":1`) {
			t.Fatalf("upload %d response = %s", index, response.Body.String())
		}
	}

	shares := s.Shares()
	if notifications != 2 {
		t.Fatalf("share change notifications = %d, want 2", notifications)
	}
	if len(shares) != 2 {
		t.Fatalf("shares = %+v", shares)
	}
	wantNames := []string{"report.txt", "report (2).txt"}
	for index, share := range shares {
		if !share.ManagedTemporary {
			t.Fatalf("fallback share %d is missing its managed-temporary marker", index)
		}
		if share.Path == uploadDir {
			t.Fatal("fallback directory itself was shared")
		}
		if filepath.Dir(share.Path) != uploadDir {
			t.Fatalf("share escaped fallback directory: %+v", share)
		}
		if filepath.Base(share.Path) != wantNames[index] {
			t.Fatalf("share %d path = %q", index, share.Path)
		}
		content, err := os.ReadFile(share.Path)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != []string{"first", "second"}[index] {
			t.Fatalf("share %d content = %q", index, content)
		}
	}
	configPath := filepath.Join(t.TempDir(), "shares.json")
	if err := s.Save(configPath); err != nil {
		t.Fatal(err)
	}
	reloaded := New(io.Discard)
	if err := reloaded.Load(configPath); err != nil {
		t.Fatal(err)
	}
	for index, share := range reloaded.Shares() {
		if !share.ManagedTemporary {
			t.Fatalf("reloaded fallback share %d lost its managed-temporary marker", index)
		}
	}
}

func TestFallbackRootUploadRejectsOtherPostsAndDisabledUpload(t *testing.T) {
	uploadDir := t.TempDir()
	s := New(io.Discard)
	if err := s.SetFallbackUploadDir(uploadDir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, true)

	formRequest := httptest.NewRequest(http.MethodPost, "http://example.test/", strings.NewReader("action=mkdir&name=nope"))
	formRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	formResponse := httptest.NewRecorder()
	s.ServeHTTP(formResponse, formRequest)
	if formResponse.Code != http.StatusMethodNotAllowed {
		t.Fatalf("non-upload root POST status = %d", formResponse.Code)
	}
	if len(s.Shares()) != 0 {
		t.Fatal("non-upload root POST changed shares")
	}

	s.SetAccess("", false, true, true)
	uploadResponse := httptest.NewRecorder()
	s.ServeHTTP(uploadResponse, fallbackUploadRequest(t, "blocked.txt", "blocked"))
	if uploadResponse.Code != http.StatusForbidden {
		t.Fatalf("disabled upload status = %d", uploadResponse.Code)
	}
	root := httptest.NewRecorder()
	s.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	rootBody := root.Body.String()
	if !strings.Contains(rootBody, `id="upload"`) || !strings.Contains(rootBody, `enctype="multipart/form-data" hidden`) {
		t.Fatal("disabled root upload area was not kept hidden for client-side directory navigation")
	}
	if _, err := os.Stat(filepath.Join(uploadDir, "blocked.txt")); !os.IsNotExist(err) {
		t.Fatalf("disabled upload wrote a file: %v", err)
	}
}

func TestFallbackRootUploadReportsUnavailableDirectory(t *testing.T) {
	uploadDir := t.TempDir()
	s := New(io.Discard)
	if err := s.SetFallbackUploadDir(uploadDir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, false)
	if err := os.Remove(uploadDir); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	s.ServeHTTP(response, fallbackUploadRequest(t, "file.txt", "content"))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "上传目录不可写或不可用") {
		t.Fatalf("unclear error: %s", response.Body.String())
	}
}

func TestSetFallbackUploadDirRejectsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := New(io.Discard).SetFallbackUploadDir(file); err == nil {
		t.Fatal("expected a non-directory error")
	}
}

func TestFallbackUploadCancellationRollsBackFileAndShare(t *testing.T) {
	uploadDir := t.TempDir()
	s := New(io.Discard)
	if err := s.SetFallbackUploadDir(uploadDir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, false)
	request := fallbackUploadRequest(t, "cancelled.txt", strings.Repeat("x", 256<<10))
	originalBody := request.Body
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	request.Body = &cancelOnFirstReadCloser{ReadCloser: originalBody, cancel: cancel}
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("cancelled upload status = %d", response.Code)
	}
	if shares := s.Shares(); len(shares) != 0 {
		t.Fatalf("cancelled upload created shares: %#v", shares)
	}
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled upload left files: %#v", entries)
	}
}

type cancelOnFirstReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelOnFirstReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.ReadCloser.Read(buffer)
	if count > 0 {
		r.once.Do(r.cancel)
	}
	return count, err
}

func TestOldRunRequestCannotCrossRestartHandlerGate(t *testing.T) {
	s := New(io.Discard)
	address, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.lifecycleMu.Lock()
	oldContext := s.http.BaseContext(s.ln)
	s.lifecycleMu.Unlock()
	if err = s.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	request := httptest.NewRequest(http.MethodGet, "http://"+address+"/", nil).WithContext(oldContext)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("old run request status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestStopWaitsForFallbackUploadHandlerAfterForcedClose(t *testing.T) {
	originalTimeout := gracefulShutdownTimeout
	gracefulShutdownTimeout = 50 * time.Millisecond
	defer func() { gracefulShutdownTimeout = originalTimeout }()

	uploadDir := t.TempDir()
	s := New(io.Discard)
	if err := s.SetFallbackUploadDir(uploadDir); err != nil {
		t.Fatal(err)
	}
	s.SetAccess("", true, true, false)
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	s.SetShareChangeNotifier(func() {
		close(handlerEntered)
		<-releaseHandler
	})
	address, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	request := fallbackUploadRequest(t, "wait.txt", "content")
	request.URL.Scheme = "http"
	request.URL.Host = address
	request.Host = address
	request.RequestURI = ""
	requestResult := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- requestErr
	}()
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("fallback upload handler did not reach persistence callback")
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- s.Stop() }()
	select {
	case err = <-stopResult:
		t.Fatalf("Stop returned while an upload handler was still active: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseHandler)
	select {
	case err = <-stopResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not finish after the upload handler exited")
	}
	select {
	case <-requestResult:
	case <-time.After(2 * time.Second):
		t.Fatal("upload client did not finish after forced shutdown")
	}
}

func fallbackUploadRequest(t *testing.T, name, content string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://example.test/", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}
