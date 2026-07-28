package gui

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"hfsgo/internal/appinfo"
)

func TestCompactLogoKeepsModernHTMLBelowWebViewLimit(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	for y := 0; y < 1024; y++ {
		for x := 0; x < 1024; x++ {
			source.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x),
				G: uint8(y),
				B: uint8(x ^ y),
				A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	html := renderModernHTML(encoded.Bytes(), nil)
	if strings.Contains(html, "{{LOGO}}") {
		t.Fatal("logo placeholder was not replaced")
	}
	// NavigateToString has a finite payload limit. Keeping the complete page
	// comfortably below 500 KiB avoids a blank WebView even with UTF-16
	// accounting and future UI additions.
	if len(html) >= 500<<10 {
		t.Fatalf("modern HTML is too large: %d bytes", len(html))
	}

	compact, err := compactLogoPNG(encoded.Bytes(), 192)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(compact))
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Bounds().Size(); got.X != 192 || got.Y != 192 {
		t.Fatalf("compact logo size = %v, want 192x192", got)
	}
}

func TestModernPagesDoNotOverrideInactiveVisibility(t *testing.T) {
	if !strings.Contains(modernHTML, `.files-page{display:none;`) {
		t.Fatal("file page must remain hidden while another tab is active")
	}
	if !strings.Contains(modernHTML, `.files-page.active{display:grid}`) {
		t.Fatal("active file page must restore its grid layout")
	}
}

func TestModernAddressCopyUsesNativeBinding(t *testing.T) {
	for _, marker := range []string{
		`id="copy-address"`,
		`id="copy-https-address"`,
		`window.hfsCopyText(value)`,
		`copyServerAddress(state&&state.address,'HTTP')`,
		`copyServerAddress(state&&state.httpsAddress,'HTTPS')`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("native address copy UI is missing %q", marker)
		}
	}
	if strings.Contains(modernHTML, `navigator.clipboard`) {
		t.Fatal("copy button still depends on the WebView clipboard permission")
	}
}

func TestModernHTTPSControlsAndBindings(t *testing.T) {
	for _, marker := range []string{
		`id="https-port"`,
		`id="generate-certificate"`,
		`id="certificate-state"`,
		`id="certificate-note"`,
		`id="https-server-address"`,
		`httpsPort:$('https-port').value.trim()`,
		`window.hfsToggleServer($('access-host').value,$('port').value.trim(),$('https-port').value.trim())`,
		`window.hfsGenerateCertificate()`,
		`clearTimeout(saveTimer);state=await window.hfsSaveSettings(getSettingsFromForm())`,
		`state.httpsAddress`,
		`certificate.available`,
		`id="redirect-to-https"`,
		`redirectToHTTPS:$('redirect-to-https').checked`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("modern HTTPS UI is missing %q", marker)
		}
	}
}

func TestModernImageViewerNativeDropAndAboutPage(t *testing.T) {
	for _, marker := range []string{
		`id="image-viewer"`,
		`id="image-viewer-prev"`,
		`id="image-viewer-next"`,
		`class="chat-preview-image"`,
		`event.key==='ArrowUp'`,
		`event.key==='ArrowDown'`,
		`id="native-drop-zone"`,
		`window.hfsSetDropZone(`,
		`版本 {{VERSION}}`,
		`这是一个轻量、安全的局域网文件分享与聊天系统`,
		`https://github.com/aceggbond/LanChatGo`,
		`id="check-update"`,
		`window.hfsCheckUpdate()`,
		`src="{{DONATION}}"`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("modern UI is missing %q", marker)
		}
	}
	if strings.Contains(modernHTML, `id="allow-manage"`) || strings.Contains(modernHTML, `allowManage:`) {
		t.Fatal("removed browser-management setting is still visible")
	}
}

func TestRealEmbeddedImagesKeepModernHTMLBelowWebViewLimit(t *testing.T) {
	logo, err := os.ReadFile(filepath.Join("..", "..", "logo.png"))
	if err != nil {
		t.Fatal(err)
	}
	donation, err := os.ReadFile(filepath.Join("..", "..", "dashang.png"))
	if err != nil {
		t.Fatal(err)
	}
	html := renderModernHTML(logo, donation)
	if strings.Contains(html, "{{LOGO}}") || strings.Contains(html, "{{DONATION}}") {
		t.Fatal("embedded image placeholders were not replaced")
	}
	if len(html) >= 500<<10 {
		t.Fatalf("modern HTML with embedded images is too large: %d bytes", len(html))
	}
	if !strings.Contains(html, "版本 "+appinfo.Version) {
		t.Fatalf("rendered about page is missing version %s", appinfo.Version)
	}
}

func TestModernFileContextMenuAndTemporaryDeleteConfirmation(t *testing.T) {
	for _, marker := range []string{
		`id="file-context-menu"`,
		`data-action="open"`,
		`data-action="reveal"`,
		`data-action="copy-path"`,
		`data-action="rename"`,
		`data-action="remove"`,
		`data-action="add-file"`,
		`data-action="add-folder"`,
		`window.hfsRevealShare(index)`,
		`window.hfsRenameShare(index,name.trim())`,
		`items.filter(function(item){return item.temporary})`,
		`是否同时删除临时文件？`,
		`window.hfsRemoveShares(indices,deleteTemporary)`,
		`window.hfsSetActivePage(name)`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("modern file-management UI is missing %q", marker)
		}
	}
}

func TestModernOfflineVisitorCanBeRemovedWithoutConversationError(t *testing.T) {
	for _, marker := range []string{
		`if(force&&!userByIP(id))showError(error)`,
		`remove.disabled=false;rename.disabled=false`,
		`window.hfsRemoveVisitor(selectedChat)`,
		`.chat-actions{margin-left:auto;display:flex`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("modern visitor UI is missing %q", marker)
		}
	}
}

func TestModernAdministratorPrivateChatAndCompactActions(t *testing.T) {
	for _, marker := range []string{
		`privateBlocked=!group&&state.settings.groupChat&&!state.settings.allowPrivateChat`,
		`state.settings.groupChat?'管理员私信 · '`,
		`.chat-actions .button{height:34px`,
		`.chat-header{display:flex;align-items:center;gap:12px;padding:13px 16px`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("modern administrator private-chat UI is missing %q", marker)
		}
	}
}

func TestModernChatUnreadPulseAndInstantUserSearch(t *testing.T) {
	for _, marker := range []string{
		`id="visitor-search"`,
		`placeholder="搜索 IP、姓名或拼音"`,
		`$('visitor-search').addEventListener('input',renderVisitors)`,
		`function observeChatActivity(items)`,
		`chatUnread[id]=(chatUnread[id]||0)+increase`,
		`class="unread-count"`,
		`@keyframes visitor-unread-pulse`,
		`user&&user.searchKey`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("modern unread/search UI is missing %q", marker)
		}
	}
}

func TestModernJavaScriptDOMReferencesExist(t *testing.T) {
	idPattern := regexp.MustCompile(`\bid="([^"]+)"`)
	referencePattern := regexp.MustCompile(`\$\('([^']+)'\)`)

	ids := make(map[string]int)
	for _, match := range idPattern.FindAllStringSubmatch(modernHTML, -1) {
		ids[match[1]]++
	}
	for id, count := range ids {
		if count != 1 {
			t.Errorf("DOM id %q occurs %d times", id, count)
		}
	}
	for _, match := range referencePattern.FindAllStringSubmatch(modernHTML, -1) {
		if ids[match[1]] == 0 {
			t.Errorf("JavaScript references missing DOM id %q", match[1])
		}
	}
}

func TestModernOperationLogUsesExistingStateAndClearBinding(t *testing.T) {
	for _, marker := range []string{
		`id="show-logs"`,
		`id="log-content"`,
		`content.textContent=state.logs`,
		`window.hfsClearLogs()`,
		`访问、上传、下载、文件管理和聊天连接均记录来源 IP`,
	} {
		if !strings.Contains(modernHTML, marker) {
			t.Fatalf("modern operation log UI is missing %q", marker)
		}
	}
}
