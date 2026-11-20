package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAttachmentUploadFormatRejection(t *testing.T) {
	for _, tc := range []struct {
		name, phase, body string
		status, want      int
	}{
		{"presign_415", "presign", "native unsupported", 415, 415},
		{"presign_400", "presign", `{"error":{"code":"unsupported_file_type","message":"HARK internal diagnostic"}}`, 400, 415},
		{"finalize_422", "finalize", `{"error":"Unsupported file type: .fixture"}`, 422, 415},
		{"storage_415", "put", "", 415, 415},
		{"unrelated_400", "presign", `{"error":"Invalid attachment metadata"}`, 400, 400},
		{"quota_402", "presign", `{"code":"token_limit_reached","error":"Token limit exceeded"}`, 402, 402},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var phase string
				switch r.URL.Path {
				case "/api/conversations":
					io.WriteString(w, `{"conversationId":"fixture-project"}`)
					return
				case "/api/files/upload/fixture-project/presign":
					phase = "presign"
				case "/upload":
					phase = "put"
				case "/api/files/fixture-project/att-fixture/finalize":
					phase = "finalize"
				default:
					t.Errorf("unexpected upstream request after attachment rejection: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if phase == tc.phase {
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
					return
				}
				if phase == "presign" {
					_ = json.NewEncoder(w).Encode(map[string]string{
						"upload_url": "http://" + r.Host + "/upload", "attachment_id": "att-fixture",
						"path": "path-fixture", "s3_key": "key-fixture",
					})
					return
				}
				io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			server, err := NewEmbedded(t.TempDir(), Options{Upstream: upstream.URL, PollInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]any{
				"model": "hark", "stream": true,
				"input": []any{map[string]any{"role": "user", "content": []any{
					map[string]string{"type": "input_text", "text": "Read the test attachment"},
					map[string]string{"type": "input_file", "filename": "filename.fixture", "file_data": base64.StdEncoding.EncodeToString([]byte("fixture"))},
				}}},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(WithEmbeddedAccount(context.Background(), SourceAccount{ID: "fixture", Cookie: "fixture-cookie"}))
			rec := httptest.NewRecorder()
			server.EmbeddedHandler().ServeHTTP(rec, req)
			body := rec.Body.String()
			if rec.Code != tc.want || strings.Contains(body, "unsupported_file_type") != (tc.want == 415) {
				t.Fatalf("HTTP %d: %s", rec.Code, body)
			}
			if tc.want == 415 && (!strings.Contains(body, "不支持 .fixture 格式的文件") || strings.Contains(strings.ToLower(body), "hark")) {
				t.Fatalf("unexpected public format error: %s", body)
			}
		})
	}
}

func TestAttachmentTransferPreservesBytes(t *testing.T) {
	names := []string{"sample.xls", "sample.xlsx", "sample.csv", "sample.pdf", "sample.docx",
		"sample.pptx", "sample.txt", "sample.md", "sample.png", "sample.jpeg",
		"sample.mp3", "sample.mp4", "sample.zip", "sample.future", "README"}
	seen := make(chan []byte, len(names))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/upload/fixture-project/presign":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"upload_url":    "http://" + r.Host + "/upload",
				"attachment_id": "att-fixture", "path": "path-fixture", "s3_key": "key-fixture",
			})
		case "/upload":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read uploaded bytes: %v", err)
			}
			seen <- data
		case "/api/files/fixture-project/att-fixture/finalize":
			io.WriteString(w, "{}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	server, err := NewEmbedded(t.TempDir(), Options{Upstream: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		want := []byte{0, byte(i), 255}
		att, err := server.uploadFile(context.Background(), "fixture-cookie", "fixture-project",
			newInputFile(name, "", want, "file"))
		if err != nil || att.Name != name || att.Size != int64(len(want)) {
			t.Fatalf("upload %s: %+v, %v", name, att, err)
		}
		select {
		case got := <-seen:
			if !bytes.Equal(want, got) {
				t.Errorf("%s: got %v, want %v", name, got, want)
			}
		default:
			t.Errorf("%s: no upload received", name)
		}
	}
}
