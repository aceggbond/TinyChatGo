package gui

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxClipboardAttachmentBytes = int64(1 << 30)

func downloadClipboardAttachment(rawURL, rawName, serverURL string) (string, error) {
	attachmentURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || attachmentURL.Host == "" {
		return "", errors.New("附件地址无效")
	}
	server, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || server.Host == "" {
		return "", errors.New("客户端尚未连接服务端")
	}
	if !strings.EqualFold(attachmentURL.Host, server.Host) ||
		!strings.HasPrefix(attachmentURL.EscapedPath(), "/__hfs/chat/file/") {
		return "", errors.New("只允许复制当前服务端的聊天附件")
	}
	switch attachmentURL.Scheme {
	case "http", "https":
	default:
		return "", errors.New("附件地址协议无效")
	}

	request, err := http.NewRequest(http.MethodGet, attachmentURL.String(), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "TinyChatGo-Client")
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- LAN servers may use the generated self-signed certificate.
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Minute,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if !strings.EqualFold(next.URL.Host, server.Host) ||
				!strings.HasPrefix(next.URL.EscapedPath(), "/__hfs/chat/file/") {
				return errors.New("附件下载被重定向到其他地址")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("下载附件失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("服务端返回状态 %d", response.StatusCode)
	}
	if response.ContentLength > maxClipboardAttachmentBytes {
		return "", errors.New("文件超过 1 GiB，无法复制")
	}

	directory, err := os.MkdirTemp("", "TinyChatGo-Clipboard-*")
	if err != nil {
		return "", fmt.Errorf("创建剪贴板临时目录失败：%w", err)
	}
	name := safeClipboardFileName(rawName)
	target := filepath.Join(directory, name)
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = os.Remove(directory)
		return "", fmt.Errorf("创建剪贴板临时文件失败：%w", err)
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, maxClipboardAttachmentBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written <= 0 || written > maxClipboardAttachmentBytes {
		_ = os.Remove(target)
		_ = os.Remove(directory)
		switch {
		case copyErr != nil:
			return "", fmt.Errorf("保存附件失败：%w", copyErr)
		case closeErr != nil:
			return "", fmt.Errorf("保存附件失败：%w", closeErr)
		default:
			return "", errors.New("附件大小无效")
		}
	}
	return target, nil
}

func safeClipboardFileName(raw string) string {
	name := strings.TrimSpace(filepath.Base(strings.ReplaceAll(raw, `\`, `/`)))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "TinyChatGo-附件"
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*', 0:
			return '_'
		default:
			if r < 32 {
				return '_'
			}
			return r
		}
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return "TinyChatGo-附件"
	}
	return name
}
