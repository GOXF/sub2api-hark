package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	maxFile       = 20 << 20
	maxBody       = maxFile/3*4*3 + (2 << 20)
	maxSelections = 16
	wrapperHeader = "# Files mentioned by the user:"
	requestMarker = "\n## My request:"
)

type object = map[string]json.RawMessage
type confirmation func(destination string, paths []string) bool

type snapshot struct {
	key    [32]byte
	parts  []json.RawMessage
	size   int
	expiry time.Time
}

type attachmentState struct {
	root        *os.Root
	destination string
	confirm     confirmation
	history     []snapshot // prepare is called by one request at a time
}

type localSelection struct {
	path  string
	image bool
}

func wrapJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only JSON parsed values and our own structs are marshalled
	}
	return b
}

func asString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func parseWrapper(text string) (string, []localSelection) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if !strings.HasPrefix(text, wrapperHeader+"\n") {
		return "", nil
	}
	header, ask, ok := strings.Cut(text, requestMarker)
	if !ok || ask != "" && !strings.HasPrefix(ask, "\n") {
		return "", nil
	}
	var selected []localSelection
	for _, line := range strings.Split(strings.TrimPrefix(header, wrapperHeader), "\n") {
		line = strings.TrimSpace(line)
		if label, ok := strings.CutPrefix(line, "## "); ok {
			name, path, found := strings.Cut(label, ":")
			if found && strings.TrimSpace(name) != "" && strings.TrimSpace(path) != "" {
				selected = append(selected, localSelection{path: strings.TrimSpace(path)})
			}
		} else if line == "Image attachment: true" && len(selected) != 0 {
			selected[len(selected)-1].image = true
		}
	}
	return strings.TrimSpace(ask), selected
}

func asParts(raw json.RawMessage) []object {
	var content []object
	if json.Unmarshal(raw, &content) == nil {
		return content
	}
	return []object{{"type": wrapJSON("input_text"), "text": wrapJSON(asString(raw))}}
}

// Only request missing selections. Native input_file and input_image parts
// already carry content, and must not be read again from a local path.
func missingSelections(selected []localSelection, parts []object) []localSelection {
	files := map[string]int{}
	images := 0
	for _, part := range parts {
		switch asString(part["type"]) {
		case "input_file":
			if asString(part["file_data"]) != "" || asString(part["file_url"]) != "" || asString(part["file_id"]) != "" {
				files[strings.ToLower(asString(part["filename"]))]++
			}
		case "input_image":
			if asString(part["image_url"]) != "" || asString(part["file_id"]) != "" {
				images++
			}
		}
	}
	out := selected[:0]
	for _, sel := range selected {
		name := strings.ToLower(filepath.Base(sel.path))
		if files[name] > 0 {
			files[name]--
			continue
		}
		if sel.image && images > 0 {
			images--
			continue
		}
		out = append(out, sel)
	}
	return out
}

func sessionScope(req object, h http.Header) string {
	var id string
	for _, name := range []string{"X-Hark-Session-Id", "X-Session-Id", "Session_id", "Session-Id"} {
		if id = h.Get(name); id != "" {
			break
		}
	}
	for _, name := range []string{"session_id", "prompt_cache_key"} {
		if id == "" {
			id = asString(req[name])
		}
	}
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 256 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return ""
	}
	return fmt.Sprintf("%q/%q/%q/%q", id, asString(req["model"]), h.Get("Authorization"), h.Get("X-Api-Key"))
}

func (s *attachmentState) prepare(ctx context.Context, header http.Header, body []byte) ([]byte, error) {
	var req object
	if json.Unmarshal(body, &req) != nil || req == nil {
		return nil, &relayError{400, "attachment_relay_json", "请求需要有效的 JSON 对象"}
	}
	var items []object
	if json.Unmarshal(req["input"], &items) != nil {
		text := asString(req["input"])
		if _, selections := parseWrapper(text); len(selections) == 0 {
			return body, nil
		}
		items = []object{{"role": wrapJSON("user"), "content": wrapJSON(text)}}
	}
	scope := sessionScope(req, header)
	now := time.Now()
	fresh := s.history[:0]
	for _, entry := range s.history {
		if now.Before(entry.expiry) {
			fresh = append(fresh, entry)
		}
	}
	clear(s.history[len(fresh):])
	s.history = fresh
	var pending *snapshot
	changed := false
	for i, item := range items {
		if asString(item["role"]) != "user" || (asString(item["type"]) != "" && asString(item["type"]) != "message") {
			continue
		}
		parts := asParts(item["content"])
		for j, part := range parts {
			if asString(part["type"]) != "input_text" {
				continue
			}
			ask, selected := parseWrapper(asString(part["text"]))
			if len(selected) == 0 {
				continue
			}
			selected = missingSelections(selected, parts)
			if len(selected) == 0 {
				continue
			}
			if scope == "" {
				return nil, &relayError{400, "attachment_session_missing", "这轮缺少会话标识，未读取附件"}
			}
			key := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d/%s", scope, i, j, part["text"])))
			var files []json.RawMessage
			for _, entry := range s.history {
				if entry.key == key {
					files = entry.parts
					break
				}
			}
			if files == nil {
				if i != len(items)-1 || pending != nil {
					return nil, &relayError{409, "attachment_approval_missing", "历史附件的本机确认已失效，请在新会话重新选择文件"}
				}
				var err error
				files, err = s.readSelections(ctx, selected)
				if err != nil {
					return nil, err
				}
				pending = &snapshot{key: key, parts: files, expiry: time.Now().Add(30 * time.Minute)}
				for _, file := range files {
					pending.size += len(file)
				}
			}
			part["text"] = wrapJSON(ask)
			wire := make([]json.RawMessage, 0, len(parts)+len(files))
			for _, p := range parts {
				wire = append(wire, wrapJSON(p))
			}
			item["content"] = wrapJSON(append(wire, files...))
			changed = true
			break // one picker wrapper per user message
		}
	}
	if !changed {
		return body, nil
	}
	req["input"] = wrapJSON(items)
	body = wrapJSON(req)
	if len(body) > maxBody {
		return nil, &relayError{413, "attachment_request_too_large", "附件和会话内容合计过大，请减少文件后重试"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pending != nil {
		total := pending.size
		for _, entry := range s.history {
			total += entry.size
		}
		for total > maxBody || len(s.history) >= 64 {
			total -= s.history[0].size
			s.history[0] = snapshot{}
			s.history = s.history[1:]
		}
		s.history = append(s.history, *pending)
	}
	return body, nil
}

func localPath(name string) bool {
	return filepath.IsAbs(name) && !strings.HasPrefix(strings.ReplaceAll(name, "\\", "/"), "//") &&
		strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf) }) < 0
}

func (s *attachmentState) readSelections(ctx context.Context, selected []localSelection) ([]json.RawMessage, error) {
	if len(selected) > maxSelections {
		return nil, &relayError{400, "too_many_files", "一次最多选择 16 个附件"}
	}
	reject := &relayError{403, "attachment_path_not_allowed", "只允许选择指定目录中的普通本机文件，不读取文件夹、链接或网络路径"}
	invalid := &relayError{400, "attachment_unreadable", "附件无法读取、为空或已变更，请重新选择"}
	tooLarge := &relayError{413, "file_too_large", "附件超过大小限制，请减少文件后重试"}
	paths, relative := make([]string, 0, len(selected)), make([]string, 0, len(selected))
	for _, sel := range selected {
		if !localPath(sel.path) {
			return nil, reject
		}
		path := filepath.Clean(sel.path)
		rel, err := filepath.Rel(s.root.Name(), path)
		if err != nil || !filepath.IsLocal(rel) || rel == "." || strings.ContainsRune(rel, ':') {
			return nil, reject
		}
		paths, relative = append(paths, path), append(relative, rel)
	}
	if ctx.Err() != nil || !s.confirm(s.destination, paths) || ctx.Err() != nil {
		return nil, &relayError{403, "attachment_not_approved", "未确认发送附件，本次没有读取文件"}
	}
	remaining := 60 << 20
	parts := make([]json.RawMessage, 0, len(relative))
	for _, rel := range relative {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Root handles containment even if a parent path is renamed during approval.
		for p := rel; p != "."; p = filepath.Dir(p) {
			fi, err := s.root.Lstat(p)
			if err != nil || isReparsePoint(fi) || (p == rel && !fi.Mode().IsRegular()) {
				return nil, reject
			}
		}
		f, err := s.root.Open(rel)
		if err != nil {
			return nil, invalid
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			_ = f.Close()
			return nil, invalid
		}
		limit := min(maxFile, remaining)
		data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
		_ = f.Close()
		if err != nil || len(data) == 0 {
			return nil, invalid
		}
		if len(data) > limit {
			return nil, tooLarge
		}
		remaining -= len(data)
		parts = append(parts, wrapJSON(map[string]string{
			"type": "input_file", "filename": filepath.Base(rel), "file_data": base64.StdEncoding.EncodeToString(data),
		}))
	}
	return parts, nil
}
