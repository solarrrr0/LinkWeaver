package main

import (
	"errors"
	"os/exec"
	"strings"
)

func launchClashURI(uri string) error {
	if !strings.HasPrefix(uri, "clash://install-config?") {
		return errors.New("Clash 导入地址无效")
	}
	check := exec.Command("reg.exe", "query", `HKCR\clash\shell\open\command`, "/ve")
	hideWindow(check)
	if err := check.Run(); err != nil {
		return errors.New("未检测到 Clash 导入协议，请使用“复制 Clash 导入链接”在客户端的订阅页导入")
	}
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", uri)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return errors.New("无法唤起 Clash，请复制导入链接到客户端")
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
