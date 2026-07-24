//go:build windows

package gui

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestEncodeClipboardText(t *testing.T) {
	const value = "http://10.0.0.8:1122/路径😀"
	encoded, err := encodeClipboardText(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || encoded[len(encoded)-1] != 0 {
		t.Fatal("clipboard text is not NUL terminated")
	}
	if got := syscall.UTF16ToString(encoded); got != value {
		t.Fatalf("clipboard round trip = %q, want %q", got, value)
	}
	if _, err = encodeClipboardText("bad\x00value"); err == nil {
		t.Fatal("embedded NUL was accepted")
	}
}

func TestParseOpenFilePaths(t *testing.T) {
	encode := func(parts ...string) []uint16 {
		var value []uint16
		for _, part := range parts {
			encoded, err := syscall.UTF16FromString(part)
			if err != nil {
				t.Fatal(err)
			}
			value = append(value, encoded...)
		}
		return append(value, 0)
	}

	single := parseOpenFilePaths(encode(`C:\data\one.txt`))
	if len(single) != 1 || single[0] != `C:\data\one.txt` {
		t.Fatalf("single selection = %#v", single)
	}
	multiple := parseOpenFilePaths(encode(`C:\data`, "one.txt", "two.bin"))
	if len(multiple) != 2 || multiple[0] != `C:\data\one.txt` || multiple[1] != `C:\data\two.bin` {
		t.Fatalf("multiple selection = %#v", multiple)
	}
}

func TestOpenFileNameWindowsLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(openFileName{}) != 152 {
		t.Fatalf("OPENFILENAMEW x64 size = %d, want 152", unsafe.Sizeof(openFileName{}))
	}
	if unsafe.Offsetof(openFileName{}.FileOffset) != unsafe.Offsetof(openFileName{}.Flags)+4 {
		t.Fatal("OPENFILENAMEW WORD offsets do not follow Flags")
	}
}

func TestSingleInstanceWaitsUntilPrimaryWindowIsReady(t *testing.T) {
	suffix := fmt.Sprintf("%d.%d", os.Getpid(), time.Now().UnixNano())
	mutexName := `Local\HFS-Go.TestMutex.` + suffix
	eventName := `Local\HFS-Go.TestReady.` + suffix
	messageName := `HFS-Go.TestActivate.` + suffix

	primary, isPrimary, err := acquireNamedSingleInstance(mutexName, eventName, messageName, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !isPrimary || primary == nil {
		t.Fatal("first acquisition was not primary")
	}
	defer primary.close()

	type result struct {
		guard   *singleInstanceGuard
		primary bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		guard, secondPrimary, acquireErr := acquireNamedSingleInstance(mutexName, eventName, messageName, time.Second)
		done <- result{guard: guard, primary: secondPrimary, err: acquireErr}
	}()

	select {
	case second := <-done:
		t.Fatalf("second acquisition returned before primary was ready: %#v", second)
	case <-time.After(50 * time.Millisecond):
	}
	if err = primary.signalReady(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-done:
		if second.err != nil {
			t.Fatal(second.err)
		}
		if second.primary || second.guard != nil {
			t.Fatalf("second acquisition result = %#v, want activated secondary", second)
		}
	case <-time.After(time.Second):
		t.Fatal("second acquisition did not resume after primary became ready")
	}

	primary.close()
	restarted, restartedPrimary, err := acquireNamedSingleInstance(mutexName, eventName, messageName, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !restartedPrimary || restarted == nil {
		t.Fatal("mutex remained held after the primary closed")
	}
	restarted.close()
}
