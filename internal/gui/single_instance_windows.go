//go:build windows

package gui

import (
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	singleInstanceMutexName      = `Local\HFS-Go.SingleInstance.4A7D3219`
	singleInstanceReadyEventName = `Local\HFS-Go.SingleInstanceReady.4A7D3219`
	singleInstanceMessageName    = `HFS-Go.ActivateExisting.4A7D3219`

	errorAlreadyExists = syscall.Errno(183)
	hwndBroadcast      = 0xffff
)

var (
	createMutex     = kernel32.NewProc("CreateMutexW")
	createEvent     = kernel32.NewProc("CreateEventW")
	setEvent        = kernel32.NewProc("SetEvent")
	waitForSingle   = kernel32.NewProc("WaitForSingleObject")
	closeHandle     = kernel32.NewProc("CloseHandle")
	instanceMessage uint32
)

type singleInstanceGuard struct {
	mutex     uintptr
	ready     uintptr
	message   uint32
	closeOnce sync.Once
}

func acquireSingleInstance() (*singleInstanceGuard, bool, error) {
	return acquireNamedSingleInstance(
		singleInstanceMutexName,
		singleInstanceReadyEventName,
		singleInstanceMessageName,
		5*time.Second,
	)
}

func acquireNamedSingleInstance(mutexName, readyEventName, messageName string, readyTimeout time.Duration) (*singleInstanceGuard, bool, error) {
	messageText, err := syscall.UTF16FromString(messageName)
	if err != nil {
		return nil, false, fmt.Errorf("无效的单实例消息名称: %w", err)
	}
	registered, _, registerErr := registerMessage.Call(uintptr(unsafe.Pointer(&messageText[0])))
	if registered == 0 {
		return nil, false, fmt.Errorf("无法注册单实例窗口消息: %v", registerErr)
	}

	mutexText, err := syscall.UTF16FromString(mutexName)
	if err != nil {
		return nil, false, fmt.Errorf("无效的单实例互斥锁名称: %w", err)
	}
	mutex, _, mutexErr := createMutex.Call(0, 0, uintptr(unsafe.Pointer(&mutexText[0])))
	if mutex == 0 {
		return nil, false, fmt.Errorf("无法创建单实例互斥锁: %v", mutexErr)
	}
	alreadyRunning := mutexErr == errorAlreadyExists

	eventText, err := syscall.UTF16FromString(readyEventName)
	if err != nil {
		closeHandle.Call(mutex)
		return nil, false, fmt.Errorf("无效的单实例就绪事件名称: %w", err)
	}
	ready, _, eventErr := createEvent.Call(0, 1, 0, uintptr(unsafe.Pointer(&eventText[0])))
	if ready == 0 {
		closeHandle.Call(mutex)
		return nil, false, fmt.Errorf("无法创建单实例就绪事件: %v", eventErr)
	}

	guard := &singleInstanceGuard{mutex: mutex, ready: ready, message: uint32(registered)}
	if !alreadyRunning {
		return guard, true, nil
	}

	// The primary process can own the mutex before WebView has finished
	// creating and subclassing its window. Wait for that exact point so the
	// activation message is not lost during startup.
	timeoutMilliseconds := uint64(readyTimeout / time.Millisecond)
	if timeoutMilliseconds > uint64(^uint32(0)-1) {
		timeoutMilliseconds = uint64(^uint32(0) - 1)
	}
	waitForSingle.Call(ready, uintptr(timeoutMilliseconds))
	postMessage.Call(hwndBroadcast, registered, 0, 0)
	guard.close()
	return nil, false, nil
}

func (g *singleInstanceGuard) signalReady() error {
	if g == nil || g.ready == 0 {
		return nil
	}
	result, _, err := setEvent.Call(g.ready)
	if result == 0 {
		return fmt.Errorf("无法标记主窗口就绪: %v", err)
	}
	return nil
}

func (g *singleInstanceGuard) close() {
	if g == nil {
		return
	}
	g.closeOnce.Do(func() {
		if g.ready != 0 {
			closeHandle.Call(g.ready)
			g.ready = 0
		}
		if g.mutex != 0 {
			closeHandle.Call(g.mutex)
			g.mutex = 0
		}
	})
}
