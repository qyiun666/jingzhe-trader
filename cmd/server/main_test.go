package main

import "testing"

// 局域网暴露判定必须偏保守: 判成本机就会跳过 token 检查
func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":    true,
		"127.8.9.10":   true, // 整个 127/8 都是回环
		"localhost":    true,
		"::1":          true,
		"[::1]":        true,
		"0.0.0.0":      false,
		"::":           false,
		"192.168.28.1": false,
		"10.0.0.5":     false,
		"":             false,
		"example.com":  false,
	}
	for host, want := range cases {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, 期望 %v", host, got, want)
		}
	}
}
