package gui

import "testing"

func TestSafeClipboardFileName(t *testing.T) {
	tests := map[string]string{
		`C:\folder\report?.docx`: "report_.docx",
		"../demo.txt":            "demo.txt",
		`bad<>:"|?*.txt`:         "bad_______.txt",
		"":                       "LanChatGo-附件",
	}
	for input, want := range tests {
		if got := safeClipboardFileName(input); got != want {
			t.Errorf("safeClipboardFileName(%q) = %q, want %q", input, got, want)
		}
	}
}
