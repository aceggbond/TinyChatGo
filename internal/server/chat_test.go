package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

type synchronizedLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestChatStatusAndDisabledHandshake(t *testing.T) {
	s := New(io.Discard)
	ts := httptest.NewServer(s)
	defer ts.Close()

	cookie, enabled := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	if enabled {
		t.Fatal("chat should be disabled by default")
	}
	if cookie == nil || cookie.Name != chatCookieName || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("bad chat cookie: %#v", cookie)
	}

	_, response, err := dialChat(ts.URL, cookie, strings.Repeat("a", 32), "访客", "", ts.URL, nil)
	if err == nil {
		t.Fatal("disabled chat unexpectedly upgraded")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled handshake status = %#v, %v", response, err)
	}
}

func TestChatOperationLogsUseIP(t *testing.T) {
	logs := &synchronizedLogBuffer{}
	s := New(logs)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	conn, _, err := dialChat(ts.URL, cookie, strings.Repeat("b", 32), "不能作为身份", "", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := readChatWire(t, conn)
	if ready.ClientID == "" {
		t.Fatalf("ready did not expose an IP identity: %#v", ready)
	}
	if err = conn.WriteJSON(chatClientMessage{Type: "message", Text: "记录来源 IP"}); err != nil {
		t.Fatal(err)
	}
	_ = readChatWire(t, conn)
	if err = s.SendChatMessage(ready.ClientID, "后台回复"); err != nil {
		t.Fatal(err)
	}
	_ = readChatWire(t, conn)

	for _, operation := range []string{"聊天连接上线", "接收聊天文本", "后台发送聊天文本"} {
		marker := "IP=" + ready.ClientID + " 操作=" + operation
		waitFor(t, func() bool { return strings.Contains(logs.String(), marker) })
	}
	_ = conn.Close()
}

func TestChatWidgetRendersAndSelectsWSSForHTTPS(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, `location.protocol==='https:'?'wss://':'ws://'`) {
		t.Fatal("chat client does not select wss for an HTTPS page")
	}
	for _, marker := range []string{
		`id="chat-notify"`,
		`id="chat-record-button"`,
		`toggleConversationNotify(key)`,
		`id="attachment-drafts"`,
		`queueFiles(files)`,
		`sendDrafts()`,
		`socket.send(JSON.stringify({type:'read'`,
		`type:'read',targetId:key`,
		`function applyRead(frame)`,
		`if(frame.clientId!==currentIP)return`,
		`isRead=m.read===true`,
		`type:'view',targetId:target`,
		`function markConversationRead(key)`,
		`function scrollMessagesBottom()`,
		`function pageVisible()`,
		`.message.mine.pending-read .message-meta{color:var(--red)`,
		`.message.mine.pending-read .bubble{background:var(--blue)`,
		`id="group-manage-button"`,
		`text=online?'对方在线':'对方不在线'`,
		`data-user-ip=`,
		`window.lanchatOpenExternal`,
		`type:'renameGroup'`,
		`type:'addGroupMembers'`,
		`type:'removeGroupMember'`,
		`释放后加入待发送列表`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("chat widget is missing %q", marker)
		}
	}
	if strings.Contains(body, `id="chat-attach"`) || strings.Contains(body, `id="chat-file"`) || strings.Contains(body, `>选择图片</button>`) {
		t.Fatal("chat widget still renders a separate image upload button")
	}
	if strings.Contains(body, `id="chat-name"`) || strings.Contains(body, `hfs-chat-name`) || strings.Contains(body, `query.set('name'`) {
		t.Fatal("chat widget still exposes a visitor-name identity")
	}
	if strings.Contains(body, `>系统群<`) || strings.Contains(body, `selectedTarget='main'`) {
		t.Fatal("portal still exposes the removed system group")
	}

}

func TestChatControlFramesDoNotConsumeSendRate(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	conn, response, err := dialChat(
		ts.URL,
		cookie,
		strings.Repeat("9", 32),
		"",
		"",
		ts.URL,
		nil,
	)
	if err != nil {
		t.Fatalf("dial chat: %v (response %#v)", err, response)
	}
	defer conn.Close()
	_ = readChatWire(t, conn)

	for index := 0; index < chatRateEvents+20; index++ {
		if err = conn.WriteJSON(chatClientMessage{Type: "view"}); err != nil {
			t.Fatalf("write view frame %d: %v", index, err)
		}
	}
	if err = conn.WriteJSON(chatClientMessage{Type: "message", Text: "正常消息"}); err != nil {
		t.Fatal(err)
	}
	message := readChatType(t, conn, "message")
	if message.Text != "正常消息" {
		t.Fatalf("message after control frames = %#v", message)
	}
}

func TestChatRateLimitAllowsNormalBurst(t *testing.T) {
	if chatRateEvents < 30 {
		t.Fatalf("chat rate limit is still too strict: %d events per %s", chatRateEvents, chatRateWindow)
	}
	for _, eventType := range []string{"view", "read"} {
		if chatEventCountsTowardsRate(eventType) {
			t.Fatalf("%s control frame unexpectedly consumes the send allowance", eventType)
		}
	}
	for _, eventType := range []string{"message", "recall", "setName", "createGroup"} {
		if !chatEventCountsTowardsRate(eventType) {
			t.Fatalf("%s mutation unexpectedly bypasses the send allowance", eventType)
		}
	}
}

func TestChatCodeAndServerGeneratedDiceMessages(t *testing.T) {
	s := New(io.Discard)
	s.SetGroupChatEnabled(true)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	conn, response, err := dialChat(
		ts.URL,
		cookie,
		strings.Repeat("d", 32),
		"",
		"",
		ts.URL,
		nil,
	)
	if err != nil {
		t.Fatalf("dial chat: %v (response %#v)", err, response)
	}
	defer conn.Close()
	_ = readChatWire(t, conn)

	const code = "func main() {\n\tprintln(\"TinyChatGo\")\n}"
	if err = conn.WriteJSON(chatClientMessage{
		Type: "message",
		Kind: ChatMessageKindCode,
		Text: code,
	}); err != nil {
		t.Fatal(err)
	}
	wire := readChatWire(t, conn)
	if wire.Type != "message" || wire.Kind != ChatMessageKindCode || wire.Text != code {
		t.Fatalf("code message = %#v", wire)
	}

	if err = conn.WriteJSON(chatClientMessage{
		Type: "message",
		Kind: ChatMessageKindDice,
		Text: "99",
	}); err != nil {
		t.Fatal(err)
	}
	wire = readChatWire(t, conn)
	if wire.Type != "message" || wire.Kind != ChatMessageKindDice ||
		len(wire.Text) != 1 || wire.Text[0] < '1' || wire.Text[0] > '6' {
		t.Fatalf("server-generated dice message = %#v", wire)
	}

	conversation, ok := s.ChatConversationSnapshot(ChatGroupConversationID)
	if !ok || len(conversation.Messages) < 2 {
		t.Fatalf("system group snapshot = %#v, %v", conversation, ok)
	}
	messages := conversation.Messages[len(conversation.Messages)-2:]
	if messages[0].Kind != ChatMessageKindCode || messages[0].Text != code ||
		messages[1].Kind != ChatMessageKindDice || messages[1].Text != wire.Text {
		t.Fatalf("persisted special messages = %#v", messages)
	}
}

func TestPortalAttachmentContextForwardAndGroupMentions(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	s.SetUserListEnabled(true)
	s.SetPrivateMessagesEnabled(true)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("portal status = %d", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{
		`id="mention-picker"`,
		`群聊输入 @ 可选择成员`,
		`label:'@TA'`,
		`data-user-ip`,
		`avatarHTML(m.clientId,false,'','message-avatar',userIP)`,
		`fetch('/__hfs/chat/forward'`,
		`typeof window.lanchatCopyImage==='function'`,
		`typeof window.lanchatCopyFile==='function'`,
		`needsAttention=!pageVisible()||key!==routeKey()`,
		`.attachment-name{display:block;max-width:100%;overflow:hidden;text-overflow:ellipsis`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("portal attachment/mention feature missing %q", marker)
		}
	}
}

func TestBrandLogoEndpoint(t *testing.T) {
	s := New(io.Discard)
	want := []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10, 1, 2, 3}
	s.SetBrandLogo(want)
	request := httptest.NewRequest(http.MethodGet, "http://example.test/__hfs/logo.png", nil)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("logo status = %d", recorder.Code)
	}
	if got := recorder.Body.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("logo body = %v, want %v", got, want)
	}
	if recorder.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("logo content type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestChatClientAdminRoundTripAndHistory(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, enabled := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	if !enabled {
		t.Fatal("chat status should be enabled")
	}
	tab := strings.Repeat("1", 32)
	conn, response, err := dialChat(ts.URL, cookie, tab, "小明", "", ts.URL, nil)
	if err != nil {
		t.Fatalf("dial: %v (response %#v)", err, response)
	}
	ready := readChatWire(t, conn)
	if ready.Type != "ready" || ready.ClientID != "127.0.0.1" || ready.Name != ready.ClientID || len(ready.History) != 0 {
		t.Fatalf("bad ready message: %#v", ready)
	}

	if err = conn.WriteJSON(chatClientMessage{Type: "message", Text: "你好，管理员"}); err != nil {
		t.Fatal(err)
	}
	echo := readChatWire(t, conn)
	if echo.Type != "message" || echo.Sender != "user" || echo.Text != "你好，管理员" || echo.ID == "" ||
		echo.ClientID != ready.ClientID || echo.Name != ready.ClientID {
		t.Fatalf("bad user echo: %#v", echo)
	}
	waitFor(t, func() bool {
		items := s.ChatSnapshot()
		return len(items) == 1 && items[0].Online && len(items[0].Messages) == 1
	})
	copyOfSnapshot := s.ChatSnapshot()
	copyOfSnapshot[0].Messages[0].Text = "被外部修改"
	if got := s.ChatSnapshot()[0].Messages[0].Text; got != "你好，管理员" {
		t.Fatalf("ChatSnapshot did not deep-copy messages: %q", got)
	}
	if stored := s.ChatSnapshot()[0].Messages[0]; stored.ClientID != ready.ClientID || stored.Name != ready.ClientID {
		t.Fatalf("stored visitor identity is not the canonical IP: %#v", stored)
	}

	if err = s.SendChatMessage(ready.ClientID, "收到"); err != nil {
		t.Fatal(err)
	}
	reply := readChatWire(t, conn)
	if reply.Type != "message" || reply.Sender != "admin" || reply.Text != "收到" || reply.ID == "" {
		t.Fatalf("bad admin reply: %#v", reply)
	}
	_ = conn.Close()
	waitFor(t, func() bool { return s.ChatOnlineCount() == 0 })

	conn, response, err = dialChat(ts.URL, cookie, tab, "小明", "", ts.URL, nil)
	if err != nil {
		t.Fatalf("redial: %v (response %#v)", err, response)
	}
	defer conn.Close()
	ready = readChatWire(t, conn)
	if len(ready.History) != 2 || ready.History[0].Text != "你好，管理员" || ready.History[1].Text != "收到" {
		t.Fatalf("history not restored: %#v", ready.History)
	}

	if err = conn.WriteJSON(chatClientMessage{Type: "name", Name: "新的称呼"}); err != nil {
		t.Fatal(err)
	}
	nameAck := readChatWire(t, conn)
	if nameAck.Type != "name" || nameAck.Name != ready.ClientID {
		t.Fatalf("bad name ack: %#v", nameAck)
	}
	waitFor(t, func() bool {
		items := s.ChatSnapshot()
		return len(items) == 1 && items[0].Name == ready.ClientID
	})

	s.SetChatEnabled(false)
	_, _, err = conn.ReadMessage()
	var closeError *websocket.CloseError
	if !websocket.IsCloseError(err, chatCloseDisabled) {
		t.Fatalf("disable close error = %#v (%T), want code %d", err, closeError, chatCloseDisabled)
	}
	if s.ChatOnlineCount() != 0 {
		t.Fatalf("online count after disable = %d", s.ChatOnlineCount())
	}
}

func TestDuplicateChatSessionGetsIndependentReconnectSignal(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	tab := strings.Repeat("d", 32)
	first, _, err := dialChat(ts.URL, cookie, tab, "第一个页面", "", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstReady := readChatWire(t, first)

	second, _, err := dialChat(ts.URL, cookie, tab, "复制的页面", "", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondReady := readChatWire(t, second)
	if firstReady.ClientID == "" || secondReady.ClientID != firstReady.ClientID {
		t.Fatalf("duplicate session IDs = %q and %q", firstReady.ClientID, secondReady.ClientID)
	}

	_ = first.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = first.ReadMessage()
	if !websocket.IsCloseError(err, chatCloseSessionReplaced) {
		t.Fatalf("replaced page close = %v, want code %d", err, chatCloseSessionReplaced)
	}

	if err = second.WriteJSON(chatClientMessage{Type: "message", Text: "仍然在线"}); err != nil {
		t.Fatal(err)
	}
	if echo := readChatWire(t, second); echo.Type != "message" || echo.Text != "仍然在线" {
		t.Fatalf("replacement connection echo = %#v", echo)
	}
}

func TestChatIPIdentitySharesPrivateConversationAcrossTabs(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	first, response, err := dialChat(ts.URL, cookie, strings.Repeat("a", 32), "Alice", "", ts.URL, nil)
	if err != nil {
		t.Fatalf("dial first tab: %v (response %#v)", err, response)
	}
	defer first.Close()
	firstReady := readChatWire(t, first)

	second, response, err := dialChat(ts.URL, cookie, strings.Repeat("b", 32), "Bob", "", ts.URL, nil)
	if err != nil {
		t.Fatalf("dial second tab: %v (response %#v)", err, response)
	}
	defer second.Close()
	secondReady := readChatWire(t, second)

	if firstReady.ClientID != "127.0.0.1" || secondReady.ClientID != firstReady.ClientID ||
		firstReady.Name != firstReady.ClientID || secondReady.Name != secondReady.ClientID {
		t.Fatalf("IP ready identities = first %#v, second %#v", firstReady, secondReady)
	}
	if count := s.ChatOnlineCount(); count != 1 {
		t.Fatalf("unique-IP online count = %d, want 1", count)
	}
	if conversations := s.ChatOverview(); len(conversations) != 1 ||
		conversations[0].ID != firstReady.ClientID || conversations[0].Name != firstReady.ClientID {
		t.Fatalf("shared IP conversation = %#v", conversations)
	}

	if err = first.WriteJSON(chatClientMessage{Type: "message", Text: "shared from first tab"}); err != nil {
		t.Fatal(err)
	}
	for label, wire := range map[string]chatWireMessage{
		"first":  readChatWire(t, first),
		"second": readChatWire(t, second),
	} {
		if wire.Type != "message" || wire.Text != "shared from first tab" ||
			wire.ClientID != firstReady.ClientID || wire.Name != firstReady.ClientID {
			t.Fatalf("%s shared visitor message = %#v", label, wire)
		}
	}

	if err = s.SendChatMessage(firstReady.ClientID, "one admin reply"); err != nil {
		t.Fatal(err)
	}
	for label, wire := range map[string]chatWireMessage{
		"first":  readChatWire(t, first),
		"second": readChatWire(t, second),
	} {
		if wire.Type != "message" || wire.Sender != "admin" || wire.Text != "one admin reply" {
			t.Fatalf("%s shared admin message = %#v", label, wire)
		}
	}
	snapshot := s.ChatSnapshot()
	if len(snapshot) != 1 || len(snapshot[0].Messages) != 2 {
		t.Fatalf("shared history was duplicated per socket: %#v", snapshot)
	}

	_ = first.Close()
	waitFor(t, func() bool { return s.ChatOnlineCount() == 1 })
	_ = second.Close()
	waitFor(t, func() bool { return s.ChatOnlineCount() == 0 })
}

func TestDismissedOnlineVisitorReappearsOnNextMessage(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	headers := make(http.Header)
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0")
	headers.Set("Sec-CH-UA-Platform", `"Windows"`)
	headers.Set("Sec-CH-UA-Platform-Version", `"13.0.0"`)
	tab := strings.Repeat("e", 32)
	conn, _, err := dialChat(ts.URL, cookie, tab, "测试访客", "", ts.URL, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ready := readChatWire(t, conn)

	if err = conn.WriteJSON(chatClientMessage{Type: "message", Text: "删除前"}); err != nil {
		t.Fatal(err)
	}
	_ = readChatWire(t, conn)
	if !s.RemoveChatVisitor(ready.ClientID) {
		t.Fatal("online visitor was not removed")
	}
	if overview := s.ChatOverview(); len(overview) != 0 {
		t.Fatalf("removed visitor remained visible: %#v", overview)
	}
	if s.ChatOnlineCount() != 1 {
		t.Fatalf("removing visitor closed its socket, online = %d", s.ChatOnlineCount())
	}
	if err = s.SendChatMessage(ready.ClientID, "不应发送"); err == nil || !strings.Contains(err.Error(), "已移除") {
		t.Fatalf("admin send to removed visitor error = %v", err)
	}

	if err = conn.WriteJSON(chatClientMessage{Type: "message", Text: "重新出现"}); err != nil {
		t.Fatal(err)
	}
	echo := readChatWire(t, conn)
	if echo.Text != "重新出现" {
		t.Fatalf("recreated visitor echo = %#v", echo)
	}
	overview := s.ChatOverview()
	if len(overview) != 1 || overview[0].ID != ready.ClientID || len(overview[0].Messages) != 1 ||
		overview[0].Messages[0].Text != "重新出现" {
		t.Fatalf("recreated visitor overview = %#v", overview)
	}
	if overview[0].Client.Browser != "Microsoft Edge 126.0.0.0" || overview[0].Client.OS != "Windows 11" ||
		overview[0].Client.IP == "" || overview[0].Client.Port == "" || overview[0].Client.ConnectedAt.IsZero() {
		t.Fatalf("visitor metadata = %#v", overview[0].Client)
	}

	_ = conn.Close()
	waitFor(t, func() bool { return s.ChatOnlineCount() == 0 })
	if !s.RemoveChatVisitor(ready.ClientID) {
		t.Fatal("offline visitor was not removed before reconnect")
	}
	reconnected, _, err := dialChat(ts.URL, cookie, tab, "测试访客", "", ts.URL, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	reconnectedReady := readChatWire(t, reconnected)
	if reconnectedReady.ClientID != ready.ClientID || len(reconnectedReady.History) != 0 {
		t.Fatalf("removed visitor reconnect ready = %#v", reconnectedReady)
	}
	overview = s.ChatOverview()
	if len(overview) != 1 || overview[0].ID != ready.ClientID || len(overview[0].Messages) != 0 || !overview[0].Online {
		t.Fatalf("removed visitor reconnect overview = %#v", overview)
	}
	if s.RemoveChatVisitor(ChatGroupConversationID) {
		t.Fatal("synthetic group conversation was removable")
	}
}

func TestSystemGroupReportsOnlineClientsAndRemovesOfflineUser(t *testing.T) {
	s := New(io.Discard)
	s.SetGroupChatEnabled(true)
	offlineIP := "192.0.2.88"
	s.ObserveChatUser(ChatClientInfo{
		IP:          offlineIP,
		Port:        "45678",
		Browser:     "Offline Browser",
		OS:          "Offline OS",
		ConnectedAt: time.Now().UTC(),
	})
	if !s.RemoveChatVisitor(offlineIP) {
		t.Fatal("offline persisted user could not be removed in system-group mode")
	}
	if users := s.ChatUsers(); len(users) != 0 {
		t.Fatalf("removed offline user remained: %#v", users)
	}

	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)
	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	connection, _, err := dialChat(ts.URL, cookie, strings.Repeat("7", 32), "在线用户", "", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ready := readChatWire(t, connection)
	clients := s.ChatOnlineClients()
	if len(clients) != 1 || clients[0].IP != ready.ClientID {
		t.Fatalf("system-group online clients = %#v, ready = %#v", clients, ready)
	}
	if count := s.ChatOnlineCount(); count != len(clients) {
		t.Fatalf("online count = %d, clients = %d", count, len(clients))
	}
}

func TestClientInfoParsesIPv6BrowserAndOS(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	request.RemoteAddr = "[2001:db8::25]:54321"
	request.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/125.0.0.0 Mobile Safari/537.36")
	info := clientInfoFromRequest(request)
	if info.IP != "2001:db8::25" || info.Port != "54321" ||
		info.Browser != "Google Chrome 125.0.0.0" || info.OS != "Android 14" ||
		info.ConnectedAt.IsZero() {
		t.Fatalf("parsed client info = %#v", info)
	}
}

func TestClientAddressCanonicalizesIPAndTrustsOnlyLoopbackProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	request.RemoteAddr = "[::ffff:192.0.2.44]:54321"
	request.Header.Set("X-Forwarded-For", "198.51.100.8, 127.0.0.1")
	ip, port := clientAddressFromRequest(request)
	if ip != "192.0.2.44" || port != "54321" {
		t.Fatalf("untrusted forwarded address = %q:%q", ip, port)
	}

	request.RemoteAddr = "[::1]:4567"
	request.Header.Set("X-Forwarded-For", "2001:0db8:0:0:0:0:0:25, 127.0.0.1")
	ip, port = clientAddressFromRequest(request)
	if ip != "2001:db8::25" || port != "4567" {
		t.Fatalf("trusted proxy address = %q:%q", ip, port)
	}

	request.Header.Set("X-Forwarded-For", "not-an-ip, 198.51.100.8")
	ip, port = clientAddressFromRequest(request)
	if ip != "::1" || port != "4567" {
		t.Fatalf("invalid forwarded address was trusted = %q:%q", ip, port)
	}
}

func TestClientAddressAcceptsForwardedIPOnlyFromConfiguredProxy(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	request.RemoteAddr = "10.20.30.40:3210"
	request.Header.Set("X-Forwarded-For", "198.51.100.77, 10.20.30.40")
	_, network, err := net.ParseCIDR("10.20.30.40/32")
	if err != nil {
		t.Fatal(err)
	}
	ip, port := clientAddressFromRequestWithTrustedProxies(request, []*net.IPNet{network})
	if ip != "198.51.100.77" || port != "3210" {
		t.Fatalf("configured proxy address = %q:%q", ip, port)
	}

	request.RemoteAddr = "10.20.30.41:3211"
	ip, port = clientAddressFromRequestWithTrustedProxies(request, []*net.IPNet{network})
	if ip != "10.20.30.41" || port != "3211" {
		t.Fatalf("unconfigured proxy address = %q:%q", ip, port)
	}
}

func TestChatAuthenticationAndOrigin(t *testing.T) {
	s := New(io.Discard)
	s.SetAccess("secret", false, true, false)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	response, err := http.Get(ts.URL + "/__hfs/chat/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status endpoint = %d", response.StatusCode)
	}

	client := &http.Client{}
	request, _ := http.NewRequest(http.MethodGet, ts.URL+"/__hfs/chat/status", nil)
	request.SetBasicAuth("hfs", "secret")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var status struct{ Enabled bool }
	_ = json.NewDecoder(response.Body).Decode(&status)
	_ = response.Body.Close()
	cookies := response.Cookies()
	if response.StatusCode != http.StatusOK || !status.Enabled || len(cookies) == 0 {
		t.Fatalf("authenticated status = %d, %#v, cookies=%d", response.StatusCode, status, len(cookies))
	}
	cookie := cookies[0]
	auth := make(http.Header)
	auth.Set("Authorization", "Basic aGZzOnNlY3JldA==")

	_, response, err = dialChat(ts.URL, cookie, strings.Repeat("2", 32), "", "", ts.URL, nil)
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated websocket = %#v, %v", response, err)
	}
	_, response, err = dialChat(ts.URL, cookie, strings.Repeat("2", 32), "", "", "https://evil.example", auth)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin websocket = %#v, %v", response, err)
	}
	_, response, err = dialChat(ts.URL, cookie, strings.Repeat("2", 32), "", "", strings.Replace(ts.URL, "http://", "https://", 1), auth)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-scheme websocket = %#v, %v", response, err)
	}
	_, response, err = dialChat(ts.URL, cookie, strings.Repeat("2", 32), "", "", ts.URL+"/path", auth)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("origin with path = %#v, %v", response, err)
	}
	_, response, err = dialChat(ts.URL, cookie, strings.Repeat("2", 32), "", "", "", auth)
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("originless websocket = %#v, %v", response, err)
	}

	conn, response, err := dialChat(ts.URL, cookie, strings.Repeat("2", 32), "", "", ts.URL, auth)
	if err != nil {
		t.Fatalf("authenticated websocket: %v (response %#v)", err, response)
	}
	defer conn.Close()
	if ready := readChatWire(t, conn); ready.Type != "ready" {
		t.Fatalf("first message = %#v", ready)
	}
}

func TestChatRejectsUnsafeAndOversizedInput(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)
	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	conn, _, err := dialChat(ts.URL, cookie, strings.Repeat("3", 32), "", "", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = readChatWire(t, conn)

	for _, message := range []chatClientMessage{
		{Type: "message", Text: "bad\x00text"},
		{Type: "message", Text: strings.Repeat("界", chatMaxMessageRunes+1)},
	} {
		if err = conn.WriteJSON(message); err != nil {
			t.Fatal(err)
		}
		wire := readChatWire(t, conn)
		if wire.Type != "error" || wire.Text == "" {
			t.Fatalf("unsafe input response = %#v", wire)
		}
	}
	if err = conn.WriteJSON(chatClientMessage{Type: "name", Name: "bad\x00name"}); err != nil {
		t.Fatal(err)
	}
	if wire := readChatWire(t, conn); wire.Type != "name" || wire.Name != "127.0.0.1" {
		t.Fatalf("legacy rename changed IP identity: %#v", wire)
	}
	items := s.ChatSnapshot()
	if len(items) != 1 || len(items[0].Messages) != 0 || strings.ContainsRune(items[0].Name, 0) {
		t.Fatalf("unsafe input reached snapshot: %#v", items)
	}
}

func TestChatAcceptsConfiguredLongTextLimit(t *testing.T) {
	want := strings.Repeat("界", chatMaxMessageRunes)
	got, err := cleanChatText(want)
	if err != nil {
		t.Fatalf("message at configured limit was rejected: %v", err)
	}
	if got != want {
		t.Fatalf("message at configured limit changed: got %d runes, want %d", utf8.RuneCountInString(got), chatMaxMessageRunes)
	}
	if _, err = cleanChatText(want + "界"); err == nil {
		t.Fatal("message above configured limit was accepted")
	}
}

func TestChatWorksOverWSS(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	ts := httptest.NewTLSServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)
	cookie, enabled := fetchChatStatus(t, ts.Client(), ts.URL, "")
	if !enabled || cookie == nil || !cookie.Secure {
		t.Fatalf("TLS status/cookie = enabled %v, cookie %#v", enabled, cookie)
	}
	transport := ts.Client().Transport.(*http.Transport)
	dialer := &websocket.Dialer{TLSClientConfig: transport.TLSClientConfig.Clone()}
	conn, response, err := dialChat(ts.URL, cookie, strings.Repeat("4", 32), "TLS 访客", "", ts.URL, nil, dialer)
	if err != nil {
		t.Fatalf("wss dial: %v (response %#v)", err, response)
	}
	defer conn.Close()
	if ready := readChatWire(t, conn); ready.Type != "ready" || ready.ClientID != "127.0.0.1" || ready.Name != ready.ClientID {
		t.Fatalf("wss ready = %#v", ready)
	}
}

func TestStopClosesChatAndAllowsRestart(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	address, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := "http://" + address
	cookie, _ := fetchChatStatus(t, http.DefaultClient, baseURL, "")
	conn, response, err := dialChat(baseURL, cookie, strings.Repeat("5", 32), "", "", baseURL, nil)
	if err != nil {
		t.Fatalf("dial before stop: %v (response %#v)", err, response)
	}
	_ = readChatWire(t, conn)

	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop() }()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err = conn.ReadMessage(); err == nil {
		t.Fatal("websocket remained open after Stop")
	} else if !websocket.IsCloseError(err, websocket.CloseGoingAway) {
		t.Fatalf("Stop close error = %v", err)
	}
	select {
	case err = <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Stop did not wait for chat handlers")
	}
	if s.ChatOnlineCount() != 0 {
		t.Fatalf("online count after Stop = %d", s.ChatOnlineCount())
	}
	s.chat.mu.RLock()
	active, accepting, idle := s.chat.active, s.chat.accepting, s.chat.idle
	s.chat.mu.RUnlock()
	if active != 0 || accepting {
		t.Fatalf("chat lifecycle after Stop: active=%d accepting=%v", active, accepting)
	}
	select {
	case <-idle:
	default:
		t.Fatal("chat idle signal remained open after Stop")
	}

	address, err = s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	baseURL = "http://" + address
	cookie, enabled := fetchChatStatus(t, http.DefaultClient, baseURL, "")
	if !enabled {
		t.Fatal("chat setting was lost across restart")
	}
	conn, response, err = dialChat(baseURL, cookie, strings.Repeat("6", 32), "", "", baseURL, nil)
	if err != nil {
		t.Fatalf("dial after restart: %v (response %#v)", err, response)
	}
	defer conn.Close()
	if ready := readChatWire(t, conn); ready.Type != "ready" {
		t.Fatalf("ready after restart = %#v", ready)
	}
}

func TestChatGenerationAndHandlerLimit(t *testing.T) {
	hub := newChatHub()
	hub.setEnabled(true)
	generations := make([]uint64, 0, chatMaxConnections)
	for i := 0; i < chatMaxConnections; i++ {
		generation, ok := hub.beginHandler()
		if !ok {
			t.Fatalf("handler %d was rejected before the limit", i)
		}
		generations = append(generations, generation)
	}
	if _, ok := hub.beginHandler(); ok {
		t.Fatal("connection limit was not enforced before upgrade")
	}
	for range generations {
		hub.endHandler()
	}

	generation, ok := hub.beginHandler()
	if !ok {
		t.Fatal("handler reservation failed")
	}
	hub.setEnabled(false)
	hub.setEnabled(true)
	peer := &chatPeer{id: strings.Repeat("a", 32), ip: "192.0.2.1", send: make(chan chatWireMessage, 1), closed: make(chan struct{})}
	if hub.register(peer, generation) {
		t.Fatal("a handler from before disable/enable crossed the generation boundary")
	}
	hub.endHandler()

	generation, ok = hub.beginHandler()
	if !ok {
		t.Fatal("handler reservation before mode switch failed")
	}
	hub.setGroupEnabled(true)
	peer = &chatPeer{id: strings.Repeat("d", 32), ip: "192.0.2.2", send: make(chan chatWireMessage, 1), closed: make(chan struct{})}
	if hub.register(peer, generation) {
		t.Fatal("a handler crossed the private/group generation boundary")
	}
	hub.endHandler()

	if hub.active != 0 {
		t.Fatalf("active handlers = %d", hub.active)
	}
	select {
	case <-hub.idle:
	default:
		t.Fatal("idle channel was not closed")
	}
}

func TestChatRejectsStaleAccessVersion(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	oldVersion := s.accessVersion
	s.SetAccess("new-password", false, true, false)
	request := httptest.NewRequest(http.MethodGet, "http://example.test/__hfs/chat/ws?tab="+strings.Repeat("b", 32), nil)
	request.Host = "example.test"
	request.Header.Set("Origin", "http://example.test")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.AddCookie(&http.Cookie{Name: chatCookieName, Value: strings.Repeat("c", 32)})
	recorder := httptest.NewRecorder()
	if status := s.handleChatWebSocket(recorder, request, oldVersion); status != http.StatusUnauthorized {
		t.Fatal("stale access version upgraded")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("stale access status = %d", recorder.Code)
	}
}

func TestChatNotifierIsLockFreeAndHistoryIsBounded(t *testing.T) {
	s := New(io.Discard)
	notified := make(chan struct{}, 1)
	s.SetChatNotifier(func() {
		_ = s.ChatSnapshot()
		select {
		case notified <- struct{}{}:
		default:
		}
	})
	s.SetChatEnabled(true)
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("chat notifier was not called or deadlocked")
	}

	conversation := &chatConversationState{id: "bounded", name: "访客"}
	for i := 0; i < chatMaxHistory+25; i++ {
		appendChatMessage(conversation, ChatMessage{ID: fmt.Sprint(i), Text: fmt.Sprint(i), SentAt: time.Unix(int64(i), 0)})
	}
	if len(conversation.messages) != chatMaxHistory || conversation.messages[0].Text != "25" {
		t.Fatalf("bounded history = len %d, first %#v", len(conversation.messages), conversation.messages[0])
	}
}

func TestChatOverviewAndConversationSnapshot(t *testing.T) {
	s := New(io.Discard)
	imageData := encodeTestImage(t, "png", 2, 2)
	olderID, newerID := "192.0.2.10", "192.0.2.20"

	s.chat.mu.Lock()
	older := &chatConversationState{id: olderID, name: olderID}
	appendChatMessage(older, ChatMessage{
		ID:       "image",
		Kind:     ChatMessageKindImage,
		Sender:   "user",
		ClientID: olderID,
		Name:     olderID,
		Text:     "图片说明",
		Mime:     "image/png",
		Data:     imageData,
		SentAt:   time.Unix(10, 0),
	})
	newer := &chatConversationState{id: newerID, name: newerID}
	appendChatMessage(newer, ChatMessage{
		ID:       "text",
		Kind:     ChatMessageKindText,
		Sender:   "user",
		ClientID: newerID,
		Name:     newerID,
		Text:     "保留文本",
		SentAt:   time.Unix(20, 0),
	})
	s.chat.conversations[olderID] = older
	s.chat.conversations[newerID] = newer
	s.chat.mu.Unlock()

	overview := s.ChatOverview()
	if len(overview) != 2 || overview[0].ID != newerID || overview[1].ID != olderID {
		t.Fatalf("overview ordering = %#v", overview)
	}
	imageMetadata := overview[1].Messages[0]
	if imageMetadata.ID != "image" || imageMetadata.Kind != ChatMessageKindImage ||
		imageMetadata.ClientID != olderID || imageMetadata.Name != olderID ||
		imageMetadata.Text != "图片说明" || imageMetadata.Mime != "image/png" ||
		!imageMetadata.SentAt.Equal(time.Unix(10, 0)) || imageMetadata.Data != nil {
		t.Fatalf("overview image metadata = %#v", imageMetadata)
	}
	if overview[0].Messages[0].Text != "保留文本" || overview[0].Messages[0].Data != nil {
		t.Fatalf("overview text message = %#v", overview[0].Messages[0])
	}

	full, ok := s.ChatConversationSnapshot(olderID)
	if !ok || full.ID != olderID || len(full.Messages) != 1 || !bytes.Equal(full.Messages[0].Data, imageData) {
		t.Fatalf("full conversation snapshot = %#v, %v", full, ok)
	}
	full.Messages[0].Data[0] ^= 0xff
	full.Messages[0].Text = "外部修改"
	again, ok := s.ChatConversationSnapshot(olderID)
	if !ok || !bytes.Equal(again.Messages[0].Data, imageData) || again.Messages[0].Text != "图片说明" {
		t.Fatal("single conversation snapshot was not deeply copied")
	}
	all := s.ChatSnapshot()
	if len(all) != 2 || !bytes.Equal(all[1].Messages[0].Data, imageData) {
		t.Fatalf("ChatSnapshot lost its full-copy behavior: %#v", all)
	}
	all[1].Messages[0].Data[0] ^= 0xff
	if got, _ := s.ChatConversationSnapshot(olderID); !bytes.Equal(got.Messages[0].Data, imageData) {
		t.Fatal("ChatSnapshot image data shared storage with the hub")
	}
	if _, ok = s.ChatConversationSnapshot(ChatGroupConversationID); ok {
		t.Fatal("group conversation was visible in private mode")
	}
	if _, ok = s.ChatConversationSnapshot("missing"); ok {
		t.Fatal("missing private conversation was reported")
	}

	s.SetGroupChatEnabled(true)
	s.chat.mu.Lock()
	appendChatMessage(s.chat.group, ChatMessage{
		ID:       "group-image",
		Kind:     ChatMessageKindImage,
		Sender:   "user",
		ClientID: olderID,
		Name:     olderID,
		Mime:     "image/png",
		Data:     imageData,
		SentAt:   time.Unix(30, 0),
	})
	s.chat.mu.Unlock()
	overview = s.ChatOverview()
	if len(overview) != 1 || overview[0].ID != ChatGroupConversationID ||
		len(overview[0].Messages) != 1 || overview[0].Messages[0].Data != nil {
		t.Fatalf("group overview = %#v", overview)
	}
	group, ok := s.ChatConversationSnapshot(ChatGroupConversationID)
	if !ok || len(group.Messages) != 1 || !bytes.Equal(group.Messages[0].Data, imageData) {
		t.Fatalf("group conversation snapshot = %#v, %v", group, ok)
	}
	privateInGroup, ok := s.ChatConversationSnapshot(olderID)
	if !ok || privateInGroup.ID != olderID || len(privateInGroup.Messages) != 1 {
		t.Fatalf("administrator private conversation was not available in group mode: %#v, %v", privateInGroup, ok)
	}

	s.SetGroupChatEnabled(false)
	if _, ok = s.ChatConversationSnapshot(ChatGroupConversationID); ok {
		t.Fatal("group conversation remained visible after returning to private mode")
	}
	if _, ok = s.ChatConversationSnapshot(olderID); !ok {
		t.Fatal("private conversation was not restored after leaving group mode")
	}
}

func TestChatSnapshotsAreRaceSafeDuringModeChanges(t *testing.T) {
	s := New(io.Discard)
	id := strings.Repeat("3", 32)
	s.chat.mu.Lock()
	conversation := &chatConversationState{id: id, name: "并发会话"}
	appendChatMessage(conversation, ChatMessage{
		ID:     "message",
		Kind:   ChatMessageKindText,
		Sender: "user",
		Text:   "并发读取",
		SentAt: time.Now().UTC(),
	})
	s.chat.conversations[id] = conversation
	s.chat.mu.Unlock()

	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		for i := 0; i < 250; i++ {
			s.SetGroupChatEnabled(i%2 == 0)
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 500; i++ {
			_ = s.ChatOverview()
			_ = s.ChatSnapshot()
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 500; i++ {
			_, _ = s.ChatConversationSnapshot(id)
			_, _ = s.ChatConversationSnapshot(ChatGroupConversationID)
		}
	}()
	workers.Wait()
}

func TestChatImageValidationAndHistoryBudget(t *testing.T) {
	pngData := encodeTestImage(t, "png", 2, 2)
	mime, copied, err := cleanChatImage(" IMAGE/PNG ", pngData)
	if err != nil || mime != "image/png" || !bytes.Equal(copied, pngData) {
		t.Fatalf("valid PNG = mime %q, data equal %v, err %v", mime, bytes.Equal(copied, pngData), err)
	}
	copied[0] ^= 0xff
	if bytes.Equal(copied, pngData) {
		t.Fatal("cleanChatImage did not copy its input")
	}

	jpegData := encodeTestImage(t, "jpeg", 2, 2)
	if mime, _, err = cleanChatImage("image/jpeg", jpegData); err != nil || mime != "image/jpeg" {
		t.Fatalf("valid JPEG = mime %q, err %v", mime, err)
	}
	for name, test := range map[string]struct {
		mime string
		data []byte
	}{
		"empty":         {mime: "image/png"},
		"unsupported":   {mime: "image/gif", data: pngData},
		"mime mismatch": {mime: "image/jpeg", data: pngData},
		"malformed":     {mime: "image/png", data: []byte("not an image")},
		"too many bytes": {
			mime: "image/png",
			data: append(append([]byte(nil), pngData...), make([]byte, chatMaxImageBytes+1-len(pngData))...),
		},
		"too wide": {mime: "image/png", data: encodeTestImage(t, "png", chatMaxImageDimension+1, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := cleanChatImage(test.mime, test.data); err == nil {
				t.Fatal("invalid image was accepted")
			}
		})
	}

	conversation := &chatConversationState{id: "images", name: "图片历史"}
	for i := 0; i < 5; i++ {
		appendChatMessage(conversation, ChatMessage{
			ID:     fmt.Sprint(i),
			Kind:   ChatMessageKindImage,
			Mime:   "image/png",
			Data:   make([]byte, chatMaxImageBytes),
			SentAt: time.Unix(int64(i), 0),
		})
	}
	if len(conversation.messages) != 4 || conversation.messages[0].ID != "1" {
		t.Fatalf("image history eviction = len %d, first %#v", len(conversation.messages), conversation.messages[0])
	}
	if conversation.historyBytes > chatMaxHistoryBytes {
		t.Fatalf("image history retained %d bytes, limit %d", conversation.historyBytes, chatMaxHistoryBytes)
	}
}

func TestGroupChatBroadcastImagesAndIndependentHistory(t *testing.T) {
	s := New(io.Discard)
	s.SetGroupChatEnabled(true)
	s.SetChatEnabled(true)
	ts := httptest.NewServer(s)
	defer ts.Close()
	defer s.SetChatEnabled(false)

	cookie, _ := fetchChatStatus(t, http.DefaultClient, ts.URL, "")
	headersA := make(http.Header)
	headersA.Set("X-Forwarded-For", "192.0.2.71")
	headersB := make(http.Header)
	headersB.Set("X-Forwarded-For", "192.0.2.72")
	connA, response, err := dialChat(ts.URL, cookie, strings.Repeat("7", 32), "小明", "", ts.URL, headersA)
	if err != nil {
		t.Fatalf("dial A: %v (response %#v)", err, response)
	}
	defer connA.Close()
	readyA := readChatWire(t, connA)
	connB, response, err := dialChat(ts.URL, cookie, strings.Repeat("8", 32), "小红", "", ts.URL, headersB)
	if err != nil {
		t.Fatalf("dial B: %v (response %#v)", err, response)
	}
	defer connB.Close()
	readyB := readChatWire(t, connB)
	if !readyA.Group || !readyB.Group || readyA.ClientID != "192.0.2.71" ||
		readyB.ClientID != "192.0.2.72" || readyA.ClientID == readyB.ClientID ||
		readyA.Name != readyA.ClientID || readyB.Name != readyB.ClientID {
		t.Fatalf("group ready messages = A %#v, B %#v", readyA, readyB)
	}

	if err = connA.WriteJSON(chatClientMessage{Type: "message", Kind: ChatMessageKindText, Text: "大家好"}); err != nil {
		t.Fatal(err)
	}
	for label, wire := range map[string]chatWireMessage{
		"A": readChatWire(t, connA),
		"B": readChatWire(t, connB),
	} {
		if wire.Type != "message" || !wire.Group || wire.Kind != ChatMessageKindText ||
			wire.ClientID != readyA.ClientID || wire.Name != readyA.ClientID || wire.Text != "大家好" {
			t.Fatalf("%s group text = %#v", label, wire)
		}
	}

	pngData := encodeTestImage(t, "png", 3, 2)
	if err = connB.WriteJSON(chatClientMessage{
		Type: ChatMessageKindImage,
		Mime: "image/png",
		Data: pngData,
	}); err != nil {
		t.Fatal(err)
	}
	for label, wire := range map[string]chatWireMessage{
		"A": readChatWire(t, connA),
		"B": readChatWire(t, connB),
	} {
		if wire.Type != "message" || wire.Kind != ChatMessageKindImage || wire.ClientID != readyB.ClientID ||
			wire.Name != readyB.ClientID || wire.Mime != "image/png" || !bytes.Equal(wire.Data, pngData) {
			t.Fatalf("%s group image = %#v", label, wire)
		}
	}

	if err = s.SendChatMessage(ChatGroupConversationID, "管理员通知"); err != nil {
		t.Fatal(err)
	}
	for label, wire := range map[string]chatWireMessage{
		"A": readChatWire(t, connA),
		"B": readChatWire(t, connB),
	} {
		if wire.Kind != ChatMessageKindText || wire.Sender != "admin" || wire.ClientID != "admin" ||
			wire.Name != "管理员" || wire.Text != "管理员通知" {
			t.Fatalf("%s admin group text = %#v", label, wire)
		}
	}

	adminImage := append([]byte(nil), pngData...)
	if err = s.SendChatImage(ChatGroupConversationID, "image/png", adminImage); err != nil {
		t.Fatal(err)
	}
	adminImage[0] ^= 0xff
	for label, wire := range map[string]chatWireMessage{
		"A": readChatWire(t, connA),
		"B": readChatWire(t, connB),
	} {
		if wire.Kind != ChatMessageKindImage || wire.Sender != "admin" || !bytes.Equal(wire.Data, pngData) {
			t.Fatalf("%s admin group image = %#v", label, wire)
		}
	}

	snapshot := s.ChatSnapshot()
	if len(snapshot) != 1 || snapshot[0].ID != ChatGroupConversationID || !snapshot[0].Online || len(snapshot[0].Messages) != 4 {
		t.Fatalf("group snapshot = %#v", snapshot)
	}
	snapshot[0].Messages[1].Data[0] ^= 0xff
	if got := s.ChatSnapshot()[0].Messages[1].Data; !bytes.Equal(got, pngData) {
		t.Fatal("ChatSnapshot did not deep-copy image bytes")
	}

	s.SetGroupChatEnabled(false)
	for label, conn := range map[string]*websocket.Conn{"A": connA, "B": connB} {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, _, err = conn.ReadMessage(); !websocket.IsCloseError(err, chatCloseModeChanged) {
			t.Fatalf("%s mode switch close = %v", label, err)
		}
	}
	privateSnapshot := s.ChatSnapshot()
	if len(privateSnapshot) != 2 {
		t.Fatalf("private snapshot after group mode = %#v", privateSnapshot)
	}
	for _, conversation := range privateSnapshot {
		if len(conversation.Messages) != 0 {
			t.Fatalf("group history leaked into private conversation: %#v", conversation)
		}
	}

	s.SetGroupChatEnabled(true)
	if snapshot = s.ChatSnapshot(); len(snapshot) != 1 || len(snapshot[0].Messages) != 4 {
		t.Fatalf("group history was not preserved across mode switches: %#v", snapshot)
	}
	connC, response, err := dialChat(ts.URL, cookie, strings.Repeat("7", 32), "", "", ts.URL, headersA)
	if err != nil {
		t.Fatalf("redial group: %v (response %#v)", err, response)
	}
	defer connC.Close()
	readyC := readChatWire(t, connC)
	if !readyC.Group || readyC.ClientID != readyA.ClientID || readyC.Name != readyA.ClientID || len(readyC.History) != 4 {
		t.Fatalf("group reconnect ready = %#v", readyC)
	}
}

func TestForwardedHTTPSOnlyTrustedFromLoopback(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://files.example/__hfs/chat/ws", nil)
	request.Host = "files.example"
	request.Header.Set("Origin", "https://files.example")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.RemoteAddr = "192.0.2.10:4567"
	if sameChatOrigin(request) {
		t.Fatal("untrusted remote address spoofed X-Forwarded-Proto")
	}
	request.RemoteAddr = "127.0.0.1:4567"
	if !sameChatOrigin(request) {
		t.Fatal("loopback TLS reverse proxy was not accepted")
	}
}

func fetchChatStatus(t *testing.T, client *http.Client, baseURL, authorization string) (*http.Cookie, bool) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, baseURL+"/__hfs/chat/status", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d", response.StatusCode)
	}
	var payload struct {
		Enabled bool `json:"enabled"`
	}
	if err = json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == chatCookieName {
			return cookie, payload.Enabled
		}
	}
	t.Fatal("chat status did not set a session cookie")
	return nil, false
}

func dialChat(baseURL string, cookie *http.Cookie, tab, name, authorization, origin string, headers http.Header, customDialer ...*websocket.Dialer) (*websocket.Conn, *http.Response, error) {
	websocketURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/__hfs/chat/ws?tab=" + tab
	if name != "" {
		websocketURL += "&name=" + urlQueryEscape(name)
	}
	requestHeaders := make(http.Header)
	for key, values := range headers {
		requestHeaders[key] = append([]string(nil), values...)
	}
	if cookie != nil {
		requestHeaders.Set("Cookie", cookie.String())
	}
	if authorization != "" {
		requestHeaders.Set("Authorization", authorization)
	}
	if origin != "" {
		requestHeaders.Set("Origin", origin)
	}
	dialer := websocket.DefaultDialer
	if len(customDialer) > 0 {
		dialer = customDialer[0]
	}
	return dialer.Dial(websocketURL, requestHeaders)
}

func urlQueryEscape(value string) string {
	return url.QueryEscape(value)
}

func readChatWire(t *testing.T, conn *websocket.Conn) chatWireMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var wire chatWireMessage
	if err := conn.ReadJSON(&wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func encodeTestImage(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	pixels := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixels.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}
	var buffer bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buffer, pixels)
	case "jpeg":
		err = jpeg.Encode(&buffer, pixels, &jpeg.Options{Quality: 80})
	default:
		t.Fatalf("unknown test image format %q", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestCleanChatText(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		err   bool
	}{
		{input: "  hello\r\nworld  ", want: "hello\nworld"},
		{input: "\x00", err: true},
		{input: "\u0007", err: true},
		{input: "", err: true},
	} {
		t.Run(fmt.Sprintf("%q", test.input), func(t *testing.T) {
			got, err := cleanChatText(test.input)
			if (err != nil) != test.err || got != test.want {
				t.Fatalf("cleanChatText(%q) = %q, %v", test.input, got, err)
			}
		})
	}
}
