package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const maxFilesPerRequest = 16

var errTooLarge = errors.New("attachment too large")

// harkAttachment is the attachment object Hark expects in /api/messages/send,
// following jyao0708/hark2api and ozuijoy/hark2api-node (presign → PUT → finalize).
type harkAttachment struct {
	AttachmentID string `json:"attachment_id"`
	Name         string `json:"name"`
	MediaType    string `json:"media_type"`
	Size         int64  `json:"size"`
	Path         string `json:"path"`
	S3Key        string `json:"s3_key"`
	Kind         string `json:"kind"`
}

func newInputFile(name, mediaType string, data []byte, kind string) inputFile {
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	if mediaType == "" || mediaType == "application/octet-stream" {
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); byExt != "" {
			mediaType = strings.Split(byExt, ";")[0]
		} else if len(data) > 0 {
			mediaType = strings.Split(http.DetectContentType(data), ";")[0]
		}
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	if kind == "" {
		kind = "file"
		if strings.HasPrefix(mediaType, "image/") {
			kind = "image"
		}
	}
	name = safeFilename(name)
	if name == "" {
		name = kind
		if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
			name += preferredExt(mediaType, exts)
		} else if kind == "image" {
			name += ".png"
		} else {
			name += ".bin"
		}
	}
	return inputFile{Kind: kind, Name: name, MediaType: mediaType, Data: data}
}

func preferredExt(mediaType string, exts []string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "text/plain":
		return ".txt"
	}
	return exts[0]
}

func safeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = name[:200-len(ext)] + ext
	}
	return name
}

func decodeBase64(data string, limit int64) ([]byte, error) {
	data = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, data)
	if int64(len(data))/4*3 > limit+3 {
		return nil, errTooLarge
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(data); err == nil {
			if len(b) == 0 {
				return nil, errors.New("empty")
			}
			if int64(len(b)) > limit {
				return nil, errTooLarge
			}
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// parseDataURL decodes data:[<mediatype>][;base64],<data>.
func parseDataURL(raw string, limit int64) (string, []byte, error) {
	rest, ok := strings.CutPrefix(raw, "data:")
	if !ok {
		return "", nil, errors.New("not a data URL")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, errors.New("data URL missing comma")
	}
	params := strings.Split(meta, ";")
	mediaType := strings.ToLower(strings.TrimSpace(params[0]))
	isBase64 := false
	for _, p := range params[1:] {
		if strings.EqualFold(strings.TrimSpace(p), "base64") {
			isBase64 = true
		}
	}
	if mediaType != "" {
		if _, _, err := mime.ParseMediaType(mediaType); err != nil {
			return "", nil, errors.New("bad media type")
		}
	}
	if isBase64 {
		b, err := decodeBase64(payload, limit)
		return mediaType, b, err
	}
	text, err := url.PathUnescape(payload)
	if err != nil {
		return "", nil, err
	}
	if int64(len(text)) > limit {
		return "", nil, errTooLarge
	}
	if text == "" {
		return "", nil, errors.New("empty")
	}
	return mediaType, []byte(text), nil
}

func (s *Server) fileFromURL(kind, name, raw, code string) (inputFile, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "data:") {
		mediaType, data, err := parseDataURL(raw, s.maxUpload)
		if err != nil {
			return inputFile{}, fileDataError(err, code, "data: URL 无效或不是有效的 base64")
		}
		if kind == "image" && mediaType != "" && !strings.HasPrefix(mediaType, "image/") {
			return inputFile{}, inputError(code, "image_url 的 data: URL 必须是 image/* 类型")
		}
		return newInputFile(name, mediaType, data, kind), nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return inputFile{}, inputError(code, "URL 需要是 data: 或 http(s) 地址")
	}
	if name == "" {
		name = safeFilename(u.Path)
	}
	return inputFile{Kind: kind, Name: name, URL: u.String()}, nil
}

// privateAddressGuard refuses connections to loopback, private, link-local
// and unspecified addresses, so a remote image URL cannot reach the gateway
// host's internal network (SSRF). Tests and local mode may disable it.
func privateAddressGuard(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("refusing to fetch from %s", host)
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64 { // CGNAT 100.64/10
		return fmt.Errorf("refusing to fetch from %s", host)
	}
	return nil
}

func (s *Server) newFetchClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !s.allowPrivateFetch {
		dialer.Control = privateAddressGuard
	}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, Proxy: nil}
	return &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
			return errors.New("bad redirect scheme")
		}
		return nil
	}}
}

// resolveFiles downloads http(s) attachments of the messages about to be sent.
func (s *Server) resolveFiles(ctx context.Context, messages []chatMessage) error {
	for i := range messages {
		for j := range messages[i].Files {
			f := &messages[i].Files[j]
			if f.URL == "" || len(f.Data) > 0 {
				continue
			}
			data, mediaType, err := s.fetchRemote(ctx, f.URL)
			if err != nil {
				return err
			}
			if f.Kind == "image" && mediaType != "" && !strings.HasPrefix(mediaType, "image/") {
				sniffed := strings.Split(http.DetectContentType(data), ";")[0]
				if !strings.HasPrefix(sniffed, "image/") {
					return &statusInputError{400, "invalid_image_url", "image_url 指向的内容不是图片"}
				}
				mediaType = sniffed
			}
			resolved := newInputFile(f.Name, mediaType, data, f.Kind)
			resolved.URL = f.URL
			*f = resolved
		}
	}
	return nil
}

func (s *Server) fetchRemote(ctx context.Context, raw string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", &statusInputError{400, "invalid_image_url", "附件 URL 无效"}
	}
	req.Header.Set("User-Agent", "hark2api/1.0")
	resp, err := s.fetchClient.Do(req)
	if err != nil {
		return nil, "", &statusInputError{400, "attachment_fetch_failed", "附件 URL 下载失败（不可达或被安全策略拒绝）"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", &statusInputError{400, "attachment_fetch_failed", fmt.Sprintf("附件 URL 下载失败：HTTP %d", resp.StatusCode)}
	}
	if resp.ContentLength > s.maxUpload {
		return nil, "", &statusInputError{413, "file_too_large", "附件超过大小限制"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.maxUpload+1))
	if err != nil {
		return nil, "", &statusInputError{400, "attachment_fetch_failed", "附件 URL 读取失败"}
	}
	if int64(len(data)) > s.maxUpload {
		return nil, "", &statusInputError{413, "file_too_large", "附件超过大小限制"}
	}
	if len(data) == 0 {
		return nil, "", &statusInputError{400, "attachment_fetch_failed", "附件 URL 内容为空"}
	}
	return data, strings.Split(resp.Header.Get("Content-Type"), ";")[0], nil
}

// uploadFile follows the presign flow of jyao0708/hark2api and
// ozuijoy/hark2api-node: POST presign → PUT upload_url → POST finalize.
func (s *Server) uploadFile(ctx context.Context, cookie, cid string, f inputFile) (harkAttachment, error) {
	var presigned struct {
		UploadURL    string `json:"upload_url"`
		AttachmentID string `json:"attachment_id"`
		S3Key        string `json:"s3_key"`
		Path         string `json:"path"`
		ContentType  string `json:"content_type"`
	}
	if err := s.harkJSON(ctx, http.MethodPost, "/api/files/upload/"+url.PathEscape(cid)+"/presign", cookie,
		map[string]any{"filename": f.Name, "content_type": f.MediaType, "size": len(f.Data)}, &presigned); err != nil {
		return harkAttachment{}, err
	}
	if !strings.HasPrefix(presigned.UploadURL, "http://") && !strings.HasPrefix(presigned.UploadURL, "https://") {
		return harkAttachment{}, &upstreamError{502, "upstream_upload_invalid", "Hark 未返回有效的附件上传地址"}
	}
	if presigned.AttachmentID == "" || presigned.S3Key == "" || presigned.Path == "" {
		return harkAttachment{}, &upstreamError{502, "upstream_upload_invalid", "Hark 返回的附件元数据不完整"}
	}
	contentType := presigned.ContentType
	if contentType == "" {
		contentType = f.MediaType
	}
	upCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(upCtx, http.MethodPut, presigned.UploadURL, bytes.NewReader(f.Data))
	if err != nil {
		return harkAttachment{}, &upstreamError{502, "upstream_upload_invalid", "Hark 附件上传地址无效"}
	}
	req.ContentLength = int64(len(f.Data))
	req.Header.Set("Content-Type", contentType)
	client := s.uploadClientFor(cookie)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return harkAttachment{}, &upstreamError{502, "upstream_upload_failed", "附件上传到 Hark 存储失败"}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return harkAttachment{}, &upstreamError{502, "upstream_upload_failed", fmt.Sprintf("附件上传到 Hark 存储失败：HTTP %d", resp.StatusCode)}
	}
	var finalized struct {
		AttachmentID string `json:"attachment_id"`
		Name         string `json:"name"`
		MediaType    string `json:"media_type"`
		Size         int64  `json:"size"`
		Path         string `json:"path"`
		S3Key        string `json:"s3_key"`
	}
	if err := s.harkJSON(ctx, http.MethodPost, "/api/files/"+url.PathEscape(cid)+"/"+url.PathEscape(presigned.AttachmentID)+"/finalize", cookie,
		map[string]any{"s3_key": presigned.S3Key, "path": presigned.Path}, &finalized); err != nil {
		return harkAttachment{}, err
	}
	a := harkAttachment{AttachmentID: presigned.AttachmentID, Name: f.Name, MediaType: f.MediaType, Size: int64(len(f.Data)), Path: presigned.Path, S3Key: presigned.S3Key, Kind: "file"}
	if finalized.AttachmentID != "" {
		a.AttachmentID = finalized.AttachmentID
	}
	if finalized.Name != "" {
		a.Name = finalized.Name
	}
	if finalized.MediaType != "" {
		a.MediaType = finalized.MediaType
	}
	if finalized.Size > 0 {
		a.Size = finalized.Size
	}
	if finalized.Path != "" {
		a.Path = finalized.Path
	}
	if finalized.S3Key != "" {
		a.S3Key = finalized.S3Key
	}
	return a, nil
}
