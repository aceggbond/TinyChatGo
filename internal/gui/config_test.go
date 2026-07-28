package gui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistedSettingsRoundTrip(t *testing.T) {
	filename := filepath.Join(t.TempDir(), settingsFileName)
	want := persistedSettings{
		Password:            "测试密码",
		AllowUpload:         true,
		AllowDownload:       false,
		RedirectToHTTPS:     true,
		AllowChat:           true,
		GroupChat:           true,
		AllowClientDownload: true,
		NotifyNewVisitor:    false,
		NotifyNewMessage:    true,
		Port:                "7788",
		HTTPSPort:           "7789",
		AccessHost:          "192.168.1.8",
	}
	if err := savePersistedSettings(filename, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadPersistedSettings(filename)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestPersistedSettingsDefaults(t *testing.T) {
	got, err := loadPersistedSettings(filepath.Join(t.TempDir(), settingsFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !got.AllowDownload || !got.NotifyNewVisitor || !got.NotifyNewMessage || got.Port != "1122" || got.HTTPSPort != "" {
		t.Fatalf("unexpected defaults: %#v", got)
	}
}

func TestPersistedSettingsRejectsInvalidJSON(t *testing.T) {
	filename := filepath.Join(t.TempDir(), settingsFileName)
	if err := os.WriteFile(filename, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadPersistedSettings(filename)
	if err == nil {
		t.Fatal("expected invalid JSON error")
	}
	if got != defaultPersistedSettings() {
		t.Fatalf("invalid config should return safe defaults: %#v", got)
	}
}
