package gateway

import (
	"context"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Hark replies can carry file attachments (downloadUrl under /api/files/...).
// They need the account Cookie to download, so the gateway stores a copy under
// data/media and links it as /v1/media/{name} in the reply text.

var mediaName = regexp.MustCompile(`^[a-f0-9]{32}\.[a-z0-9]{1,8}$`)
var extPattern = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)

const maxReplyMedia = 128 * 1024 * 1024

func (s *Server) mediaDir() string { return filepath.Join(s.data, "media") }

func (s *Server) publicBase(r *http.Request) string {
	if s.publicBaseURL != "" {
		return s.publicBaseURL
	}
	if s.publicOrigin != "" {
		return s.publicOrigin
	}
	return requestOrigin(r)
}

func mediaExt(name, mediaType string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if extPattern.MatchString(ext) {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		e := preferredExt(mediaType, exts)
		if extPattern.MatchString(e) {
			return e
		}
	}
	return ".bin"
}

func (s *Server) downloadReplyFile(ctx context.Context, cookie string, f harkFile) (string, string, error) {
	if !strings.HasPrefix(f.DownloadURL, "/api/files/") {
		return "", "", &upstreamError{502, "upstream_attachment_invalid", "Hark 返回了无效的附件地址"}
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.upstream+f.DownloadURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Cookie", cookie)
	// Follow redirects to object storage; Go drops the Cookie header when the
	// redirect leaves the Hark host.
	resp, err := s.clientFor(cookie, 0, true).Do(req)
	if err != nil {
		return "", "", &upstreamError{502, "upstream_attachment_failed", "Hark 附件下载失败"}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", &upstreamError{502, "upstream_attachment_failed", "Hark 附件下载失败"}
	}
	mediaType := strings.Split(f.MediaType, ";")[0]
	if mediaType == "" {
		mediaType = strings.Split(resp.Header.Get("Content-Type"), ";")[0]
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") && !strings.HasSuffix(strings.ToLower(f.Name), ".html") {
		return "", "", &upstreamError{502, "upstream_session_expired", "Hark 返回登录页面而不是附件"}
	}
	if err := os.MkdirAll(s.mediaDir(), 0700); err != nil {
		return "", "", err
	}
	name := randomID("")[:32] + mediaExt(f.Name, mediaType)
	tmp, err := os.CreateTemp(s.mediaDir(), ".part-*")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxReplyMedia+1))
	tmp.Close()
	if err != nil || n > maxReplyMedia {
		return "", "", &upstreamError{502, "upstream_attachment_failed", "Hark 附件过大或读取失败"}
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.mediaDir(), name)); err != nil {
		return "", "", err
	}
	return name, mediaType, nil
}

func (s *Server) replyAttachmentsMarkdown(ctx context.Context, r *http.Request, cookie string, files []harkFile) string {
	base := strings.TrimRight(s.publicBase(r), "/")
	var links []string
	for _, f := range files {
		label := strings.NewReplacer("[", "(", "]", ")", "\n", " ").Replace(f.Name)
		if label == "" {
			label = "attachment"
		}
		name, mediaType, err := s.downloadReplyFile(ctx, cookie, f)
		if err != nil {
			s.projectWarning("回复附件下载失败：" + label)
			links = append(links, "[附件下载失败: "+label+"]")
			continue
		}
		url := base + "/v1/media/" + name
		if strings.HasPrefix(mediaType, "image/") {
			links = append(links, "!["+label+"]("+url+")")
		} else {
			links = append(links, "["+label+"]("+url+")")
		}
	}
	return strings.Join(links, "\n\n")
}

// serveMedia needs no API key: names are 128-bit random and only reachable
// through reply text. Non-image types are served as downloads.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !mediaName.MatchString(name) {
		fail(w, 404, "not_found", "文件不存在")
		return
	}
	path := filepath.Join(s.mediaDir(), name)
	f, err := os.Open(path)
	if err != nil {
		fail(w, 404, "not_found", "文件不存在")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		fail(w, 404, "not_found", "文件不存在")
		return
	}
	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if !strings.HasPrefix(ctype, "image/") || strings.Contains(ctype, "svg") {
		w.Header().Set("Content-Disposition", "attachment")
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// RunMediaCleanup deletes downloaded reply attachments older than retention.
func (s *Server) RunMediaCleanup(ctx context.Context, retention time.Duration) {
	clean := func() {
		entries, err := os.ReadDir(s.mediaDir())
		if err != nil {
			return
		}
		cutoff := time.Now().Add(-retention)
		for _, e := range entries {
			info, err := e.Info()
			if err == nil && !e.IsDir() && info.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(s.mediaDir(), e.Name()))
			}
		}
	}
	clean()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			clean()
		}
	}
}
