package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunEventReplayCursorScopeAndReset(t *testing.T) {
	a := setupA1(t, filepath.Join(t.TempDir(), "run-replay.sqlite"))
	defer a.close()
	ctx := t.Context()
	terminal, err := a.store.Submit(ctx, "fixture-terminal-run", "test first run", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.StopConversation(ctx, terminal.ConversationID, terminal.ID); err != nil {
		t.Fatal(err)
	}
	active, err := a.store.Submit(ctx, "fixture-active-run", "test second run", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := a.store.Events(terminal.ID, 0)
	if err != nil || len(first) < 2 {
		t.Fatalf("terminal fixture: %+v (%v)", first, err)
	}
	second, _, err := a.store.Events(active.ID, 0)
	if err != nil || len(second) == 0 {
		t.Fatalf("active fixture: %+v (%v)", second, err)
	}
	base := "/api/v1/runs/" + terminal.ID
	activeBase := "/api/v1/runs/" + active.ID
	seq := func(n int64) string { return strconv.FormatInt(n, 10) }
	frame := func(n int64) string { return fmt.Sprintf("id: %d\n", n) }
	fetch := func(path, header string, want int) string {
		t.Helper()
		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req := a.request(http.MethodGet, path, "").WithContext(reqCtx)
		if header != "" {
			req.Header.Set("Last-Event-ID", header)
		}
		res, err := a.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("GET %s with cursor %q: %d %s (%v)", path, header, res.StatusCode, body, err)
		}
		if want == http.StatusConflict && (res.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json")) {
			t.Fatalf("reset was not returned as non-cacheable JSON: %v", res.Header)
		}
		return string(body)
	}
	wantDone := "event: done\ndata: {}\n\n"
	for _, query := range []string{"", "?after=0"} {
		got := fetch(base+"/events"+query, "", 200)
		if !strings.Contains(got, frame(first[0].Seq)) || !strings.Contains(got, wantDone) {
			t.Fatalf("initial replay: %s", got)
		}
	}
	for _, input := range [][2]string{{"?after=" + seq(first[0].Seq), ""}, {"?after=9223372036854775807", seq(first[0].Seq)}} {
		got := fetch(base+"/events"+input[0], input[1], 200)
		if strings.Contains(got, frame(first[0].Seq)) || !strings.Contains(got, frame(first[1].Seq)) || !strings.Contains(got, wantDone) {
			t.Fatalf("valid resume: %s", got)
		}
	}
	for _, test := range [][2]string{
		{base + "/events?after=" + seq(second[0].Seq), ""},
		{activeBase + "/events", seq(first[0].Seq)},
		{activeBase + "/events", "9223372036854775807"},
	} {
		got := fetch(test[0], test[1], 409)
		if !strings.Contains(got, `"run_event_cursor_reset"`) || strings.Contains(got, terminal.ID) || strings.Contains(got, active.ID) {
			t.Fatalf("unexpected cursor reset body: %s", got)
		}
	}
	if got := fetch(base+"/events", "-1", 400); !strings.Contains(got, `"invalid_cursor"`) {
		t.Fatalf("malformed cursor: %s", got)
	}
	// While the run is still queued, a valid cursor opens the SSE instead
	// of returning done. Cancel the local HTTP connection after headers.
	activeCtx, cancelActive := context.WithTimeout(ctx, 2*time.Second)
	defer cancelActive()
	req := a.request(http.MethodGet, activeBase+"/events", "").WithContext(activeCtx)
	req.Header.Set("Last-Event-ID", seq(second[len(second)-1].Seq))
	res, err := a.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	isStream := res.StatusCode == http.StatusOK && res.Header.Get("Content-Type") == "text/event-stream"
	cancelActive()
	res.Body.Close()
	if !isStream {
		t.Fatalf("valid active replay did not open a stream: %d %v", res.StatusCode, res.Header)
	}
	if got := fetch(activeBase, "", 200); !strings.Contains(got, active.ID) || !strings.Contains(got, `"queued"`) {
		t.Fatalf("current snapshot unavailable after reset: %s", got)
	}
}
