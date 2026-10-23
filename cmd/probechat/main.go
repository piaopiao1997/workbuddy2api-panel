// probechat — 一次性诊断：对被剔除的账号发真实对话，验证是否仍被上游 11140 拒绝。
//
// 背景：11140 只打 chat_stream（真实对话），余额/签到类 RPC 完全不受影响。
// 所以「credit 能查到余额」不能证明「对话没被封」，必须真发一次对话。
//
// 用法（在 module 根目录）：
//
//	go run ./cmd/probechat -dir /probe/auths -model glm-5.2
//
// 输出每个号一行：uid8 / 昵称 / HTTP 状态 / 分类 / 响应片段。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

type authFile struct {
	Account struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	} `json:"auth"`
}

func main() {
	dir := flag.String("dir", "./auths", "auth 文件目录")
	model := flag.String("model", "glm-5.2", "测试模型")
	prompt := flag.String("prompt", "1+1等于几？直接回答。", "提示词")
	timeout := flag.Duration("timeout", 60*time.Second, "单个号超时")
	flag.Parse()

	files, err := filepath.Glob(filepath.Join(*dir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "没有找到 auth 文件（dir=%s）\n", *dir)
		os.Exit(1)
	}
	sort.Strings(files)

	c := upstream.New()

	fmt.Printf("%-9s %-20s %-6s %-18s %s\n", "UID8", "昵称", "HTTP", "分类", "响应片段")
	fmt.Println(strings.Repeat("-", 100))

	var okN, blockedN, otherN int
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var af authFile
		if json.Unmarshal(raw, &af) != nil || af.Account.UID == "" {
			continue
		}

		a := &auth.Auth{
			UID:          af.Account.UID,
			Nickname:     af.Account.Nickname,
			AccessToken:  af.Auth.AccessToken,
			RefreshToken: af.Auth.RefreshToken,
			ExpiresAt:    af.Auth.ExpiresAt,
			Domain:       af.Auth.Domain,
		}

		body, _ := json.Marshal(map[string]any{
			"model":    *model,
			"messages": []map[string]any{{"role": "user", "content": *prompt}},
			"stream":   true,
		})

		type result struct {
			status   int
			respBody []byte
			err      error
		}
		done := make(chan result, 1)
		go func() {
			rc, st, rb, e := c.ChatStream(a, body, "", upstream.ChatMeta{})
			if rc != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
				rc.Close()
			}
			done <- result{st, rb, e}
		}()

		var r result
		select {
		case r = <-done:
		case <-time.After(*timeout):
			r = result{err: fmt.Errorf("timeout")}
		}

		uid8 := af.Account.UID
		if len(uid8) > 8 {
			uid8 = uid8[:8]
		}
		nick := af.Account.Nickname
		if len([]rune(nick)) > 18 {
			nick = string([]rune(nick)[:18])
		}

		kind := "-"
		snippet := ""
		if r.err != nil {
			kind = "TRANSPORT"
			snippet = r.err.Error()
			otherN++
		} else {
			snippet = strings.ReplaceAll(string(r.respBody), "\n", " ")
			if len(snippet) > 120 {
				snippet = snippet[:120]
			}
			k := upstream.Classify(r.status, string(r.respBody))
			kind = k.String()
			switch k {
			case upstream.ErrNone:
				okN++
			case upstream.ErrAccountFault, upstream.ErrContentBlocked:
				blockedN++
			default:
				otherN++
			}
		}
		fmt.Printf("%-9s %-20s %-6d %-18s %s\n", uid8, nick, r.status, kind, snippet)
	}

	fmt.Println(strings.Repeat("-", 100))
	fmt.Printf("汇总: 正常=%d  账号级拒绝=%d  其他=%d\n", okN, blockedN, otherN)
}
