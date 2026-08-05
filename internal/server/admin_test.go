package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSeparateAdminLoginAndStatus(t *testing.T) {
	s := New(&bytes.Buffer{})

	disabled := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, "http://example.test/admin/", nil))
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("disabled admin status = %d", disabled.Code)
	}

	s.SetAdminPassword("admin-secret-password")
	public := httptest.NewRecorder()
	s.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "http://example.test/admin/", nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("public server exposed admin page: status = %d", public.Code)
	}
	page := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://example.test/admin/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "TCGS 管理后台") || !strings.Contains(page.Body.String(), "群聊管理") || !strings.Contains(page.Body.String(), "归档管理") {
		t.Fatalf("admin page = %d, %q", page.Code, page.Body.String())
	}

	loginRequest := httptest.NewRequest(http.MethodPost, "http://example.test/__admin/login", strings.NewReader(`{"password":"admin-secret-password"}`))
	loginRequest.Header.Set("Origin", "http://example.test")
	login := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login = %d, cookies=%d, body=%q", login.Code, len(login.Result().Cookies()), login.Body.String())
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "http://example.test/__admin/status", nil)
	statusRequest.AddCookie(login.Result().Cookies()[0])
	status := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(status, statusRequest)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"accounts":0`) {
		t.Fatalf("status = %d, %q", status.Code, status.Body.String())
	}

	settingsRequest := httptest.NewRequest(http.MethodPost, "http://example.test/__admin/settings", strings.NewReader(`{"requireApproval":true,"allowGroups":true,"showUsers":true,"privateChat":true}`))
	settingsRequest.Header.Set("Origin", "http://example.test")
	settingsRequest.AddCookie(login.Result().Cookies()[0])
	settings := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(settings, settingsRequest)
	if settings.Code != http.StatusOK || !s.AccountApprovalRequired() || !s.UserGroupCreationEnabled() || !s.UserListEnabled() || !s.PrivateMessagesEnabled() {
		t.Fatalf("settings = %d, %q", settings.Code, settings.Body.String())
	}
}
