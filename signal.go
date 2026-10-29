package main

// Signaling: how members of a room find each other's network addresses.
//
// Each room code maps to a topic on ntfy.sh (a free public pub/sub relay).
// Members post small encrypted "announce" messages there (their name, public
// and LAN UDP address) and listen for everyone else's. ntfy only ever sees
// ciphertext: the key comes from the room code, which never leaves the app.
// After that, voice goes directly between the members' PCs over UDP.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Signaler interface {
	Publish(msg []byte) error
	// Subscribe calls fn for every message on the topic, including recent
	// ones from before we joined, until ctx is cancelled.
	Subscribe(ctx context.Context, fn func(msg []byte))
}

var ntfyBase = "https://ntfy.sh"

type ntfySignal struct {
	topic string
	cl    *http.Client
	logf  func(string, ...any)
}

func newNtfySignal(topic string, logf func(string, ...any)) *ntfySignal {
	return &ntfySignal{topic: topic, cl: &http.Client{Timeout: 15 * time.Second}, logf: logf}
}

func (n *ntfySignal) Publish(msg []byte) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		req, _ := http.NewRequest("POST", ntfyBase+"/"+n.topic, bytes.NewReader(msg))
		req.Header.Set("X-Title", "ringlink")
		var r *http.Response
		r, err = n.cl.Do(req)
		if err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode == 200 {
				return nil
			}
			err = fmt.Errorf("relay answered %s", r.Status)
		}
		time.Sleep(time.Duration(2+3*attempt) * time.Second)
	}
	return err
}

type ntfyEvent struct {
	ID      string `json:"id"`
	Event   string `json:"event"`
	Message string `json:"message"`
}

func (n *ntfySignal) Subscribe(ctx context.Context, fn func([]byte)) {
	since := "15m"
	stream := &http.Client{} // no timeout: long-lived stream (ntfy sends keepalives)
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, "GET", ntfyBase+"/"+n.topic+"/json?since="+since, nil)
		r, err := stream.Do(req)
		if err == nil && r.StatusCode == 200 {
			backoff = 2 * time.Second
			sc := bufio.NewScanner(r.Body)
			sc.Buffer(make([]byte, 64*1024), 64*1024)
			for sc.Scan() {
				var e ntfyEvent
				if json.Unmarshal(sc.Bytes(), &e) != nil {
					continue
				}
				if e.Event == "message" {
					since = e.ID
					fn([]byte(e.Message))
				}
			}
			r.Body.Close()
		} else if err == nil {
			r.Body.Close()
			err = fmt.Errorf("relay answered %s", r.Status)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil && n.logf != nil && !strings.Contains(err.Error(), "context canceled") {
			n.logf("lost the room relay (%v) - reconnecting", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// memSignal is an in-process relay used by tests.
type memHub struct {
	mu   sync.Mutex
	msgs map[string][][]byte
	subs map[string][]chan []byte
}

func newMemHub() *memHub {
	return &memHub{msgs: map[string][][]byte{}, subs: map[string][]chan []byte{}}
}

type memSignal struct {
	hub   *memHub
	topic string
}

func (m *memSignal) Publish(msg []byte) error {
	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
	m.hub.msgs[m.topic] = append(m.hub.msgs[m.topic], msg)
	for _, c := range m.hub.subs[m.topic] {
		select {
		case c <- msg:
		default:
		}
	}
	return nil
}

func (m *memSignal) Subscribe(ctx context.Context, fn func([]byte)) {
	c := make(chan []byte, 256)
	m.hub.mu.Lock()
	old := append([][]byte(nil), m.hub.msgs[m.topic]...)
	m.hub.subs[m.topic] = append(m.hub.subs[m.topic], c)
	m.hub.mu.Unlock()
	for _, b := range old {
		fn(b)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-c:
			fn(b)
		}
	}
}
