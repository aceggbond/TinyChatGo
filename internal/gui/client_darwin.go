//go:build darwin

package gui

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -fblocks
#cgo LDFLAGS: -framework Cocoa -framework WebKit -framework Security

#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <Security/Security.h>
#import <dispatch/dispatch.h>
#include <stdlib.h>

@interface LCGClientDelegate : NSObject <NSWindowDelegate, WKNavigationDelegate>
@property(nonatomic, weak) NSWindow *window;
@end

static LCGClientDelegate *lcgClientDelegate;
static NSStatusItem *lcgStatusItem;

@implementation LCGClientDelegate
- (BOOL)windowShouldClose:(NSWindow *)sender {
  [sender orderOut:nil];
  return NO;
}
- (void)showWindow:(id)sender {
  [self.window makeKeyAndOrderFront:nil];
  [NSApp activateIgnoringOtherApps:YES];
}
- (void)quitClient:(id)sender {
  [NSApp terminate:nil];
}
- (void)webView:(WKWebView *)webView
    didReceiveAuthenticationChallenge:(NSURLAuthenticationChallenge *)challenge
                    completionHandler:(void (^)(NSURLSessionAuthChallengeDisposition disposition,
                                                NSURLCredential *credential))completionHandler {
  if ([challenge.protectionSpace.authenticationMethod isEqualToString:NSURLAuthenticationMethodServerTrust] &&
      challenge.protectionSpace.serverTrust != nil) {
    completionHandler(NSURLSessionAuthChallengeUseCredential,
                      [NSURLCredential credentialForTrust:challenge.protectionSpace.serverTrust]);
    return;
  }
  completionHandler(NSURLSessionAuthChallengePerformDefaultHandling, nil);
}
@end

static void LCGConfigureClient(void *windowPtr, const void *logoBytes, int logoLength) {
  NSWindow *window = (__bridge NSWindow *)windowPtr;
  if (window == nil) {
    return;
  }
  lcgClientDelegate = [LCGClientDelegate new];
  lcgClientDelegate.window = window;
  window.delegate = lcgClientDelegate;
  if ([window.contentView isKindOfClass:[WKWebView class]]) {
    ((WKWebView *)window.contentView).navigationDelegate = lcgClientDelegate;
  }

  lcgStatusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
  if (logoBytes != NULL && logoLength > 0) {
    NSData *data = [NSData dataWithBytes:logoBytes length:(NSUInteger)logoLength];
    NSImage *image = [[NSImage alloc] initWithData:data];
    image.size = NSMakeSize(18, 18);
    image.template = NO;
    lcgStatusItem.button.image = image;
  }
  lcgStatusItem.button.toolTip = @"LanChatGo 客户端";
  NSMenu *menu = [NSMenu new];
  NSMenuItem *showItem = [[NSMenuItem alloc] initWithTitle:@"显示 LanChatGo"
                                                   action:@selector(showWindow:)
                                            keyEquivalent:@""];
  showItem.target = lcgClientDelegate;
  [menu addItem:showItem];
  [menu addItem:[NSMenuItem separatorItem]];
  NSMenuItem *quitItem = [[NSMenuItem alloc] initWithTitle:@"退出"
                                                   action:@selector(quitClient:)
                                            keyEquivalent:@""];
  quitItem.target = lcgClientDelegate;
  [menu addItem:quitItem];
  lcgStatusItem.menu = menu;
}

static void LCGNotify(const char *titleText, const char *bodyText) {
  NSString *title = titleText ? [NSString stringWithUTF8String:titleText] : @"LanChatGo";
  NSString *body = bodyText ? [NSString stringWithUTF8String:bodyText] : @"";
  dispatch_async(dispatch_get_main_queue(), ^{
    NSUserNotification *notification = [NSUserNotification new];
    notification.title = title;
    notification.informativeText = body;
    notification.soundName = NSUserNotificationDefaultSoundName;
    [[NSUserNotificationCenter defaultUserNotificationCenter] deliverNotification:notification];
    [NSApp requestUserAttention:NSCriticalRequest];
  });
}

static void LCGSetUnread(int total) {
  dispatch_async(dispatch_get_main_queue(), ^{
    if (lcgStatusItem != nil) {
      lcgStatusItem.button.title = total > 0 ? [NSString stringWithFormat:@" %d", total] : @"";
    }
    if (total > 0) {
      [NSApp requestUserAttention:NSInformationalRequest];
    }
  });
}
*/
import "C"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	webview "github.com/webview/webview_go"

	"hfsgo/internal/appinfo"
	"hfsgo/internal/discovery"
)

type darwinClientSettings struct {
	ServerURL     string `json:"serverUrl,omitempty"`
	AutoStart     bool   `json:"autoStart"`
	Notifications bool   `json:"notifications"`
}

type darwinClientServer struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
}

type darwinClientState struct {
	Version       string               `json:"version"`
	ServerURL     string               `json:"serverUrl"`
	Status        string               `json:"status"`
	Servers       []darwinClientServer `json:"servers"`
	AutoStart     bool                 `json:"autoStart"`
	Notifications bool                 `json:"notifications"`
}

type darwinClientController struct {
	mu         sync.RWMutex
	view       webview.WebView
	logoURL    string
	configPath string
	settings   darwinClientSettings
	servers    []darwinClientServer
	status     string
}

func Run(_, _ []byte) error {
	return errors.New("macOS 版本仅提供 LanChatGo 客户端")
}

func RunClient(logo []byte) error {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("无法确定客户端配置目录：%w", err)
	}
	configDir = filepath.Join(configDir, "LanChatGo")
	if err = os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("无法创建客户端配置目录：%w", err)
	}
	configPath := filepath.Join(configDir, "client.json")
	settings := loadDarwinClientSettings(configPath)
	settings.AutoStart = darwinClientAutoStartEnabled()

	view := webview.New(false)
	if view == nil {
		return errors.New("无法创建 macOS 客户端窗口")
	}
	defer view.Destroy()
	view.SetTitle("LanChatGo 客户端")
	view.SetSize(1180, 800, webview.HintNone)

	controller := &darwinClientController{
		view:       view,
		logoURL:    "data:image/png;base64," + base64.StdEncoding.EncodeToString(logo),
		configPath: configPath,
		settings:   settings,
		status:     "请选择发现的局域网服务，或手动输入服务端地址",
	}
	for name, binding := range map[string]interface{}{
		"clientGetState":      controller.getState,
		"clientConnect":       controller.connect,
		"clientScan":          controller.scan,
		"clientSetOption":     controller.setOption,
		"lanchatNotify":       controller.notify,
		"lanchatUnread":       controller.updateUnread,
		"lanchatOpenSettings": controller.showLauncher,
	} {
		if err = view.Bind(name, binding); err != nil {
			return fmt.Errorf("注册 macOS 客户端操作 %s 失败：%w", name, err)
		}
	}
	view.SetHtml(renderDarwinClientHTML(controller.logoURL))
	if len(logo) > 0 {
		C.LCGConfigureClient(view.Window(), unsafe.Pointer(&logo[0]), C.int(len(logo)))
	} else {
		C.LCGConfigureClient(view.Window(), nil, 0)
	}
	view.Run()
	return nil
}

func loadDarwinClientSettings(path string) darwinClientSettings {
	settings := darwinClientSettings{Notifications: true}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	settings.ServerURL, _ = normalizeDarwinClientURL(settings.ServerURL)
	return settings
}

func (c *darwinClientController) saveSettings() error {
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.configPath, data, 0600)
}

func (c *darwinClientController) getState() darwinClientState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return darwinClientState{
		Version:       appinfo.Version,
		ServerURL:     c.settings.ServerURL,
		Status:        c.status,
		Servers:       append([]darwinClientServer(nil), c.servers...),
		AutoStart:     c.settings.AutoStart,
		Notifications: c.settings.Notifications,
	}
}

func (c *darwinClientController) connect(raw string) error {
	address, err := normalizeDarwinClientURL(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.settings.ServerURL = address
	c.status = "正在连接 " + address
	c.mu.Unlock()
	if err = c.saveSettings(); err != nil {
		return fmt.Errorf("保存服务地址失败：%w", err)
	}
	c.view.Dispatch(func() { c.view.Navigate(address) })
	return nil
}

func normalizeDarwinClientURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("请输入服务端地址")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("服务端地址格式不正确")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("服务端地址只支持 HTTP 或 HTTPS")
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "/", "", "", ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (c *darwinClientController) scan() darwinClientState {
	services, err := discovery.ScanCClass(context.Background(), 1600*time.Millisecond)
	found := make([]darwinClientServer, 0, len(services))
	for _, service := range services {
		if address := service.PreferredURL(); address != "" {
			found = append(found, darwinClientServer{Name: service.Name, Version: service.Version, URL: address})
		}
	}
	c.mu.Lock()
	c.servers = found
	switch {
	case err != nil:
		c.status = "自动发现不可用，可手动输入服务端地址"
	case len(found) == 0:
		c.status = "未发现局域网服务，可手动输入地址或重新扫描"
	default:
		c.status = fmt.Sprintf("已发现 %d 个 LanChatGo 服务", len(found))
	}
	c.mu.Unlock()
	return c.getState()
}

func (c *darwinClientController) setOption(name string, enabled bool) (darwinClientState, error) {
	switch name {
	case "notifications":
		c.mu.Lock()
		c.settings.Notifications = enabled
		c.mu.Unlock()
	case "autoStart":
		if err := setDarwinClientAutoStart(enabled); err != nil {
			return c.getState(), err
		}
		c.mu.Lock()
		c.settings.AutoStart = enabled
		c.mu.Unlock()
	default:
		return c.getState(), errors.New("未知的客户端设置")
	}
	if err := c.saveSettings(); err != nil {
		return c.getState(), err
	}
	return c.getState(), nil
}

func (c *darwinClientController) notify(title, body, _ string) error {
	c.mu.RLock()
	enabled := c.settings.Notifications
	c.mu.RUnlock()
	if !enabled {
		return nil
	}
	titleCString, bodyCString := C.CString(title), C.CString(body)
	defer C.free(unsafe.Pointer(titleCString))
	defer C.free(unsafe.Pointer(bodyCString))
	C.LCGNotify(titleCString, bodyCString)
	return nil
}

func (c *darwinClientController) updateUnread(total int, _ string) {
	C.LCGSetUnread(C.int(total))
}

func (c *darwinClientController) showLauncher() error {
	htmlText := renderDarwinClientHTML(c.logoURL)
	c.view.Dispatch(func() { c.view.SetHtml(htmlText) })
	return nil
}

func darwinClientLaunchAgentPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.aceggbond.LanChatGo.plist")
}

func darwinClientAutoStartEnabled() bool {
	path := darwinClientLaunchAgentPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func setDarwinClientAutoStart(enabled bool) error {
	path := darwinClientLaunchAgentPath()
	if path == "" {
		return errors.New("无法确定 macOS LaunchAgents 目录")
	}
	if !enabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.aceggbond.LanChatGo</string>
<key>ProgramArguments</key><array><string>` + html.EscapeString(executable) + `</string><string>--client</string><string>--autostart</string></array>
<key>RunAtLoad</key><true/>
</dict></plist>`
	return os.WriteFile(path, []byte(plist), 0600)
}

func renderDarwinClientHTML(logoURL string) string {
	page := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>LanChatGo 客户端</title><style>
:root{font-family:-apple-system,BlinkMacSystemFont,"PingFang SC",sans-serif;color:#1d2737;background:#f3f5f8}*{box-sizing:border-box}body{margin:0}.top{height:70px;display:flex;align-items:center;padding:0 28px;border-bottom:1px solid #dfe4ec;background:#fff}.logo{width:44px;height:44px;border-radius:11px}.brand{margin-left:12px;font-size:20px;font-weight:800}.version{margin-left:8px;padding:2px 7px;border-radius:99px;background:#edf3ff;color:#2f6fed;font-size:10px}.shell{width:min(940px,calc(100% - 32px));margin:24px auto;display:grid;grid-template-columns:minmax(0,1fr) 300px;gap:16px}.card{overflow:hidden;border:1px solid #dfe4ec;border-radius:14px;background:#fff}.head{padding:19px;border-bottom:1px solid #e7ebf1}.title{font-size:18px;font-weight:800}.note{margin-top:6px;color:#758196;font-size:11px}.body{padding:17px}.status{padding:11px 13px;border-radius:9px;background:#edf3ff;color:#3869b4;font-size:11px}.servers{display:grid;gap:8px;margin-top:12px}.server{width:100%;display:grid;grid-template-columns:42px minmax(0,1fr) auto;align-items:center;gap:10px;padding:10px;border:1px solid #dfe5ee;border-radius:10px;background:#fff;text-align:left}.server:hover{border-color:#8db5f5}.icon{width:42px;height:42px;display:grid;place-items:center;border-radius:9px;background:#2f6fed;color:#fff;font-weight:800}.name{font-size:13px;font-weight:750}.url{margin-top:4px;color:#758196;font-size:10px}.tag{color:#16835d;font-size:10px}.empty{padding:30px;color:#8994a5;text-align:center;font-size:11px}.manual{display:flex;gap:8px;margin-top:13px}.input{min-width:0;flex:1;height:40px;padding:0 11px;border:1px solid #d8e0ea;border-radius:9px;outline:0}button{height:40px;padding:0 13px;border:1px solid #d8e0ea;border-radius:9px;background:#fff;color:#34435a;cursor:pointer}.primary{border-color:#2f6fed;background:#2f6fed;color:#fff;font-weight:700}.side-title{padding:17px;border-bottom:1px solid #e7ebf1;font-size:14px;font-weight:800}.option{display:flex;align-items:center;gap:12px;padding:15px 17px;border-bottom:1px solid #edf0f4}.copy{min-width:0;flex:1}.option-name{font-size:12px;font-weight:700}.option-note{margin-top:4px;color:#7a8698;font-size:9px;line-height:1.5}.switch{width:18px;height:18px;accent-color:#2f6fed}@media(max-width:720px){.shell{grid-template-columns:1fr}}</style></head><body>
<header class="top"><img class="logo" src="{{LOGO}}"><span class="brand">LanChatGo 客户端</span><span class="version">v{{VERSION}}</span></header>
<main class="shell"><section class="card"><div class="head"><div class="title">连接服务端</div><div class="note">启动时仅进行一次低频局域网广播发现，不逐个扫描 IP 或端口。</div></div><div class="body"><div id="status" class="status">正在准备…</div><div id="servers" class="servers"></div><form id="manual" class="manual"><input id="address" class="input" placeholder="例如 https://192.168.1.10"><button class="primary">连接</button><button id="scan" type="button">重新扫描</button></form></div></section>
<aside class="card"><div class="side-title">客户端设置</div><label class="option"><span class="copy"><span class="option-name">开机自动启动</span><span class="option-note">登录 macOS 后自动启动客户端</span></span><input id="autoStart" class="switch" type="checkbox"></label><label class="option"><span class="copy"><span class="option-name">新消息通知</span><span class="option-note">使用 macOS 通知与 Dock 提醒</span></span><input id="notifications" class="switch" type="checkbox"></label></aside></main>
<script>(function(){'use strict';var state;function $(id){return document.getElementById(id)}function esc(v){return String(v||'').replace(/[&<>"']/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]})}function render(){if(!state)return;$('status').textContent=state.status||'等待操作';if(document.activeElement!==$('address'))$('address').value=state.serverUrl||'';$('autoStart').checked=!!state.autoStart;$('notifications').checked=!!state.notifications;var rows=(state.servers||[]).map(function(s){return'<button class="server" type="button" data-url="'+esc(s.url)+'"><span class="icon">LC</span><span><span class="name">'+esc(s.name||'LanChatGo')+' · v'+esc(s.version||'?')+'</span><span class="url">'+esc(s.url)+'</span></span><span class="tag">连接</span></button>'}).join('');$('servers').innerHTML=rows||'<div class="empty">暂未发现服务，可手动输入地址。</div>';$('servers').querySelectorAll('.server').forEach(function(b){b.onclick=function(){window.clientConnect(b.dataset.url).catch(alert)}})}async function refresh(){state=await window.clientGetState();render()}$('manual').onsubmit=function(e){e.preventDefault();window.clientConnect($('address').value).catch(function(e){alert(e.message||e)})};$('scan').onclick=async function(){state=await window.clientScan();render()};['autoStart','notifications'].forEach(function(k){$(k).onchange=async function(){try{state=await window.clientSetOption(k,$(k).checked);render()}catch(e){alert(e.message||e);refresh()}}});(async function(){await refresh();if(state.serverUrl){window.clientConnect(state.serverUrl);return}state=await window.clientScan();render();if(state.servers&&state.servers.length===1)window.clientConnect(state.servers[0].url)})()})();</script></body></html>`
	page = strings.ReplaceAll(page, "{{LOGO}}", logoURL)
	return strings.ReplaceAll(page, "{{VERSION}}", appinfo.Version)
}
