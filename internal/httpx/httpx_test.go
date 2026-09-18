package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeadlineCutsSlowBodies(t *testing.T) {
	srv := httptest.NewServer(ReadDeadline(200 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			t.Error("read of a stalled body should fail")
		}
		w.WriteHeader(http.StatusRequestTimeout)
	})))
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("partial"))
		time.Sleep(time.Second)
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, pr)
	req.ContentLength = 100
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestTimeout {
			t.Errorf("status %d", resp.StatusCode)
		}
	}
}

func TestDeadlineClearsAndNoStore(t *testing.T) {
	srv := httptest.NewServer(NoStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Deadline(w, 50*time.Millisecond)
		Deadline(w, 0) // cleared: the slow body below must still be read
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != "hello" {
			t.Errorf("body %q err %v", b, err)
		}
	})))
	defer srv.Close()
	pr, pw := io.Pipe()
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(pw, "hello")
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, pr)
	req.ContentLength = 5
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("status %d cache-control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	// Recorders do not expose the connection; Deadline must not panic.
	Deadline(httptest.NewRecorder(), time.Second)
	if !strings.HasPrefix(srv.URL, "http://") {
		t.Fatal(srv.URL)
	}
}
