//go:build !client

package gui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const settingsFileName = "lanchatgo.config"

type persistedSettings struct {
	Password            string `json:"password"`
	AllowUpload         bool   `json:"allowUpload"`
	AllowDownload       bool   `json:"allowDownload"`
	RedirectToHTTPS     bool   `json:"redirectToHTTPS"`
	AllowChat           bool   `json:"allowChat"`
	GroupChat           bool   `json:"groupChat"`
	AllowGroupChat      bool   `json:"allowGroupChat"`
	ShowUserList        bool   `json:"showUserList"`
	AllowPrivateChat    bool   `json:"allowPrivateChat"`
	AllowClientDownload bool   `json:"allowClientDownload"`
	NotifyNewVisitor    bool   `json:"notifyNewVisitor"`
	NotifyNewMessage    bool   `json:"notifyNewMessage"`
	Port                string `json:"port,omitempty"`
	HTTPSPort           string `json:"httpsPort,omitempty"`
	AccessHost          string `json:"accessHost,omitempty"`
}

func defaultPersistedSettings() persistedSettings {
	return persistedSettings{
		AllowDownload:    true,
		NotifyNewVisitor: true,
		NotifyNewMessage: true,
		Port:             "80",
		HTTPSPort:        "443",
		AllowChat:        true,
		GroupChat:        false,
		AllowGroupChat:   true,
	}
}

func loadPersistedSettings(filename string) (persistedSettings, error) {
	settings := defaultPersistedSettings()
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if err = json.Unmarshal(data, &settings); err != nil {
		return defaultPersistedSettings(), err
	}
	if settings.Port == "" {
		settings.Port = "80"
	}
	if settings.HTTPSPort == "" {
		settings.HTTPSPort = "443"
	}
	return settings, nil
}

func savePersistedSettings(filename string, settings persistedSettings) error {
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(filename)
	tmp, err := os.CreateTemp(dir, ".lanchatgo.config-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	_ = os.Remove(filename)
	return os.Rename(tmpName, filename)
}
