package wsrelay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestWebsocketPair(t *testing.T) (*session, *websocket.Conn, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var serverConn *websocket.Conn
	var serverSess *session
	ready := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade error: %v", err)
			return
		}
		serverConn = conn
		serverSess = &session{
			conn:   conn,
			closed: make(chan struct{}),
		}
		close(ready)
	}))

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	clientConn, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if errDial != nil {
		ts.Close()
		t.Fatalf("dial error: %v", errDial)
	}
	<-ready

	cleanup := func() {
		_ = clientConn.Close()
		if serverSess != nil {
			serverSess.cleanup(errClosed)
		}
		if serverConn != nil {
			_ = serverConn.Close()
		}
		ts.Close()
	}

	return serverSess, clientConn, cleanup
}

func TestPendingRequest_ConcurrentSendAndClose(t *testing.T) {
	for iter := 0; iter < 1000; iter++ {
		s := &session{
			closed: make(chan struct{}),
		}
		reqID := fmt.Sprintf("req-%d", iter)
		req := newPendingRequest(context.Background())
		s.pending.Store(reqID, req)

		var wg sync.WaitGroup
		wg.Add(3)

		// Producer
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				s.dispatch(Message{
					ID:   reqID,
					Type: MessageTypeStreamChunk,
					Payload: map[string]any{
						"seq": i,
					},
				})
			}
		}()

		// Consumer
		go func() {
			defer wg.Done()
			for range req.ch {
			}
		}()

		// Deleter / closer
		go func() {
			defer wg.Done()
			if actual, loaded := s.pending.LoadAndDelete(reqID); loaded {
				actual.(*pendingRequest).close()
			}
		}()

		wg.Wait()
	}
}

func TestSession_Cleanup_RaceSyncMap(t *testing.T) {
	s := &session{
		closed: make(chan struct{}),
	}
	var stop int32
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for atomic.LoadInt32(&stop) == 0 {
			req := newPendingRequest(context.Background())
			s.pending.Store("key", req)
			s.pending.Load("key")
			s.pending.Delete("key")
			req.cancel()
		}
	}()

	for i := 0; i < 50; i++ {
		s.cleanup(errClosed)
	}

	atomic.StoreInt32(&stop, 1)
	wg.Wait()
}

func TestSession_Request_NoGoroutineLeakOnCompletion(t *testing.T) {
	sess, clientConn, cleanup := newTestWebsocketPair(t)
	defer cleanup()

	// Consume messages on client side and immediately reply with a terminal response.
	go func() {
		for {
			var msg Message
			if err := clientConn.ReadJSON(&msg); err != nil {
				return
			}
			sess.dispatch(Message{
				ID:   msg.ID,
				Type: MessageTypeHTTPResp,
			})
		}
	}()

	const count = 30
	for i := 0; i < count; i++ {
		reqID := fmt.Sprintf("req-leak-%d", i)
		ch, errReq := sess.request(context.Background(), Message{ID: reqID, Type: MessageTypeHTTPReq})
		if errReq != nil {
			t.Fatalf("request error: %v", errReq)
		}
		// Drain channel completely until closed.
		for range ch {
		}
	}
}

func TestSession_Dispatch_NoSilentFrameLoss(t *testing.T) {
	s := &session{
		closed: make(chan struct{}),
	}
	reqID := "stream-slow-consumer"
	req := newPendingRequest(context.Background())
	s.pending.Store(reqID, req)

	// Send 10 chunks followed by terminal message
	for i := 0; i < 10; i++ {
		s.dispatch(Message{
			ID:   reqID,
			Type: MessageTypeStreamChunk,
			Payload: map[string]any{
				"chunk": i,
			},
		})
	}
	s.dispatch(Message{
		ID:   reqID,
		Type: MessageTypeStreamEnd,
	})

	var received []Message
	for msg := range req.ch {
		received = append(received, msg)
	}

	if len(received) != 11 {
		t.Fatalf("frame loss: expected 11 frames, got %d", len(received))
	}
}

func TestSession_Cleanup_DeliversErrorFrameWhenBufferFull(t *testing.T) {
	s := &session{
		closed: make(chan struct{}),
	}
	reqID := "req-err-cleanup-full"
	req := newPendingRequest(context.Background())
	s.pending.Store(reqID, req)

	// Fill channel to buffer capacity
	for i := 0; i < pendingChannelBuffer; i++ {
		s.dispatch(Message{
			ID:   reqID,
			Type: MessageTypeStreamChunk,
			Payload: map[string]any{
				"seq": i,
			},
		})
	}

	s.cleanup(errClosed)

	var msgs []Message
	for msg := range req.ch {
		msgs = append(msgs, msg)
	}

	if len(msgs) == 0 {
		t.Fatalf("expected messages on cleanup, got 0")
	}
	lastMsg := msgs[len(msgs)-1]
	if lastMsg.Type != MessageTypeError {
		t.Fatalf("expected last message to be MessageTypeError, got %s", lastMsg.Type)
	}
}

// dispatchPromptly fails the test if dispatch waits on the request's consumer.
func dispatchPromptly(t *testing.T, s *session, msg Message) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.dispatch(msg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("dispatch of %s for %s blocked on a slow consumer", msg.Type, msg.ID)
	}
}

func drainWithin(t *testing.T, ch <-chan Message) []Message {
	t.Helper()
	var msgs []Message
	timeout := time.After(2 * time.Second)
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return msgs
			}
			msgs = append(msgs, msg)
		case <-timeout:
			t.Fatalf("channel not closed after %d messages", len(msgs))
		}
	}
}

func fillPastBuffer(t *testing.T, s *session, reqID string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		dispatchPromptly(t, s, Message{ID: reqID, Type: MessageTypeStreamChunk, Payload: map[string]any{"seq": i}})
	}
}

func assertChunksInOrder(t *testing.T, msgs []Message, count int) {
	t.Helper()
	if len(msgs) < count {
		t.Fatalf("frame loss: expected at least %d chunks, got %d messages", count, len(msgs))
	}
	for i := 0; i < count; i++ {
		if msgs[i].Type != MessageTypeStreamChunk || msgs[i].Payload["seq"] != i {
			t.Fatalf("message %d: expected chunk seq %d, got %s %v", i, i, msgs[i].Type, msgs[i].Payload["seq"])
		}
	}
}

func TestSession_Dispatch_TerminalDeliveredWhenBufferFull(t *testing.T) {
	s := &session{closed: make(chan struct{})}
	reqID := "req-terminal-full"
	req := newPendingRequest(context.Background())
	s.pending.Store(reqID, req)

	count := pendingChannelBuffer + 10
	fillPastBuffer(t, s, reqID, count)
	dispatchPromptly(t, s, Message{ID: reqID, Type: MessageTypeStreamEnd})

	msgs := drainWithin(t, req.ch)
	if len(msgs) != count+1 {
		t.Fatalf("frame loss: expected %d frames, got %d", count+1, len(msgs))
	}
	assertChunksInOrder(t, msgs, count)
	if last := msgs[len(msgs)-1]; last.Type != MessageTypeStreamEnd {
		t.Fatalf("expected last message to be MessageTypeStreamEnd, got %s", last.Type)
	}
}

func TestSession_SlowConsumer_ContextCancelReleasesRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{closed: make(chan struct{})}
	reqID := "req-ctx-cancel"
	req := newPendingRequest(ctx)
	sess.pending.Store(reqID, req)

	fillPastBuffer(t, sess, reqID, pendingChannelBuffer+10)
	cancel()

	// The consumer is gone; the forwarder must stop and close the channel.
	drainWithin(t, req.ch)
	if req.deliver(sess.closed, Message{ID: reqID, Type: MessageTypeStreamChunk}) {
		t.Fatalf("deliver accepted a message after the request context ended")
	}
}

func TestSession_SlowConsumer_SessionCloseDeliversQueuedThenError(t *testing.T) {
	sess := &session{closed: make(chan struct{})}
	reqID := "req-sess-close"
	req := newPendingRequest(context.Background())
	sess.pending.Store(reqID, req)

	count := pendingChannelBuffer + 10
	fillPastBuffer(t, sess, reqID, count)

	cleanupDone := make(chan struct{})
	go func() {
		sess.cleanup(errClosed)
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatalf("session cleanup remained blocked")
	}

	msgs := drainWithin(t, req.ch)
	if len(msgs) != count+1 {
		t.Fatalf("expected %d chunks plus an error frame, got %d messages", count, len(msgs))
	}
	assertChunksInOrder(t, msgs, count)
	last := msgs[len(msgs)-1]
	if last.Type != MessageTypeError || last.Payload["error"] != errClosed.Error() {
		t.Fatalf("expected trailing error frame %q, got %s %v", errClosed.Error(), last.Type, last.Payload["error"])
	}
}

func TestSession_SlowConsumer_TerminalAfterContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &session{closed: make(chan struct{})}
	reqID := "req-terminal-ctx-cancel"
	req := newPendingRequest(ctx)
	sess.pending.Store(reqID, req)

	fillPastBuffer(t, sess, reqID, pendingChannelBuffer+10)
	cancel()
	dispatchPromptly(t, sess, Message{ID: reqID, Type: MessageTypeStreamEnd})

	if _, ok := sess.pending.Load(reqID); ok {
		t.Fatalf("terminal message for a cancelled request left it pending")
	}
	drainWithin(t, req.ch)
}

func TestSession_SlowConsumer_TerminalThenSessionClose(t *testing.T) {
	sess := &session{closed: make(chan struct{})}
	reqID := "req-terminal-sess-close"
	req := newPendingRequest(context.Background())
	sess.pending.Store(reqID, req)

	count := pendingChannelBuffer + 10
	fillPastBuffer(t, sess, reqID, count)
	dispatchPromptly(t, sess, Message{ID: reqID, Type: MessageTypeStreamEnd})
	sess.cleanup(errClosed)

	// The terminal frame was accepted before the session closed, so the
	// consumer still gets the whole stream and no extra error.
	msgs := drainWithin(t, req.ch)
	if len(msgs) != count+1 {
		t.Fatalf("expected %d frames, got %d", count+1, len(msgs))
	}
	assertChunksInOrder(t, msgs, count)
	if last := msgs[len(msgs)-1]; last.Type != MessageTypeStreamEnd {
		t.Fatalf("expected last message to be MessageTypeStreamEnd, got %s", last.Type)
	}
}

func TestSession_SlowConsumer_TerminalError_SessionClose(t *testing.T) {
	sess := &session{closed: make(chan struct{})}
	reqID := "req-terminal-err-sess-close"
	req := newPendingRequest(context.Background())
	sess.pending.Store(reqID, req)

	count := pendingChannelBuffer + 10
	fillPastBuffer(t, sess, reqID, count)
	dispatchPromptly(t, sess, Message{
		ID:      reqID,
		Type:    MessageTypeError,
		Payload: map[string]any{"error": "upstream model timeout"},
	})
	sess.cleanup(errClosed)

	msgs := drainWithin(t, req.ch)
	assertChunksInOrder(t, msgs, count)
	last := msgs[len(msgs)-1]
	if last.Type != MessageTypeError || last.Payload["error"] != "upstream model timeout" {
		t.Fatalf("expected the upstream error frame last, got %s %v", last.Type, last.Payload["error"])
	}
}

// A request whose consumer stops reading must not stall the session read loop
// for the other requests sharing the websocket.
func TestSession_StalledConsumerDoesNotStarveOtherRequests(t *testing.T) {
	sess, clientConn, cleanup := newTestWebsocketPair(t)
	runDone := make(chan struct{})
	go func() {
		sess.run(context.Background())
		close(runDone)
	}()
	defer func() {
		cleanup()
		<-runDone
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Request A: nobody ever reads its channel.
	if _, errReq := sess.request(ctx, Message{ID: "stalled", Type: MessageTypeHTTPReq}); errReq != nil {
		t.Fatalf("request A error: %v", errReq)
	}
	chB, errReq := sess.request(ctx, Message{ID: "healthy", Type: MessageTypeHTTPReq})
	if errReq != nil {
		t.Fatalf("request B error: %v", errReq)
	}

	// The relay peer streams more chunks to A than its buffer holds, then one to B.
	for i := 0; i < pendingChannelBuffer+10; i++ {
		if errWrite := clientConn.WriteJSON(Message{ID: "stalled", Type: MessageTypeStreamChunk, Payload: map[string]any{"seq": i}}); errWrite != nil {
			t.Fatalf("write chunk for A: %v", errWrite)
		}
	}
	if errWrite := clientConn.WriteJSON(Message{ID: "healthy", Type: MessageTypeStreamChunk, Payload: map[string]any{"data": "hello"}}); errWrite != nil {
		t.Fatalf("write chunk for B: %v", errWrite)
	}

	select {
	case msg, ok := <-chB:
		if !ok {
			t.Fatalf("request B channel closed before its chunk arrived")
		}
		if msg.Payload["data"] != "hello" {
			t.Fatalf("request B got unexpected message: %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("request B received nothing: the read loop is blocked on request A's full buffer")
	}
}

// A peer that keeps sending frames is alive; the session must not drop it at the
// read deadline just because no pong arrived in that window.
func TestSession_ReadDeadlineExtendsOnTraffic(t *testing.T) {
	previous := readTimeout
	readTimeout = 500 * time.Millisecond
	t.Cleanup(func() { readTimeout = previous })

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	sessCh := make(chan *session, 1)
	runDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade error: %v", err)
			return
		}
		s := newSession(conn, nil, "deadline-test")
		sessCh <- s
		go func() {
			s.run(context.Background())
			close(runDone)
		}()
	}))
	defer ts.Close()

	clientConn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	if errDial != nil {
		t.Fatalf("dial error: %v", errDial)
	}
	defer func() { _ = clientConn.Close() }()
	sess := <-sessCh
	defer func() {
		sess.cleanup(errClosed)
		<-runDone
	}()

	// Send a frame every 50ms for three read-timeout windows. The client never
	// reads, so it never answers pings with pongs.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	end := time.After(3 * readTimeout)
	for sending := true; sending; {
		select {
		case <-ticker.C:
			if errWrite := clientConn.WriteJSON(Message{ID: "unknown", Type: MessageTypeStreamChunk}); errWrite != nil {
				t.Fatalf("write frame: %v", errWrite)
			}
		case <-end:
			sending = false
		}
	}

	select {
	case <-sess.closed:
		t.Fatalf("session closed although the peer sent a frame every 50ms")
	default:
	}
}
