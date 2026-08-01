package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"tinychatgo/internal/database"
	"tinychatgo/internal/server"
)

func TestRegisteredAccountApprovalLoginAndPersistence(t *testing.T) {
	store, err := database.Open(filepath.Join(t.TempDir(), "tinychatgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := server.New(io.Discard)
	if err = s.SetPersistence(store); err != nil {
		t.Fatal(err)
	}
	s.SetAccountApprovalRequired(true)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()

	response, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("登录 · TinyChatGo")) {
		t.Fatalf("anonymous root = %d %q", response.StatusCode, body)
	}

	if got := authJSON(t, http.DefaultClient, ts.URL, "/__auth/register", "王超_01", "correct-password"); got.status != http.StatusAccepted || !got.pending {
		t.Fatalf("pending registration = %#v", got)
	}
	if got := authJSON(t, http.DefaultClient, ts.URL, "/__auth/login", "王超_01", "correct-password"); got.status != http.StatusForbidden || !got.pending {
		t.Fatalf("pending login = %#v", got)
	}

	accounts := s.Accounts()
	if len(accounts) != 1 || accounts[0].Username != "王超_01" || accounts[0].Status != server.AccountStatusPending {
		t.Fatalf("accounts = %#v", accounts)
	}
	if err = s.SetAccountStatus(accounts[0].ID, server.AccountStatusActive); err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if got := authJSON(t, client, ts.URL, "/__auth/login", "王超_01", "correct-password"); got.status != http.StatusOK || !got.authenticated {
		t.Fatalf("approved login = %#v", got)
	}
	response, err = client.Get(ts.URL + "/__hfs/chat/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d", response.StatusCode)
	}
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/__hfs/chat/ws?tab=0123456789abcdef0123456789abcdef"
	headers := http.Header{"Origin": []string{ts.URL}}
	for _, cookie := range jar.Cookies(response.Request.URL) {
		headers.Add("Cookie", cookie.String())
	}
	connection, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatal(err)
	}
	var ready map[string]any
	if err = connection.ReadJSON(&ready); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	_ = connection.Close()
	if ready["type"] != "ready" || ready["clientId"] != accounts[0].ID || ready["name"] != "王超_01" {
		t.Fatalf("websocket identity leaked network IP: %#v", ready)
	}
	response, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	profiles := s.ChatUsers()
	if len(profiles) != 1 || profiles[0].IP != accounts[0].ID ||
		profiles[0].Username != "王超_01" || profiles[0].Client.IP == profiles[0].IP {
		t.Fatalf("chat profile is not account based: %#v", profiles)
	}
	logoutRequest, _ := http.NewRequest(http.MethodPost, ts.URL+"/__auth/logout", nil)
	logoutRequest.Header.Set("Origin", ts.URL)
	logoutResponse, err := client.Do(logoutRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = logoutResponse.Body.Close()
	response, err = client.Get(ts.URL + "/__hfs/chat/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logged-out status = %d", response.StatusCode)
	}
	s.SetAccountApprovalRequired(false)
	autoJar, _ := cookiejar.New(nil)
	autoClient := &http.Client{Jar: autoJar}
	if got := authJSON(t, autoClient, ts.URL, "/__auth/register", "auto_user", "another-password"); got.status != http.StatusCreated || !got.authenticated {
		t.Fatalf("automatic registration = %#v", got)
	}
	response, err = autoClient.Get(ts.URL + "/__hfs/chat/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("automatic account status = %d", response.StatusCode)
	}

	stored, err := store.LoadAccounts()
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored accounts = %#v, %v", stored, err)
	}
	for _, account := range stored {
		if account.PasswordHash == "correct-password" || account.PasswordHash == "another-password" ||
			!strings.HasPrefix(account.PasswordHash, "$argon2id$") {
			t.Fatalf("password was not stored as Argon2id: %q", account.PasswordHash)
		}
	}
}

func TestAdministratorPasswordResetRevokesExistingSessions(t *testing.T) {
	store, err := database.Open(filepath.Join(t.TempDir(), "tinychatgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := server.New(io.Discard)
	if err = s.SetPersistence(store); err != nil {
		t.Fatal(err)
	}
	s.SetAccountApprovalRequired(false)
	ts := httptest.NewServer(s)
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if got := authJSON(t, client, ts.URL, "/__auth/register", "reset_user", "old-password"); got.status != http.StatusCreated {
		t.Fatalf("register = %#v", got)
	}
	accounts := s.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %#v", accounts)
	}
	if err = s.SetAccountPassword(accounts[0].ID, "new-password"); err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(ts.URL + "/__hfs/chat/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session status = %d", response.StatusCode)
	}
	if got := authJSON(t, client, ts.URL, "/__auth/login", "reset_user", "old-password"); got.status != http.StatusUnauthorized {
		t.Fatalf("old password login = %#v", got)
	}
	if got := authJSON(t, client, ts.URL, "/__auth/login", "reset_user", "new-password"); got.status != http.StatusOK {
		t.Fatalf("new password login = %#v", got)
	}
}

type authResult struct {
	status        int
	pending       bool
	authenticated bool
	message       string
}

func authJSON(t *testing.T, client *http.Client, baseURL, endpoint, username, password string) authResult {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
	request, _ := http.NewRequest(http.MethodPost, baseURL+endpoint, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", baseURL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value struct {
		Pending       bool   `json:"pending"`
		Authenticated bool   `json:"authenticated"`
		Message       string `json:"message"`
	}
	_ = json.NewDecoder(response.Body).Decode(&value)
	return authResult{
		status: response.StatusCode, pending: value.Pending,
		authenticated: value.Authenticated, message: value.Message,
	}
}
