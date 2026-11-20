// An opt-in Windows loopback relay for Codex file selections.
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type relayError struct {
	status  int
	code    string
	message string
}

const relayTokenHeader = "X-Codex-Attachment-Token"

func (e *relayError) Error() string { return e.message }

func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"type": "invalid_request_error", "code": code, "message": message,
	}})
}

func upstreamOrigin(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err == nil && u.Hostname() != "" && strings.TrimSuffix(u.Path, "/") == "/v1" &&
		u.RawPath == "" && u.User == nil && !u.ForceQuery && u.RawQuery == "" && u.Fragment == "" {
		host := u.Hostname()
		if u.Scheme == "https" || u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
			u.Path = ""
			return u, nil
		}
	}
	return nil, errors.New("上游需为以 /v1 结尾的 HTTPS 地址，或本机的 HTTP 地址；不要包含密钥")
}

func newRelay(root *os.Root, upstream *url.URL, localToken string, confirm confirmation) http.Handler {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 180 * time.Second,
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.Header.Del(relayTokenHeader)
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("Referer")
		},
		ModifyResponse: func(r *http.Response) error {
			r.Header.Del("Set-Cookie")
			if r.StatusCode >= 300 && r.StatusCode < 400 {
				return errors.New("upstream redirect refused")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if r.Context().Err() == nil {
				fail(w, http.StatusBadGateway, "attachment_relay_upstream", "连接上游失败，请检查本机模型线路")
			}
		},
	}
	attachments := &attachmentState{root: root, destination: upstream.String(), confirm: confirm}
	// Bound concurrent request buffers and dialogs. Stream through the proxy
	// on the original request context so a cancelled client cannot start a send.
	slot := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		if !ok || r.Host != local.String() || r.URL.IsAbs() || r.Header.Get("Origin") != "" ||
			r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Upgrade") != "" {
			fail(w, http.StatusForbidden, "attachment_relay_local_only", "仅接受本机客户端请求")
			return
		}
		tokens := r.Header.Values(relayTokenHeader)
		if len(tokens) != 1 || subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(localToken)) != 1 {
			fail(w, http.StatusUnauthorized, "attachment_relay_token", "本机附件中转令牌缺失或不匹配")
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			proxy.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/responses/compact") {
			fail(w, http.StatusNotFound, "attachment_relay_route", "仅支持模型列表和 Responses 请求")
			return
		}
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct != "application/json" || (r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity") {
			fail(w, http.StatusUnsupportedMediaType, "attachment_relay_json", "请发送未压缩的 JSON 请求")
			return
		}
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		default:
			fail(w, http.StatusTooManyRequests, "attachment_relay_busy", "已有请求在处理，请等当前请求结束")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				fail(w, http.StatusRequestEntityTooLarge, "attachment_relay_too_large", "请求超过附件大小限制")
			} else {
				fail(w, http.StatusBadRequest, "attachment_relay_body", "无法读取请求内容")
			}
			return
		}
		if r.URL.Path == "/v1/responses" {
			body, err = attachments.prepare(r.Context(), r.Header, body)
			if err != nil {
				var rejected *relayError
				if errors.As(err, &rejected) && r.Context().Err() == nil {
					fail(w, rejected.status, rejected.code, rejected.message)
				}
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Del("Content-Length")
		proxy.ServeHTTP(w, r)
	})
}

func run(base, dir string, port int) error {
	if runtime.GOOS != "windows" {
		return errors.New("当前本机确认窗口仅支持 Windows")
	}
	token := os.Getenv("CODEX_ATTACHMENT_TOKEN")
	if data, err := hex.DecodeString(token); err != nil || len(data) != 32 {
		return errors.New("请在当前终端的 CODEX_ATTACHMENT_TOKEN 设置 32 字节随机十六进制令牌；不要用 API Key")
	}
	upstream, err := upstreamOrigin(base)
	if err != nil {
		return err
	}
	if port < 1 || port > 65535 || dir == "" {
		return errors.New("请指定附件目录和有效的本机监听端口")
	}
	dir, err = filepath.Abs(dir)
	if err != nil || !localPath(dir) || filepath.Dir(dir) == dir || !isLocalVolume(dir) {
		return errors.New("请指定一个本机附件专用文件夹，不要选盘符根目录或网络路径")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || isReparsePoint(info) {
		return errors.New("附件目录不存在，或不是普通本机文件夹")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if upstream.Host == addr || upstream.Host == fmt.Sprintf("localhost:%d", port) || upstream.Host == fmt.Sprintf("[::1]:%d", port) {
		return errors.New("上游和本机中转不能使用同一地址")
	}
	srv := &http.Server{
		Addr: addr, Handler: newRelay(root, upstream, token, confirmFiles),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
	}
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	fmt.Printf("Codex 附件中转：http://%s/v1\n附件目录：%s\n上游：%s/v1\n", addr, dir, upstream)
	return srv.Serve(listener)
}

func main() {
	base := flag.String("upstream", "", "原来的模型 Base URL（以 /v1 结尾）")
	dir := flag.String("dir", "", "仅允许读取这个目录里经确认的文件")
	port := flag.Int("port", 64720, "本机监听端口")
	flag.Parse()
	if err := run(*base, *dir, *port); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
