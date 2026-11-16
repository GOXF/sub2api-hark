package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func issuedDurable(t *testing.T) *durableRig {
	t.Helper()
	x := newDurableRig(t, "responses", false)
	if w := x.post("fixture-key-A", x.path, x.body, false); w.Code != 409 {
		t.Fatal(w.Body)
	}
	if w := x.prepare(); w.Code != 200 {
		t.Fatal(w.Body)
	}
	return x
}

func TestDurableRecoveryTailCrashGuards(t *testing.T) {
	for _, mode := range []string{"unknown-acceptance", "store-before-send", "parallel-receipt"} {
		t.Run(mode, func(t *testing.T) {
			x := issuedDurable(t)
			w := x.post("fixture-key-A", x.path, x.body, false)
			id := regexp.MustCompile(`call_[a-f0-9]+`).FindString(w.Body.String())
			body := durableTail("responses", x.body, id)
			switch mode {
			case "unknown-acceptance":
				x.mu.Lock()
				x.tailFailure = 503
				x.mu.Unlock()
				w = x.post("fixture-key-A", x.path, body, false)
				if w.Code != 409 || !strings.Contains(w.Body.String(), "recovery_submission_uncertain") {
					t.Fatalf("unknown acceptance not retained: %d %s", w.Code, w.Body)
				}
				x.restart()
				w = x.post("fixture-key-A", x.path, body, false)
				if w.Code != 409 {
					t.Fatal("restart allowed unknown accepted tail replay")
				}
			case "store-before-send":
				x.s.data = filepath.Join(x.dir, "unavailable-parent")
				w = x.post("fixture-key-A", x.path, body, false)
				if w.Code < 400 {
					t.Fatal("tail was submitted without durable intent")
				}
				x.mu.Lock()
				count := x.sends
				x.mu.Unlock()
				if count != 1 {
					t.Fatal("tail submitted before persistence")
				}
				x.s.data = x.dir
				x.restart()
				w = x.post("fixture-key-A", x.path, body, false)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
			case "parallel-receipt":
				var wg sync.WaitGroup
				codes := make(chan int, 4)
				for i := 0; i < 4; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); codes <- x.post("fixture-key-A", x.path, body, false).Code }()
				}
				wg.Wait()
				close(codes)
				ok := 0
				for code := range codes {
					if code == 200 {
						ok++
					} else if code != 409 {
						t.Fatalf("unexpected receipt status %d", code)
					}
				}
				if ok != 1 {
					t.Fatalf("receipt applied %d times", ok)
				}
			}
			x.mu.Lock()
			defer x.mu.Unlock()
			if x.sends != 2 || x.archives != 0 {
				t.Fatalf("tail repeated: submissions=%d archives=%d", x.sends, x.archives)
			}
		})
	}
}

func TestDurableRecoveryLegacyAndCorruption(t *testing.T) {
	t.Run("legacy-no-owner", func(t *testing.T) {
		x := newDurableRig(t, "responses", false)
		w := x.post("", x.path, x.body, false)
		if w.Code != 409 {
			t.Fatal(w.Body)
		}
		w = x.prepare()
		if w.Code != 409 || !strings.Contains(w.Body.String(), "recovery_contract_unverified") {
			t.Fatal("legacy context was guessed or adopted")
		}
		for _, p := range x.s.projects {
			if p.OwnerID != "" || p.PendingTurn.Contract != nil {
				t.Fatal("legacy ownership changed")
			}
		}
	})
	t.Run("corrupt-wire-fails-closed", func(t *testing.T) {
		x := issuedDurable(t)
		path := filepath.Join(x.dir, "project-sessions.json")
		raw, _ := os.ReadFile(path)
		var rows map[string]map[string]any
		_ = json.Unmarshal(raw, &rows)
		for _, row := range rows {
			d := row["recovery_delivery"].(map[string]any)
			d["wire_sha256"] = strings.Repeat("0", 64)
		}
		raw, _ = json.Marshal(rows)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewEmbedded(x.dir, Options{Upstream: x.upstream}); err == nil {
			t.Fatal("corrupted saved wire was reissued after restart")
		}
	})
	t.Run("pending-cannot-end", func(t *testing.T) {
		x := issuedDurable(t)
		w := x.post("fixture-key-A", "/v1/sessions/original-session/end", `{}`, false)
		if w.Code != http.StatusConflict {
			t.Fatalf("archiving bypassed pending recovery: %d %s", w.Code, w.Body)
		}
	})
}
