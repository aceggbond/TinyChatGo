package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUserAliasesPrivateMessagesAndBlacklist(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	s.SetUserListEnabled(true)
	s.SetPrivateMessagesEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()

	cookie, enabled := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	if !enabled {
		t.Fatal("chat status was disabled")
	}
	headersA := http.Header{"X-Forwarded-For": []string{"192.0.2.10"}}
	headersB := http.Header{"X-Forwarded-For": []string{"192.0.2.20"}}
	connA, response, err := dialChat(ts.URL, cookie, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "", ts.URL, headersA)
	if err != nil {
		t.Fatalf("dial A: %v (response %#v)", err, response)
	}
	defer connA.Close()
	readyA := readChatWire(t, connA)
	if readyA.Type != "ready" || readyA.ClientID != "192.0.2.10" || !readyA.UserListEnabled || !readyA.PrivateEnabled {
		t.Fatalf("ready A = %#v", readyA)
	}

	connB, response, err := dialChat(ts.URL, cookie, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", "", ts.URL, headersB)
	if err != nil {
		t.Fatalf("dial B: %v (response %#v)", err, response)
	}
	defer connB.Close()
	readyB := readChatWire(t, connB)
	if readyB.Type != "ready" || readyB.ClientID != "192.0.2.20" || len(readyB.Users) != 2 {
		t.Fatalf("ready B = %#v", readyB)
	}
	usersA := readChatType(t, connA, "users")
	if len(usersA.Users) < 2 {
		usersA = readChatType(t, connA, "users")
	}
	if len(usersA.Users) != 2 {
		t.Fatalf("A user list = %#v", usersA)
	}

	if err = connA.WriteJSON(chatClientMessage{Type: "setName", Name: "王超"}); err != nil {
		t.Fatal(err)
	}
	ack := readChatType(t, connA, "name")
	if ack.Name != "192.0.2.10-王超" {
		t.Fatalf("name ack = %#v", ack)
	}
	foundAlias := false
	var usersForB chatWireMessage
	for attempt := 0; attempt < 3 && !foundAlias; attempt++ {
		usersForB = readChatType(t, connB, "users")
		for _, user := range usersForB.Users {
			if user.IP == "192.0.2.10" && user.Alias == "王超" && user.Name == "192.0.2.10-王超" {
				foundAlias = true
			}
		}
	}
	if !foundAlias {
		t.Fatalf("renamed user list = %#v", usersForB.Users)
	}

	if err = connA.WriteJSON(chatClientMessage{
		Type:     "message",
		Kind:     ChatMessageKindText,
		Text:     "这是一条私信",
		TargetID: "192.0.2.20",
	}); err != nil {
		t.Fatal(err)
	}
	privateA := readChatType(t, connA, "message")
	privateB := readChatType(t, connB, "message")
	for label, message := range map[string]chatWireMessage{"sender": privateA, "recipient": privateB} {
		if !message.Private || message.ClientID != "192.0.2.10" || message.TargetID != "192.0.2.20" || message.Text != "这是一条私信" {
			t.Fatalf("%s private message = %#v", label, message)
		}
	}

	if err = s.SetChatUserBlacklisted("192.0.2.20", true); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	request.Header.Set("X-Forwarded-For", "192.0.2.20")
	blocked, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden {
		t.Fatalf("blacklisted HTTP status = %d", blocked.StatusCode)
	}
}

func TestChatUserSearchKeyContainsPinyinAndInitials(t *testing.T) {
	key := chatUserSearchKey("192.0.2.10", "王超")
	for _, expected := range []string{"192.0.2.10", "wangchao", "wang chao", "wc"} {
		if !strings.Contains(key, expected) {
			t.Fatalf("search key %q does not contain %q", key, expected)
		}
	}
	if exported := ChatUserSearchKey("192.0.2.10", "王超"); exported != key {
		t.Fatalf("exported search key %q differs from browser search key %q", exported, key)
	}
}

func TestPortalSettingsAndFluentEmojiPicker(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("portal status = %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{
		`id="portal-settings-button"`,
		`id="settings-my-info"`,
		`id="native-settings"`,
		`id="web-settings"`,
		`id="native-server-address-note"`,
		`.settings-row:has(#native-server-address-note){display:none!important}`,
		`id="native-check-update"`,
		`$('web-settings').hidden=nativeClient`,
		`$('native-settings').hidden=!nativeClient`,
		`id="emoji-picker"`,
		`Microsoft Fluent 3D 动态表情 · 离线可用`,
		`textBox.setRangeText(b.dataset.emoji,s,e,'end')`,
		`var emojis=['😀'`,
		`/__hfs/fluent-emoji/`,
		`openNameModal('my',false)`,
		`id="chat-code-button"`,
		`id="chat-dice-button"`,
		`id="code-modal"`,
		`data-external-url="`,
		`if(selectedTarget||!selectedGroup)return[]`,
		`if(key.indexOf('group:')!==0)return false`,
		`id="group-manage-button"`,
		`text=online?'对方在线':'对方不在线'`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("portal name/emoji controls missing %q", marker)
		}
	}
	for _, removed := range []string{
		`id="native-connect-form"`,
		`id="native-server-address"`,
		`id="native-scan-services"`,
		"发现服务",
		"window.clientConnect",
		"window.clientScan",
	} {
		if strings.Contains(body, removed) {
			t.Fatalf("portal still exposes editable client connection UI %q", removed)
		}
	}
	if strings.Contains(body, `id="my-name-button"`) ||
		strings.Contains(body, `id="native-client-settings"`) {
		t.Fatal("legacy standalone client/name button is still rendered")
	}
}

func TestPortalChatRecordsAndContactContextActions(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("portal status = %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{
		`id="chat-record-button"`,
		`id="history-tab-chat"`,
		`id="history-tab-file"`,
		`id="history-tab-image"`,
		`data-history-kind="text">搜索聊天`,
		`var contact=event.target.closest&&event.target.closest('.contact-item')`,
		`item.label!=='发私信'`,
		`label:'设置备注'`,
		`button.ondblclick=function(){if(button.dataset.ip)openPrivateChat(button.dataset.ip)}`,
		`label:'删除会话'`,
		`'关闭消息提醒':'开启消息提醒'`,
		`'取消置顶会话':'置顶会话'`,
		`hiddenSessions=loadObject('lanchatgo-hidden-sessions')`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("portal chat records/contact action missing %q", marker)
		}
	}
	if strings.Contains(body, `id="chat-more-button"`) || strings.Contains(body, `id="chat-more-menu"`) {
		t.Fatal("legacy more menu is still rendered")
	}
	groupIndex := strings.Index(body, `id="create-group-button"`)
	onlineIndex := strings.Index(body, `id="online-count"`)
	if groupIndex < 0 || onlineIndex < 0 || groupIndex > onlineIndex {
		t.Fatal("+群聊 is not placed before online count")
	}
}

func TestSecureDicePointAlwaysReturnsValidFace(t *testing.T) {
	seen := make(map[int]bool)
	for index := 0; index < 256; index++ {
		point, err := secureDicePoint()
		if err != nil {
			t.Fatal(err)
		}
		if point < 1 || point > 6 {
			t.Fatalf("dice point = %d", point)
		}
		seen[point] = true
	}
	if len(seen) < 2 {
		t.Fatalf("dice source did not vary: %#v", seen)
	}
}

func TestAdministratorPrivateMessageInSystemGroupTargetsOneUser(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	s.SetGroupChatEnabled(true)
	s.SetUserListEnabled(true)
	s.SetPrivateMessagesEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	headersA := http.Header{"X-Forwarded-For": []string{"192.0.2.31"}}
	headersB := http.Header{"X-Forwarded-For": []string{"192.0.2.32"}}
	first, _, err := dialChat(ts.URL, cookie, strings.Repeat("3", 32), "甲", "", ts.URL, headersA)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstReady := readChatWire(t, first)
	second, _, err := dialChat(ts.URL, cookie, strings.Repeat("4", 32), "乙", "", ts.URL, headersB)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = readChatWire(t, second)

	if err = s.SendChatMessage(firstReady.ClientID, "仅发送给甲"); err != nil {
		t.Fatal(err)
	}
	private := readChatType(t, first, "message")
	if private.Text != "仅发送给甲" || private.Sender != "admin" || !private.Private ||
		private.TargetID != firstReady.ClientID || private.Group {
		t.Fatalf("administrator private message = %#v", private)
	}
	_ = second.SetReadDeadline(time.Now().Add(180 * time.Millisecond))
	for {
		var message chatWireMessage
		readErr := second.ReadJSON(&message)
		if readErr != nil {
			if timeout, ok := readErr.(net.Error); ok && timeout.Timeout() {
				break
			}
			t.Fatalf("read non-target connection: %v", readErr)
		}
		if message.Type == "message" {
			t.Fatalf("non-target user received administrator private message: %#v", message)
		}
	}

	group, ok := s.ChatConversationSnapshot(ChatGroupConversationID)
	if !ok {
		t.Fatal("system-group snapshot missing")
	}
	for _, message := range group.Messages {
		if message.Text == "仅发送给甲" {
			t.Fatal("administrator private message leaked into system-group history")
		}
	}
	direct, ok := s.ChatConversationSnapshot(firstReady.ClientID)
	if !ok || len(direct.Messages) != 1 || !direct.Messages[0].Private {
		t.Fatalf("administrator private conversation = %#v, ok=%v", direct, ok)
	}
	if err = first.WriteJSON(chatClientMessage{Type: "setName", Name: "甲"}); err != nil {
		t.Fatal(err)
	}
	if named := readChatType(t, first, "name"); named.Name != firstReady.ClientID+"-甲" {
		t.Fatalf("name acknowledgement before administrator private message = %#v", named)
	}
	if err = first.WriteJSON(chatClientMessage{
		Type:     "message",
		Kind:     ChatMessageKindText,
		Text:     "回复管理员",
		TargetID: ChatAdminConversationID,
	}); err != nil {
		t.Fatal(err)
	}
	reply := readChatType(t, first, "message")
	if reply.Text != "回复管理员" || reply.Sender != "user" || !reply.Private ||
		reply.TargetID != ChatAdminConversationID || reply.Group {
		t.Fatalf("user-to-administrator private message = %#v", reply)
	}
	direct, ok = s.ChatConversationSnapshot(firstReady.ClientID)
	if !ok || len(direct.Messages) != 2 || direct.Messages[1].TargetID != ChatAdminConversationID {
		t.Fatalf("administrator conversation after user reply = %#v, ok=%v", direct, ok)
	}
	administratorOverview := s.ChatAdministratorOverview()
	if len(administratorOverview) != 2 {
		t.Fatalf("administrator overview should include both IP records in group mode: %#v", administratorOverview)
	}
	var firstOverview *ChatConversation
	for index := range administratorOverview {
		if administratorOverview[index].ID == firstReady.ClientID {
			firstOverview = &administratorOverview[index]
			break
		}
	}
	if firstOverview == nil || len(firstOverview.Messages) != 2 ||
		firstOverview.Messages[1].Text != "回复管理员" {
		t.Fatalf("administrator IP overview missing private activity: %#v", administratorOverview)
	}

	_ = first.Close()
	reconnected, _, err := dialChat(ts.URL, cookie, strings.Repeat("5", 32), "甲重连", "", ts.URL, headersA)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	reconnectedReady := readChatWire(t, reconnected)
	for _, message := range reconnectedReady.History {
		if message.Private {
			t.Fatalf("administrator private message leaked into restored system-group history: %#v", reconnectedReady.History)
		}
	}
	adminHistory := reconnectedReady.DirectHistory[ChatAdminConversationID]
	if len(adminHistory) != 2 || adminHistory[0].Text != "仅发送给甲" ||
		adminHistory[1].Text != "回复管理员" {
		t.Fatalf("administrator private history was not restored separately: %#v", reconnectedReady.DirectHistory)
	}
}

func readChatType(t *testing.T, connection interface {
	SetReadDeadline(time.Time) error
	ReadJSON(any) error
}, messageType string) chatWireMessage {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 10; attempt++ {
		var message chatWireMessage
		if err := connection.ReadJSON(&message); err != nil {
			t.Fatal(err)
		}
		if message.Type == messageType {
			return message
		}
	}
	t.Fatalf("did not receive chat frame %q", messageType)
	return chatWireMessage{}
}
