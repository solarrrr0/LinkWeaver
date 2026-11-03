package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"
)

func main() {
	exe, _ := os.Executable()
	rootFlag := flag.String("root", filepath.Dir(exe), "project data directory")
	open := flag.Bool("open", false, "open local dashboard")
	autostart := flag.Bool("start", false, "start core and optimize")
	background := flag.Bool("background", false, "start in system tray without opening a browser")
	login := flag.Bool("login", false, "start after Windows login")
	setStartup := flag.String("set-startup", "", "configure login startup: on or off")
	shutdown := flag.Bool("shutdown", false, "gracefully close this installation")
	removeStartup := flag.Bool("remove-startup", false, "remove this user's login startup entry")
	applyUpdate := flag.String("apply-update", "", "internal update helper")
	flag.Parse()
	if *applyUpdate != "" {
		updateWorker(*applyUpdate)
		return
	}
	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		return
	}
	if *shutdown || *removeStartup {
		if *shutdown {
			err = shutdownInstalled(root)
		} else {
			err = setLoginStartup(root, false)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err = os.MkdirAll(filepath.Join(root, "data"), 0700); err != nil {
		return
	}
	c, err := loadConfig(filepath.Join(root, "data", "config.json"))
	if err != nil {
		_ = atomicWrite(filepath.Join(root, "data", "startup-error.txt"), []byte(err.Error()))
		if *setStartup != "" {
			os.Exit(1)
		}
		return
	}
	if *setStartup != "" {
		if *setStartup != "on" && *setStartup != "off" {
			os.Exit(2)
		}
		old := c.RunAtLogin
		c.RunAtLogin = *setStartup == "on"
		if err = setLoginStartup(root, c.RunAtLogin); err == nil {
			err = saveConfig(filepath.Join(root, "data", "config.json"), c)
			if err != nil {
				_ = setLoginStartup(root, old)
			}
		}
		if err != nil {
			_ = atomicWrite(filepath.Join(root, "data", "install-error.txt"), []byte(err.Error()))
			os.Exit(1)
		}
		_ = os.Remove(filepath.Join(root, "data", "install-error.txt"))
		return
	}
	mode := "manual"
	if *login {
		mode = "windows-login"
	}
	_ = atomicWrite(filepath.Join(root, "data", "last-launch.txt"), []byte(time.Now().Format(time.RFC3339)+"\n"+mode+"\n"))
	if *login {
		// Let Explorer and the network initialize before restoring proxy cores.
		time.Sleep(10 * time.Second)
	}
	l, err := net.Listen("tcp", listenAddr(c.UIPort))
	if err != nil {
		_ = atomicWrite(filepath.Join(root, "data", "startup-error.txt"), []byte("本地管理端口被占用，程序可能已运行"))
		if *open || !*background {
			openBrowser(c.UIPort)
		}
		return
	}
	workspace, err := newWorkspace(root, c)
	if err != nil {
		l.Close()
		_ = atomicWrite(filepath.Join(root, "data", "startup-error.txt"), []byte(err.Error()))
		return
	}
	s := &Server{workspace: workspace, token: randomToken(), shutdown: make(chan struct{}, 1)}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	go func() { _ = server.Serve(l) }()
	_ = atomicWrite(filepath.Join(root, "data", "instance.json"), []byte(fmt.Sprintf(`{"pid":%d,"port":%d}`, os.Getpid(), c.UIPort)))
	_ = os.Remove(filepath.Join(root, "data", "startup-error.txt"))
	tray, trayErr := startTray(workspace, s.shutdown)
	workspace.mu.Lock()
	workspace.tray = tray
	workspace.mu.Unlock()
	if trayErr != nil {
		_ = atomicWrite(filepath.Join(root, "data", "tray-error.txt"), []byte(trayErr.Error()))
	} else {
		_ = os.Remove(filepath.Join(root, "data", "tray-error.txt"))
	}
	if *open || c.OpenOnLaunch && !*background {
		openBrowser(c.UIPort)
	}
	go workspace.restore(*autostart)
	ctx, cancel := context.WithCancel(context.Background())
	go workspace.backgroundLoop(ctx)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	select {
	case <-signals:
	case <-s.shutdown:
	}
	cancel()
	workspace.cancelBackground()
	workspace.stopAll(false)
	tray.close()
	_ = server.Close()
	_ = os.Remove(filepath.Join(root, "data", "instance.json"))
}
func openBrowser(port int) {
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", fmt.Sprintf("http://127.0.0.1:%d", port))
	hideWindow(cmd)
	_ = cmd.Start()
}
