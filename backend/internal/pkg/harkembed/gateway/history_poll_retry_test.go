package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type historyPollTransport func(*http.Request) (*http.Response, error)

func (f historyPollTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHistoryPollReadRetry(t *testing.T) {
	for _, tc := range []struct {
		name, after, code                         string
		limited, status                           int
		gets                                      int
		wait                                      time.Duration
		date, cancel                              bool
		deadline, foreign, quotaStop, sendLimited bool
	}{
		{name: "transient", limited: 1, gets: 2, wait: time.Second},
		{name: "retry-after-seconds", after: "3", limited: 1, gets: 2, wait: 3 * time.Second},
		{name: "retry-after-date", date: true, limited: 1, gets: 2, wait: 3 * time.Second},
		{name: "invalid-header", after: "invalid", limited: 1, gets: 2, wait: time.Second},
		{name: "negative-header", after: "-1", limited: 1, gets: 2, wait: time.Second},
		{name: "persistent", limited: 100, gets: 5, wait: 15 * time.Second, code: "upstream_rate_limited"},
		{name: "total-wait-budget", limited: 100, after: "10", gets: 4, wait: 30 * time.Second, code: "upstream_rate_limited"},
		{name: "long-header-no-early-retry", limited: 1, after: "31", gets: 1, code: "upstream_rate_limited"},
		{name: "overflow-header-no-early-retry", limited: 1, after: "9999999999999999999999999999", gets: 1, code: "upstream_rate_limited"},
		{name: "cancel-during-backoff", limited: 1, cancel: true, gets: 1, wait: 100 * time.Millisecond, code: "cancelled"},
		{name: "deadline-during-backoff", limited: 1, deadline: true, gets: 1, wait: 100 * time.Millisecond, code: "deadline"},
		{name: "foreign-completion-ignored", limited: 1, foreign: true, gets: 3, wait: time.Second + time.Millisecond},
		{name: "auth-no-retry", status: 401, gets: 1, code: "upstream_session_expired"},
		{name: "blocked-no-retry", status: 403, gets: 1, code: "upstream_access_blocked"},
		{name: "quota-no-retry", status: 402, gets: 1, code: "upstream_quota_exhausted"},
		{name: "server-no-retry", status: 500, gets: 1, code: "upstream_http_error"},
		{name: "terminal-budget-no-retry", quotaStop: true, gets: 1, code: "upstream_budget_exhausted"},
		{name: "submission-429-never-replayed", sendLimited: true, gets: 0, code: "upstream_rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				posts, gets, accepted := 0, 0, 0
				var requestID, expected string
				transport := historyPollTransport(func(r *http.Request) (*http.Response, error) {
					if r.Header.Get("Cookie") != "fixture-cookie" {
						t.Fatal("original account changed")
					}
					status, header, body := 200, make(http.Header), any(nil)
					switch r.URL.Path {
					case "/api/messages/send":
						posts++
						var payload map[string]any
						if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&payload) != nil || payload["conversationId"] != "fixture-project" {
							t.Fatal("submission escaped original Project")
						}
						requestID, _ = payload["idempotencyKey"].(string)
						expected, _ = payload["responseMessageId"].(string)
						body = map[string]any{"success": true, "conversationId": "fixture-project", "messageId": requestID, "redirected": false}
						if tc.sendLimited {
							status, body = 429, map[string]string{"error": "rate_limited"}
						}
					case "/api/messages/history":
						gets++
						if r.Method != http.MethodGet || r.URL.Query().Get("conversationId") != "fixture-project" {
							t.Fatal("history escaped original Project")
						}
						if gets <= tc.limited {
							status, body = 429, map[string]string{"error": "endpoint_rate_limit"}
							header.Set("Retry-After", tc.after)
							if tc.date {
								header.Set("Retry-After", start.Add(3*time.Second).UTC().Format(http.TimeFormat))
							}
						} else if tc.status != 0 {
							status, body = tc.status, map[string]string{"error": "quota_exhausted"}
							if tc.status == 402 {
								body = map[string]string{"code": "token_limit_reached", "error": "fixture quota exhausted"}
							}
						} else {
							job, text, responseID := "completed", "fixture completed", expected
							if tc.quotaStop {
								job = "cost_capped"
							}
							if tc.foreign && gets == 2 {
								responseID = "foreign-job"
							}
							body = map[string]any{"messages": []any{
								map[string]string{"id": requestID, "role": "user"},
								map[string]any{"id": responseID, "role": "assistant", "triggeredByMessageId": requestID, "status": job, "content": text},
							}}
						}
					default:
						t.Fatalf("unexpected route %s", r.URL.Path)
					}
					data, _ := json.Marshal(body)
					return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
				})
				// A nonempty synthetic proxy retains the in-memory transport.
				s := &Server{upstream: "https://fixture.invalid", upstreamProxy: "http://fixture-proxy.invalid", client: &http.Client{Transport: transport}}
				ctx, cancel := context.WithCancel(context.Background())
				if tc.deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				}
				defer cancel()
				if tc.cancel {
					timer := time.AfterFunc(100*time.Millisecond, cancel)
					defer timer.Stop()
				}
				reply, err := s.sendProjectPayload(ctx, "fixture-cookie", "fixture-project", map[string]any{"conversationId": "fixture-project", "message": "fixture"}, time.Millisecond, turnHooks{accepted: func() { accepted++ }})
				t.Logf("text=%q POST=%d GET=%d accepted=%d wait=%s error=%v", reply.Text, posts, gets, accepted, time.Since(start), err)
				wantAccepted := 1
				if tc.sendLimited {
					wantAccepted = 0
				}
				if posts != 1 || gets != tc.gets || time.Since(start) != tc.wait || accepted != wantAccepted {
					t.Fatal("read-only retry count, delay, acceptance or original submission changed")
				}
				switch tc.code {
				case "":
					if err != nil || reply.Text != "fixture completed" {
						t.Fatalf("completed original result not returned: %v", err)
					}
				case "cancelled":
					if !errors.Is(err, context.Canceled) {
						t.Fatal("cancellation not preserved")
					}
				case "deadline":
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("deadline not preserved")
					}
				default:
					var upstream *upstreamError
					if !errors.As(err, &upstream) || upstream.code != tc.code {
						t.Fatalf("wrong terminal error: %v", err)
					}
				}
			})
		})
	}
}

func TestHistoryPollResponsesSessionContinues(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			projects, posts, gets := 0, 0, 0
			var turns []map[string]any
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/conversations":
					projects++
					fmt.Fprint(w, `{"conversationId":"poll-original-project"}`)
				case "/api/messages/send":
					posts++
					var p map[string]any
					_ = json.NewDecoder(r.Body).Decode(&p)
					turns = append(turns, p)
					if p["conversationId"] != "poll-original-project" || r.Header.Get("Cookie") != "fixture-cookie" {
						t.Error("original Project/account changed")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "conversationId": "poll-original-project", "messageId": p["idempotencyKey"], "redirected": false})
				case "/api/messages/history":
					gets++
					if gets == 1 {
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(429)
						fmt.Fprint(w, `{"error":"endpoint_rate_limit"}`)
						return
					}
					var messages []any
					for i, p := range turns {
						messages = append(messages, map[string]any{"id": p["idempotencyKey"], "role": "user"}, map[string]any{"id": p["responseMessageId"], "role": "assistant", "triggeredByMessageId": p["idempotencyKey"], "status": "completed", "content": fmt.Sprintf("fixture reply %d", i+1)})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages})
				default:
					fmt.Fprint(w, `{}`)
				}
			}))
			defer u.Close()
			dir := t.TempDir()
			s, err := NewEmbedded(dir, Options{Upstream: u.URL, PollInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			post := func(input []map[string]string) *httptest.ResponseRecorder {
				t.Helper()
				body, _ := json.Marshal(map[string]any{"model": "hark", "input": input, "stream": stream})
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Hark-Session-Id", "poll-session")
				r = r.WithContext(WithEmbeddedAccount(r.Context(), SourceAccount{ID: "fixture-account", Cookie: "fixture-cookie"}))
				w := httptest.NewRecorder()
				s.CallerHandler("fixture-owner").ServeHTTP(w, r)
				return w
			}
			input := []map[string]string{{"role": "user", "content": "first fixture"}}
			first := post(input)
			if first.Code != 200 || !strings.Contains(first.Body.String(), "fixture reply 1") || strings.Contains(first.Body.String(), "upstream_turn_interrupted") {
				t.Fatalf("first completed reply was lost: %d %s", first.Code, first.Body)
			}
			// Persisted success must not resurrect pending after a gateway restart.
			s, err = NewEmbedded(dir, Options{Upstream: u.URL, PollInterval: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			input = append(input, map[string]string{"role": "assistant", "content": "fixture reply 1"}, map[string]string{"role": "user", "content": "next fixture"})
			second := post(input)
			if second.Code != 200 || !strings.Contains(second.Body.String(), "fixture reply 2") {
				t.Fatalf("following turn blocked: %d %s", second.Code, second.Body)
			}
			mu.Lock()
			defer mu.Unlock()
			if projects != 1 || posts != 2 || gets != 3 {
				t.Fatalf("Project replaced or task replayed: projects=%d POST=%d GET=%d", projects, posts, gets)
			}
			for _, p := range s.projects {
				if p.PendingTurn != nil || p.ConversationID != "poll-original-project" || p.AccountID != "fixture-account" || p.Turns != 2 {
					t.Fatal("durable original session not reusable")
				}
			}
			t.Logf("first=200 content=fixture reply 1 next=200 content=fixture reply 2 projects=%d POST=%d GET=%d pending=false restart_reused=true", projects, posts, gets)
		})
	}
}
