package server

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func encodedTestAvatar(t *testing.T, size int) string {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			source.Set(x, y, color.RGBA{
				R: uint8(x * 255 / size),
				G: uint8(y * 255 / size),
				B: 180,
				A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, source, &jpeg.Options{Quality: 82}); err != nil {
		t.Fatal(err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func TestCleanChatAvatarRequiresCompressedSquareJPEG(t *testing.T) {
	avatar := encodedTestAvatar(t, chatAvatarSize)
	cleaned, err := cleanChatAvatar(avatar)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned != avatar {
		t.Fatal("valid avatar was not preserved canonically")
	}
	if _, err = cleanChatAvatar(encodedTestAvatar(t, 64)); err == nil {
		t.Fatal("avatar with an unexpected size was accepted")
	}
	if _, err = cleanChatAvatar("data:image/png;base64,AAAA"); err == nil {
		t.Fatal("non-JPEG avatar was accepted")
	}
	if removed, err := cleanChatAvatar(""); err != nil || removed != "" {
		t.Fatalf("empty avatar removal = %q, %v", removed, err)
	}
}

func TestChatAvatarBroadcastAndReadyDirectory(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	s.SetGroupChatEnabled(true)
	s.SetUserListEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	firstIP := "192.0.2.41"
	first, response, err := dialChat(
		ts.URL,
		cookie,
		strings.Repeat("a", 32),
		"",
		"",
		ts.URL,
		http.Header{"X-Forwarded-For": []string{firstIP}},
	)
	if err != nil {
		t.Fatalf("dial first: %v (response %#v)", err, response)
	}
	_ = readChatWire(t, first)

	avatar := encodedTestAvatar(t, chatAvatarSize)
	if err = first.WriteJSON(chatClientMessage{Type: "setAvatar", Avatar: avatar}); err != nil {
		t.Fatal(err)
	}
	ack := readChatType(t, first, "avatar")
	if ack.ClientID != firstIP || ack.Avatar != avatar {
		t.Fatalf("avatar acknowledgement = %#v", ack)
	}
	s.chat.mu.Lock()
	public := s.chat.publicUsersLocked(firstIP)
	s.chat.mu.Unlock()
	if len(public) != 1 || public[0].Avatar != avatar {
		t.Fatalf("public user avatar = %#v", public)
	}
	if err = first.WriteJSON(chatClientMessage{Type: "setName", Name: "头像用户"}); err != nil {
		t.Fatal(err)
	}
	_ = readChatType(t, first, "name")
	if err = first.WriteJSON(chatClientMessage{Type: "message", Text: "保留头像的历史消息"}); err != nil {
		t.Fatal(err)
	}
	_ = readChatType(t, first, "message")
	_ = first.Close()
	waitFor(t, func() bool { return len(s.ChatOnlineClients()) == 0 })

	second, response, err := dialChat(
		ts.URL,
		cookie,
		strings.Repeat("b", 32),
		"",
		"",
		ts.URL,
		http.Header{"X-Forwarded-For": []string{"192.0.2.42"}},
	)
	if err != nil {
		t.Fatalf("dial second: %v (response %#v)", err, response)
	}
	defer second.Close()
	ready := readChatWire(t, second)
	if ready.Avatars[firstIP] != avatar {
		t.Fatalf("ready avatar directory = %#v", ready.Avatars)
	}
	if len(ready.History) != 1 || ready.History[0].ClientID != firstIP {
		t.Fatalf("ready history missing avatar owner: %#v", ready.History)
	}
}

func TestPortalRendersAvatarUploadMessageAvatarsAndMuteBadge(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	s.SetUserListEnabled(true)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("portal status = %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{
		`id="avatar-input"`,
		`function compressAvatar(file)`,
		`canvas.width=canvas.height=96`,
		`type:'setAvatar',avatar:pendingAvatar`,
		`class="avatar-muted"`,
		`message-avatar`,
		`data.type==='avatar'`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("portal avatar feature missing %q", marker)
		}
	}
}
