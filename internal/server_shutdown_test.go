package internal

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startServer serves a one-file folder on a free port and returns the page
// URL, a function that stops the server, and a channel with serve's result.
func startServer(t *testing.T, reload, closeTabs bool) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	return startServerLog(t, reload, closeTabs, nil)
}

// startServerLog is startServer with a logger.
func startServerLog(t *testing.T, reload, closeTabs bool, logger *slog.Logger) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(file, []byte("# Doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer("localhost", 0, false, false, reload, NewParser(nil), logger)
	s.SetCloseTabs(closeTabs)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, file, ready) }()
	select {
	case addr := <-ready:
		return addr, cancel, done
	case err := <-done:
		cancel()
		t.Fatalf("serve failed: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not start")
	}
	return "", cancel, done
}

func waitServe(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
}

func eventsURL(t *testing.T, addr string) string {
	t.Helper()
	u, err := url.Parse(addr)
	if err != nil {
		t.Fatal(err)
	}
	return u.Scheme + "://" + u.Host + eventsPath + "?path=" + url.QueryEscape(u.Path)
}

func TestServe_ShutdownSendsCloseEvent(t *testing.T) {
	for _, reload := range []bool{true, false} {
		t.Run(map[bool]string{true: "reload", false: "no-reload"}[reload], func(t *testing.T) {
			addr, stop, done := startServer(t, reload, true)
			defer stop()

			resp, err := http.Get(eventsURL(t, addr))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			r := bufio.NewReader(resp.Body)
			if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, ": connected") {
				t.Fatalf("unexpected first line %q", line)
			}

			stop()
			var got strings.Builder
			for {
				line, err := r.ReadString('\n')
				got.WriteString(line)
				if err != nil {
					break
				}
			}
			if !strings.Contains(got.String(), "event: close\n") {
				t.Errorf("no close event in stream: %q", got.String())
			}
			waitServe(t, done)
		})
	}
}

func TestServe_ShutdownWithoutClients(t *testing.T) {
	_, stop, done := startServer(t, true, true)
	stop()
	waitServe(t, done)
}

func TestServe_CloseTabsOffNoReloadHasNoStream(t *testing.T) {
	addr, stop, done := startServer(t, false, false)
	defer func() {
		stop()
		waitServe(t, done)
	}()

	resp, err := http.Get(eventsURL(t, addr))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("event stream should not be served when reload and close-tabs are off")
	}

	page, err := http.Get(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body := new(strings.Builder)
	_, _ = bufio.NewReader(page.Body).WriteTo(body)
	if strings.Contains(body.String(), "live-reload.js") {
		t.Error("page should not load live-reload.js")
	}
}

func TestServe_CloseTabsOffEndsStreamPromptly(t *testing.T) {
	var logs bytes.Buffer
	addr, stop, done := startServerLog(t, true, false, NewLogger(&logs, slog.LevelWarn))
	defer stop()

	resp, err := http.Get(eventsURL(t, addr))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, ": connected") {
		t.Fatalf("unexpected first line %q", line)
	}

	stop()
	var got strings.Builder
	for {
		line, err := r.ReadString('\n')
		got.WriteString(line)
		if err != nil {
			break
		}
	}
	waitServe(t, done)
	// A stream that ignored shutdown would only end when the grace period
	// runs out, which logs this warning.
	if strings.Contains(logs.String(), "did not finish in time") {
		t.Errorf("event stream outlived shutdown: %s", logs.String())
	}
	if strings.Contains(got.String(), "event: close") {
		t.Errorf("close event sent with close-tabs off: %q", got.String())
	}
}

func TestReplaceNav_Template(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{true, false} {
		for _, p := range []string{"/doc.md", "/"} {
			s := NewServer("localhost", 0, false, false, false, NewParser(nil), nil)
			s.SetReplaceNav(on)
			rec := httptest.NewRecorder()
			s.newHandler(http.Dir(dir)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			body := rec.Body.String()
			for _, want := range []string{"<html data-grip-replace-nav>", `<script src="/static/js/replace-nav.js"></script>`} {
				if strings.Contains(body, want) != on {
					t.Errorf("replaceNav=%v %s: contains %q = %v", on, p, want, !on)
				}
			}
		}
	}
}

func TestServeEvents_LateConnectionGetsClose(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, closeTabs := range []bool{true, false} {
		// Live reload on, so the stream exists even with closeTabs off. The
		// hub is never subscribed to here, so it needs no event source.
		s := NewServer("localhost", 0, false, false, true, NewParser(nil), nil)
		s.hub = NewHub(nil, reloadDebounce, nil)
		s.SetCloseTabs(closeTabs)
		h := s.newHandler(http.Dir(dir))
		s.shut = true
		close(s.closing)

		rec := httptest.NewRecorder()
		aborted := func() (aborted bool) {
			defer func() {
				if r := recover(); r != nil {
					if r != http.ErrAbortHandler {
						panic(r)
					}
					aborted = true
				}
			}()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, eventsPath+"?path=/doc.md", nil))
			return false
		}()
		if closeTabs {
			if aborted || rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: close\n") {
				t.Errorf("closeTabs on: aborted=%v status %d body %q", aborted, rec.Code, rec.Body.String())
			}
		} else if !aborted {
			// An error status would make the EventSource give up for good.
			t.Errorf("closeTabs off: want the connection dropped, got status %d", rec.Code)
		}
	}
}

func TestReload_Template(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("# Doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, reload := range []bool{true, false} {
		s := NewServer("localhost", 0, false, false, reload, NewParser(nil), nil)
		if reload {
			s.hub = NewHub(nil, reloadDebounce, nil)
		}
		s.SetCloseTabs(true)
		rec := httptest.NewRecorder()
		s.newHandler(http.Dir(dir)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/doc.md", nil))
		body := rec.Body.String()
		if !strings.Contains(body, "live-reload.js") {
			t.Errorf("reload=%v: close-tabs pages must load live-reload.js", reload)
		}
		if strings.Contains(body, "<html data-grip-reload>") != reload {
			t.Errorf("reload=%v: data-grip-reload attribute mismatch", reload)
		}
	}
}
