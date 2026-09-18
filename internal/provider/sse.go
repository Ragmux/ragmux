package provider

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one parsed Server-Sent Event.
type sseEvent struct {
	Event string
	Data  string
}

// readSSE scans an SSE stream and invokes fn for every event. It returns when
// the stream ends or fn returns false.
func readSSE(r io.Reader, fn func(sseEvent) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var ev sseEvent
	var data []string
	flush := func() bool {
		if len(data) == 0 && ev.Event == "" {
			return true
		}
		ev.Data = strings.Join(data, "\n")
		cont := fn(ev)
		ev = sseEvent{}
		data = data[:0]
		return cont
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if !flush() {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
