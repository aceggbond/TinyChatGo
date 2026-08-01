//go:build windows

package gui

import "testing"

func TestCompareVersionNumbers(t *testing.T) {
	for _, test := range []struct {
		left, right string
		want        int
	}{
		{"v1.2.0", "1.1", 1},
		{"1.1.0", "v1.1", 0},
		{"v0.1.0", "1.1", -1},
		{"2.0-beta.1", "1.9.9", 1},
	} {
		got := compareVersionNumbers(test.left, test.right)
		if got != test.want {
			t.Errorf("compareVersionNumbers(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestValidProjectURLIsRestrictedToRepository(t *testing.T) {
	for _, allowed := range []string{
		projectURL,
		projectURL + "/releases/tag/v1.2.0",
	} {
		if !validProjectURL(allowed) {
			t.Errorf("project URL rejected: %s", allowed)
		}
	}
	for _, blocked := range []string{
		"http://github.com/aceggbond/TinyChatGo",
		"https://github.com/other/repo",
		"https://evil.example/aceggbond/TinyChatGo",
		"https://github.com:444/aceggbond/TinyChatGo",
	} {
		if validProjectURL(blocked) {
			t.Errorf("external URL accepted: %s", blocked)
		}
	}
}
