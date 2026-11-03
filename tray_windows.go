package main

import (
	"errors"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var shell32 = syscall.NewLazyDLL("shell32.dll")
var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var pRegisterClass = user32.NewProc("RegisterClassExW")
var pCreateWindow = user32.NewProc("CreateWindowExW")
var pDefWindowProc = user32.NewProc("DefWindowProcW")
var pGetMessage = user32.NewProc("GetMessageW")
var pDispatchMessage = user32.NewProc("DispatchMessageW")
var pTranslateMessage = user32.NewProc("TranslateMessage")
var pDestroyWindow = user32.NewProc("DestroyWindow")
var pPostMessage = user32.NewProc("PostMessageW")
var pPostQuit = user32.NewProc("PostQuitMessage")
var pNotify = shell32.NewProc("Shell_NotifyIconW")
var pCreateIcon = user32.NewProc("CreateIcon")
var pDestroyIcon = user32.NewProc("DestroyIcon")
var pCreateMenu = user32.NewProc("CreatePopupMenu")
var pAppendMenu = user32.NewProc("AppendMenuW")
var pTrackMenu = user32.NewProc("TrackPopupMenu")
var pDestroyMenu = user32.NewProc("DestroyMenu")
var pGetCursor = user32.NewProc("GetCursorPos")
var pForeground = user32.NewProc("SetForegroundWindow")
var pSetTimer = user32.NewProc("SetTimer")
var pKillTimer = user32.NewProc("KillTimer")
var pTaskbarMessage = user32.NewProc("RegisterWindowMessageW")

type windowClass struct {
	Size        uint32
	Style       uint32
	Proc        uintptr
	ClassExtra  int32
	WindowExtra int32
	Instance    uintptr
	Icon        uintptr
	Cursor      uintptr
	Background  uintptr
	MenuName    *uint16
	ClassName   *uint16
	SmallIcon   uintptr
}
type point struct {
	X int32
	Y int32
}
type winMessage struct {
	Window  uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   point
	Private uint32
}
type notifyIcon struct {
	Size        uint32
	Window      uintptr
	ID          uint32
	Flags       uint32
	Callback    uint32
	Icon        uintptr
	Tip         [128]uint16
	State       uint32
	StateMask   uint32
	Info        [256]uint16
	Version     uint32
	InfoTitle   [64]uint16
	InfoFlags   uint32
	GUID        [16]byte
	BalloonIcon uintptr
}

type Tray struct {
	window         atomic.Uintptr
	registered     atomic.Bool
	done           chan struct{}
	workspace      *Workspace
	shutdown       chan struct{}
	icon           uintptr
	lastOpen       time.Time
	taskbarMessage uint32
}

func startTray(w *Workspace, shutdown chan struct{}) (*Tray, error) {
	t := &Tray{workspace: w, shutdown: shutdown, done: make(chan struct{})}
	ready := make(chan error, 1)
	go t.run(ready)
	select {
	case err := <-ready:
		return t, err
	case <-time.After(4 * time.Second):
		return t, errors.New("托盘初始化超时")
	}
}
func (t *Tray) data() notifyIcon {
	n := notifyIcon{Window: t.window.Load(), ID: 1, Flags: 1 | 2 | 4 | 0x80, Callback: 0x8001, Icon: t.icon}
	n.Size = uint32(unsafe.Sizeof(n))
	tip, _ := syscall.UTF16FromString(t.workspace.traySummary())
	copy(n.Tip[:], tip)
	return n
}
func (t *Tray) add() {
	n := t.data()
	r, _, _ := pNotify.Call(0, uintptr(unsafe.Pointer(&n)))
	t.registered.Store(r != 0)
	if r != 0 {
		n.Version = 4
		pNotify.Call(4, uintptr(unsafe.Pointer(&n)))
	}
}
func (t *Tray) modify() {
	if !t.registered.Load() {
		t.add()
		return
	}
	n := t.data()
	r, _, _ := pNotify.Call(1, uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		t.registered.Store(false)
	}
}
func (t *Tray) open() {
	if time.Since(t.lastOpen) < 700*time.Millisecond {
		return
	}
	t.lastOpen = time.Now()
	c := t.workspace.snapshot()
	go openBrowser(c.UIPort)
}
func (t *Tray) menu(hwnd uintptr) {
	m, _, _ := pCreateMenu.Call()
	if m == 0 {
		return
	}
	defer pDestroyMenu.Call(m)
	add := func(flags, id uintptr, label string) {
		s, _ := syscall.UTF16PtrFromString(label)
		pAppendMenu.Call(m, flags, id, uintptr(unsafe.Pointer(s)))
	}
	add(0, 1, "打开网页管理端")
	add(0x800, 0, "")
	add(2, 0, t.workspace.traySummary())
	add(0x800, 0, "")
	add(0, 2, "启动全部出口")
	add(0, 3, "停止全部出口")
	add(0x800, 0, "")
	add(0, 4, "退出链跃")
	var p point
	pGetCursor.Call(uintptr(unsafe.Pointer(&p)))
	pForeground.Call(hwnd)
	id, _, _ := pTrackMenu.Call(m, 0x100|0x2, uintptr(p.X), uintptr(p.Y), 0, hwnd, 0)
	pPostMessage.Call(hwnd, 0, 0, 0)
	switch id {
	case 1:
		t.open()
	case 2:
		go t.workspace.startAll()
	case 3:
		go t.workspace.stopAll(true)
	case 4:
		select {
		case t.shutdown <- struct{}{}:
		default:
		}
	}
}
func (t *Tray) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(t.done)
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	t.icon = createTrayIcon(instance)
	name, _ := syscall.UTF16PtrFromString("LinkWeaver.Tray." + randomToken()[:12])
	title, _ := syscall.UTF16PtrFromString("链跃 LinkWeaver")
	taskbarName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	taskbar, _, _ := pTaskbarMessage.Call(uintptr(unsafe.Pointer(taskbarName)))
	t.taskbarMessage = uint32(taskbar)
	callback := syscall.NewCallback(func(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
		if msg == t.taskbarMessage && t.taskbarMessage != 0 {
			t.add()
			return 0
		}
		switch msg {
		case 0x8001:
			event := uint32(lparam & 0xffff)
			if event == 0x202 || event == 0x203 || event == 0x400 || event == 0x401 {
				t.open()
			} else if event == 0x205 || event == 0x7b {
				t.menu(hwnd)
			}
			return 0
		case 0x113:
			t.modify()
			return 0
		case 0x10:
			pDestroyWindow.Call(hwnd)
			return 0
		case 0x2:
			n := t.data()
			pNotify.Call(2, uintptr(unsafe.Pointer(&n)))
			t.registered.Store(false)
			pKillTimer.Call(hwnd, 1)
			if t.icon != 0 {
				pDestroyIcon.Call(t.icon)
			}
			pPostQuit.Call(0)
			return 0
		}
		r, _, _ := pDefWindowProc.Call(hwnd, uintptr(msg), wparam, lparam)
		return r
	})
	wc := windowClass{Proc: callback, Instance: instance, Icon: t.icon, SmallIcon: t.icon, ClassName: name}
	wc.Size = uint32(unsafe.Sizeof(wc))
	registered, _, _ := pRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))
	if registered == 0 {
		ready <- errors.New("无法注册托盘窗口")
		return
	}
	hwnd, _, _ := pCreateWindow.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		ready <- errors.New("无法创建托盘窗口")
		return
	}
	t.window.Store(hwnd)
	t.add()
	pSetTimer.Call(hwnd, 1, 3000, 0)
	if !t.registered.Load() {
		ready <- errors.New("Windows 未接受托盘图标注册")
	} else {
		ready <- nil
	}
	var msg winMessage
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
}
func (t *Tray) close() {
	if t == nil {
		return
	}
	if hwnd := t.window.Load(); hwnd != 0 {
		pPostMessage.Call(hwnd, 0x10, 0, 0)
	}
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
}

func createTrayIcon(instance uintptr) uintptr {
	const size = 32
	andMask := make([]byte, size*4)
	pixels := make([]byte, size*size*4)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := (y*size + x) * 4
			outside := (x < 3 || x > 28) && (y < 3 || y > 28)
			if outside {
				andMask[y*4+x/8] |= 1 << uint(7-x%8)
				continue
			}
			pixels[i] = 48
			pixels[i+1] = 99
			pixels[i+2] = 55
			pixels[i+3] = 255
			arrow := (x >= 8 && x <= 21 && y >= 10 && y <= 23 && absInt((x+y)-31) <= 2) || (y >= 8 && y <= 11 && x >= 15 && x <= 24) || (x >= 21 && x <= 24 && y >= 8 && y <= 17)
			if arrow {
				pixels[i] = 164
				pixels[i+1] = 232
				pixels[i+2] = 215
				pixels[i+3] = 255
			}
		}
	}
	icon, _, _ := pCreateIcon.Call(instance, size, size, 1, 32, uintptr(unsafe.Pointer(&andMask[0])), uintptr(unsafe.Pointer(&pixels[0])))
	return icon
}
func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
