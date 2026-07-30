package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebAccessPasswordGateUsesCookieAndEmbeddedHeader(t *testing.T) {
	s := New(io.Discard)
	s.SetAccess("web-secret", false, true, false)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()

	response, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(string(body), "Web 访问密码") {
		t.Fatalf("anonymous response = %d %q", response.StatusCode, body)
	}

	client := &http.Client{}
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/__auth/access", strings.NewReader(`{"password":"wrong"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong access password status = %d", response.StatusCode)
	}

	jarClient := &http.Client{}
	request, _ = http.NewRequest(http.MethodPost, ts.URL+"/__auth/access", strings.NewReader(`{"password":"web-secret"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err = jarClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	_ = json.NewDecoder(response.Body).Decode(&result)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || result["ok"] != true {
		t.Fatalf("correct access password response = %d %#v", response.StatusCode, result)
	}
	// A plain client does not retain cookies, so the embedded header remains
	// the native client's fallback authentication mechanism.
	request, _ = http.NewRequest(http.MethodGet, ts.URL+"/__hfs/chat/status", nil)
	request.Header.Set("X-LanChatGo-Access-Password", "web-secret")
	response, err = jarClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("embedded access header status = %d", response.StatusCode)
	}
}

func TestWebAccessPasswordCookieAllowsPortal(t *testing.T) {
	s := New(io.Discard)
	s.SetAccess("web-secret", false, true, false)
	ts := httptest.NewServer(s)
	defer ts.Close()
	client := &http.Client{}
	request, _ := http.NewRequest(http.MethodPost, ts.URL+"/__auth/access", strings.NewReader(`{"password":"web-secret"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	cookies := response.Cookies()
	_ = response.Body.Close()
	if len(cookies) == 0 {
		t.Fatal("successful access did not issue a cookie")
	}
	request, _ = http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	request.AddCookie(cookies[0])
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || strings.Contains(string(body), "Web 访问密码") {
		t.Fatalf("cookie portal response = %d %q", response.StatusCode, body)
	}
}
