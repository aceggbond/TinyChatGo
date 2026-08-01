//go:build windows && !client

package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"tinychatgo/internal/appinfo"
)

const (
	projectURL          = "https://github.com/aceggbond/TinyChatGo"
	latestReleaseAPIURL = "https://api.github.com/repos/aceggbond/TinyChatGo/releases/latest"
	maxReleaseResponse  = 1 << 20
)

type modernUpdateStatus struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	UpdateAvailable bool   `json:"updateAvailable"`
	URL             string `json:"url"`
	Message         string `json:"message"`
}

func (m *modernController) checkUpdate() (modernUpdateStatus, error) {
	status := modernUpdateStatus{CurrentVersion: appinfo.Version, URL: projectURL}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseAPIURL, nil)
	if err != nil {
		return status, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "TinyChatGo/"+appinfo.Version)
	response, err := (&http.Client{Timeout: 12 * time.Second}).Do(request)
	if err != nil {
		return status, fmt.Errorf("连接 GitHub 失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return status, fmt.Errorf("GitHub 返回状态 %d", response.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxReleaseResponse))
	if err = decoder.Decode(&release); err != nil {
		return status, fmt.Errorf("解析更新信息失败：%w", err)
	}
	status.LatestVersion = strings.TrimSpace(release.TagName)
	if status.LatestVersion == "" {
		return status, errors.New("GitHub 最新发布没有版本号")
	}
	if validProjectURL(release.HTMLURL) {
		status.URL = release.HTMLURL
	}
	status.UpdateAvailable = compareVersionNumbers(status.LatestVersion, appinfo.Version) > 0
	if status.UpdateAvailable {
		status.Message = "发现新版本 " + status.LatestVersion
	} else {
		status.Message = "当前已是最新版本（GitHub 最新发布 " + status.LatestVersion + "）"
	}
	return status, nil
}

func validProjectURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return false
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return false
	}
	path := strings.ToLower(strings.TrimSuffix(parsed.EscapedPath(), "/"))
	return path == "/aceggbond/tinychatgo" || strings.HasPrefix(path, "/aceggbond/tinychatgo/")
}

func (m *modernController) openProjectURL(rawURL string) error {
	if !validProjectURL(rawURL) {
		return errors.New("只能打开 TinyChatGo 的 GitHub 项目地址")
	}
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", rawURL).Start()
}
