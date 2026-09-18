// SSE broadcast hub for GET /api/triage/events, ported from web-server.ts:1377-1406.
//
// The TypeScript server kept `const triageSseClients: express.Response[]` and wrote
// `data: ${JSON.stringify(event)}\n\n` synchronously to each response inside a try/catch that
// swallowed write errors. Go handlers are concurrent, so the equivalent is a mutex-guarded client
// registry plus a per-client buffered channel: Broadcast marshals once and does a non-blocking send,
// dropping an event for a client whose buffer is full rather than blocking every other client or the
// mutation that triggered the event. There is no replay buffer, exactly as in TS.
package httpapi

import "sync"

// hubClientBuffer bounds how far a slow SSE consumer may fall behind before events are dropped for
// it. It is small on purpose: the stream is live progress, not a durable log.
const hubClientBuffer = 64

// Hub is a concurrency-safe fan-out of server-sent event frames.
type Hub struct {
	mu      sync.Mutex
	clients map[int]chan []byte
	nextID  int
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{clients: map[int]chan []byte{}}
}

// Broadcast frames v as `data: {json}\n\n` and enqueues the frame to every subscribed client. It
// never blocks: a client whose buffer is full misses the frame, mirroring the TS loop's swallowed
// write errors.
func (h *Hub) Broadcast(v any) {
	payload, err := marshalNoHTMLEscape(v)
	if err != nil {
		return
	}
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')

	h.mu.Lock()
	clients := make([]chan []byte, 0, len(h.clients))
	for _, ch := range h.clients {
		clients = append(clients, ch)
	}
	h.mu.Unlock()

	for _, ch := range clients {
		select {
		case ch <- frame:
		default:
		}
	}
}

// Subscribe registers a client and returns its frame channel plus a cancel function. The channel is
// deliberately never closed: Broadcast may hold a reference obtained just before cancel runs, and a
// send on a closed channel panics. After cancel the channel is unreferenced and collected.
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	h.mu.Lock()
	id := h.nextID
	h.nextID++
	ch := make(chan []byte, hubClientBuffer)
	h.clients[id] = ch
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		delete(h.clients, id)
		h.mu.Unlock()
	}
	return ch, cancel
}

// ClientCount reports the number of live subscribers (used by tests).
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}
