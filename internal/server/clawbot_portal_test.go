package server

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestClawBotPortalJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	var page bytes.Buffer
	err = portalTemplate.Execute(&page, pageData{
		Chat: true, UserList: true, PrivateChat: true, LayoutClass: "layout-chat-users", Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	html := page.String()
	start, end := strings.LastIndex(html, "<script>"), strings.LastIndex(html, "</script>")
	if start < 0 || end <= start {
		t.Fatal("portal script not found")
	}
	script := html[start+len("<script>") : end]
	file, err := os.CreateTemp(t.TempDir(), "tinychatgo-portal-*.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(script); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if output, checkErr := exec.Command(node, "--check", file.Name()).CombinedOutput(); checkErr != nil {
		t.Fatalf("portal JavaScript syntax: %v\n%s", checkErr, output)
	}
}

func TestClawBotAdminJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	start, end := strings.LastIndex(adminFullPageHTML, "<script>"), strings.LastIndex(adminFullPageHTML, "</script>")
	if start < 0 || end <= start {
		t.Fatal("administrator script not found")
	}
	file, err := os.CreateTemp(t.TempDir(), "tinychatgo-admin-*.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(adminFullPageHTML[start+len("<script>") : end]); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if output, checkErr := exec.Command(node, "--check", file.Name()).CombinedOutput(); checkErr != nil {
		t.Fatalf("administrator JavaScript syntax: %v\n%s", checkErr, output)
	}
}

func TestClawBotPortalAndAdminControlsPresent(t *testing.T) {
	var page bytes.Buffer
	if err := portalTemplate.Execute(&page, pageData{Chat: true, UserList: true, PrivateChat: true}); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"微信 ClawBot", "/__hfs/clawbot/qr", "/__hfs/clawbot/send-media", "claw-forward", "claw-unbind"} {
		if !strings.Contains(page.String(), marker) {
			t.Fatalf("portal is missing %q", marker)
		}
	}
	if !strings.Contains(adminFullPageHTML, "/__admin/clawbot/unbind") || !strings.Contains(adminFullPageHTML, "强制解绑微信") {
		t.Fatal("administrator ClawBot controls are missing")
	}
}

func TestClawBotAttachmentVisibleOnlyToBoundAccount(t *testing.T) {
	owner := "u_0123456789abcdef0123456789abcdef"
	other := "u_fedcba9876543210fedcba9876543210"
	if !chatConversationVisibleToIP("clawbot:"+owner, owner) {
		t.Fatal("owner cannot access ClawBot attachment")
	}
	if chatConversationVisibleToIP("clawbot:"+owner, other) {
		t.Fatal("another account can access ClawBot attachment")
	}
}
