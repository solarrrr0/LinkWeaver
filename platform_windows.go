package main

import (
	"crypto/tls"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func hideWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }
func tlsDone(t *Timing, lock *sync.Mutex, start *time.Time) func(tls.ConnectionState, error) {
	return func(_ tls.ConnectionState, _ error) {
		lock.Lock()
		if !start.IsZero() {
			t.TLSMS = float64(time.Since(*start).Microseconds()) / 1000
		}
		lock.Unlock()
	}
}
