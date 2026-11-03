package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func deleteStartupValue(path, name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	err = k.DeleteValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

func psLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func setLoginStartup(root string, enabled bool) error {
	folder, err := windows.KnownFolderPath(windows.FOLDERID_Startup, 0)
	if err != nil {
		return errors.New("无法找到当前用户的 Windows 启动文件夹")
	}
	link := filepath.Join(folder, "LinkWeaver.lnk")
	if enabled {
		exe := filepath.Join(root, "LinkWeaver.exe")
		if _, err := os.Stat(exe); err != nil {
			return errors.New("找不到主程序，无法设置开机启动")
		}
		if err := os.MkdirAll(folder, 0700); err != nil {
			return errors.New("无法创建 Windows 启动文件夹")
		}
		script := "$ErrorActionPreference='Stop';$s=(New-Object -ComObject WScript.Shell).CreateShortcut(" + psLiteral(link) + ");" +
			"$s.TargetPath=" + psLiteral(exe) + ";$s.Arguments=" + psLiteral(`--background --login --root "`+root+`"`) + ";" +
			"$s.WorkingDirectory=" + psLiteral(root) + ";$s.Description='LinkWeaver';$s.IconLocation=" + psLiteral(exe+",0") + ";$s.Save()"
		var encoded bytes.Buffer
		_ = binary.Write(&encoded, binary.LittleEndian, utf16.Encode([]rune(script)))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded.Bytes()))
		hideWindow(cmd)
		if err := cmd.Run(); err != nil {
			return errors.New("无法创建开机启动快捷方式，请检查启动文件夹的写入权限")
		}
		if _, err := os.Stat(link); err != nil {
			return errors.New("开机启动快捷方式未能保存")
		}
	} else if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("无法移除开机启动快捷方式")
	}
	// Migrate the old Run entry so Windows starts only one instance.
	if err := deleteStartupValue(`Software\Microsoft\Windows\CurrentVersion\Run`, "LinkWeaver"); err != nil {
		return errors.New("无法移除旧的开机启动项")
	}
	if enabled {
		// An explicit enable/save also repairs a Task Manager-disabled entry.
		if err := deleteStartupValue(`Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\StartupFolder`, "LinkWeaver.lnk"); err != nil {
			return errors.New("Windows 禁用了此启动项，无法重新启用")
		}
	}
	return nil
}

// Used by Setup before replacing files and by Uninstall before removing them.
// Verify the owning executable before using the local authenticated API.
func shutdownInstalled(root string) error {
	b, err := os.ReadFile(filepath.Join(root, "data", "instance.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var instance struct {
		PID  uint32 `json:"pid"`
		Port int    `json:"port"`
	}
	if json.Unmarshal(b, &instance) != nil || instance.PID == 0 || instance.Port < 1 || instance.Port > 65535 {
		return errors.New("程序实例记录无效，请先从托盘退出程序")
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, instance.PID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return errors.New("无法检查运行中的程序，请先从托盘退出")
	}
	defer windows.CloseHandle(h)
	if state, _ := windows.WaitForSingleObject(h, 0); state == windows.WAIT_OBJECT_0 {
		return nil
	}
	path := make([]uint16, 32768)
	n := uint32(len(path))
	if windows.QueryFullProcessImageName(h, 0, &path[0], &n) != nil || !strings.EqualFold(filepath.Clean(windows.UTF16ToString(path[:n])), filepath.Join(root, "LinkWeaver.exe")) {
		return errors.New("运行中的程序路径不匹配，未关闭其他程序")
	}
	client := &http.Client{Timeout: 6 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := fmt.Sprintf("http://127.0.0.1:%d", instance.Port)
	res, err := client.Get(base + "/")
	if err != nil {
		return errors.New("无法连接运行中的程序，请先从托盘退出")
	}
	page, readErr := io.ReadAll(io.LimitReader(res.Body, 1024*1024))
	res.Body.Close()
	match := regexp.MustCompile(`name="lw-token" content="([a-f0-9]{64})"`).FindSubmatch(page)
	if readErr != nil || res.StatusCode != http.StatusOK || len(match) != 2 {
		return errors.New("无法识别运行中的程序，请先从托盘退出")
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/shutdown", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LinkWeaver-Token", string(match[1]))
	res, err = client.Do(req)
	if err != nil {
		return errors.New("无法正常退出程序，请先从托盘退出")
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("程序拒绝退出请求")
	}
	state, err := windows.WaitForSingleObject(h, 25000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		return errors.New("等待程序退出超时，请先从托盘退出后重试")
	}
	return nil
}
