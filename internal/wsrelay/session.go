package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// readTimeout is a variable so tests can shorten it.
var readTimeout = 60 * time.Second

const (
	writeTimeout         = 10 * time.Second
	maxInboundMessageLen = 64 << 20 // 64 MiB
	heartbeatInterval    = 30 * time.Second
	pendingChannelBuffer = 64
)

var errClosed = errors.New("websocket session closed")

// pendingRequest queues relay messages for one request. The session read loop
// only appends to the queue, so it never waits on a slow consumer; a per-request
// goroutine (forward) moves queued messages to ch in order and closes ch once the
// request is finished and drained, or once the request context ends. Consumers
// must read ch until it closes or cancel the request context.
type pendingRequest struct {
	mu         sync.Mutex
	queue      []Message
	wake       chan struct{}
	ch         chan Message
	closed     bool
	terminal   bool
	reqCtx     context.Context
	stopCancel func() bool
}

func newPendingRequest(ctx context.Context) *pendingRequest {
	pr := &pendingRequest{
		wake:   make(chan struct{}, 1),
		ch:     make(chan Message, pendingChannelBuffer),
		reqCtx: ctx,
	}
	go pr.forward()
	return pr
}

func (pr *pendingRequest) forward() {
	var ctxDone <-chan struct{}
	if pr.reqCtx != nil {
		ctxDone = pr.reqCtx.Done()
	}
	defer func() {
		pr.mu.Lock()
		pr.queue = nil
		pr.mu.Unlock()
		close(pr.ch)
	}()
	for {
		pr.mu.Lock()
		if len(pr.queue) == 0 {
			finished := pr.closed
			pr.mu.Unlock()
			if finished {
				return
			}
			select {
			case <-pr.wake:
				continue
			case <-ctxDone:
				return
			}
		}
		msg := pr.queue[0]
		pr.queue[0] = Message{}
		pr.queue = pr.queue[1:]
		pr.mu.Unlock()
		select {
		case pr.ch <- msg:
		case <-ctxDone:
			return
		}
	}
}

// signalLocked wakes the forwarder; pr.mu must be held.
func (pr *pendingRequest) signalLocked() {
	select {
	case pr.wake <- struct{}{}:
	default:
	}
}

func (pr *pendingRequest) enqueue(sessClosed <-chan struct{}, msg Message, terminal bool) bool {
	if pr == nil {
		return false
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.closed || pr.terminal {
		return false
	}
	select {
	case <-sessClosed:
		return false
	default:
	}
	if pr.reqCtx != nil && pr.reqCtx.Err() != nil {
		return false
	}
	pr.queue = append(pr.queue, msg)
	pr.terminal = terminal
	pr.signalLocked()
	return true
}

func (pr *pendingRequest) deliver(sessClosed <-chan struct{}, msg Message) bool {
	return pr.enqueue(sessClosed, msg, false)
}

func (pr *pendingRequest) deliverTerminal(sessClosed <-chan struct{}, msg Message) bool {
	return pr.enqueue(sessClosed, msg, true)
}

func (pr *pendingRequest) setStopCancel(stop func() bool) {
	if pr == nil {
		return
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.stopCancel = stop
}

// finishLocked marks the request as receiving no more messages; the forwarder
// drains what is queued and then closes ch. pr.mu must be held.
func (pr *pendingRequest) finishLocked() bool {
	if pr.closed {
		return false
	}
	pr.closed = true
	if pr.stopCancel != nil {
		pr.stopCancel()
	}
	pr.signalLocked()
	return true
}

func (pr *pendingRequest) cancel() {
	pr.close()
}

func (pr *pendingRequest) cancelWithError(cause error) {
	if pr == nil {
		return
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.closed {
		return
	}
	if !pr.terminal && cause != nil {
		pr.queue = append(pr.queue, Message{Type: MessageTypeError, Payload: map[string]any{"error": cause.Error()}})
	}
	pr.finishLocked()
}

func (pr *pendingRequest) close() {
	if pr == nil {
		return
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.finishLocked()
}

type session struct {
	conn       *websocket.Conn
	manager    *Manager
	provider   string
	id         string
	closed     chan struct{}
	closeOnce  sync.Once
	writeMutex sync.Mutex
	pending    sync.Map // map[string]*pendingRequest
}

func newSession(conn *websocket.Conn, mgr *Manager, id string) *session {
	s := &session{
		conn:     conn,
		manager:  mgr,
		provider: "",
		id:       id,
		closed:   make(chan struct{}),
	}
	conn.SetReadLimit(maxInboundMessageLen)
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})
	s.startHeartbeat()
	return s
}

func (s *session) startHeartbeat() {
	if s == nil || s.conn == nil {
		return
	}
	ticker := time.NewTicker(heartbeatInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-ticker.C:
				s.writeMutex.Lock()
				err := s.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(writeTimeout))
				s.writeMutex.Unlock()
				if err != nil {
					s.cleanup(err)
					return
				}
			}
		}
	}()
}

func (s *session) run(ctx context.Context) {
	defer s.cleanup(errClosed)
	for {
		var msg Message
		if err := s.conn.ReadJSON(&msg); err != nil {
			s.cleanup(err)
			return
		}
		// Any frame from the peer proves the connection is alive.
		_ = s.conn.SetReadDeadline(time.Now().Add(readTimeout))
		s.dispatch(msg)
	}
}

func (s *session) dispatch(msg Message) {
	if msg.Type == MessageTypePing {
		_ = s.send(context.Background(), Message{ID: msg.ID, Type: MessageTypePong})
		return
	}
	if value, ok := s.pending.Load(msg.ID); ok {
		req := value.(*pendingRequest)
		isTerminal := msg.Type == MessageTypeHTTPResp || msg.Type == MessageTypeError || msg.Type == MessageTypeStreamEnd
		if isTerminal {
			delivered := req.deliverTerminal(s.closed, msg)
			if actual, loaded := s.pending.LoadAndDelete(msg.ID); loaded {
				actualReq := actual.(*pendingRequest)
				if delivered {
					actualReq.close()
				} else {
					var cause error
					select {
					case <-s.closed:
						cause = errClosed
					default:
						if actualReq.reqCtx != nil && actualReq.reqCtx.Err() != nil {
							cause = actualReq.reqCtx.Err()
						} else if msg.Type == MessageTypeError {
							if errMsg, ok := msg.Payload["error"].(string); ok && errMsg != "" {
								cause = errors.New(errMsg)
							} else {
								cause = errors.New("wsrelay: upstream error")
							}
						}
					}
					if cause != nil {
						actualReq.cancelWithError(cause)
					} else {
						actualReq.cancel()
					}
				}
			}
		} else {
			req.deliver(s.closed, msg)
		}
		return
	}
	if msg.Type == MessageTypeHTTPResp || msg.Type == MessageTypeError || msg.Type == MessageTypeStreamEnd {
		if s.manager != nil {
			s.manager.logDebugf("wsrelay: received terminal message for unknown id %s (provider=%s)", msg.ID, s.provider)
		}
	}
}

func (s *session) send(ctx context.Context, msg Message) error {
	select {
	case <-s.closed:
		return errClosed
	default:
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	if err := s.conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

func (s *session) request(ctx context.Context, msg Message) (<-chan Message, error) {
	if msg.ID == "" {
		return nil, fmt.Errorf("wsrelay: message id is required")
	}
	req := newPendingRequest(ctx)
	if _, loaded := s.pending.LoadOrStore(msg.ID, req); loaded {
		req.close()
		return nil, fmt.Errorf("wsrelay: duplicate message id %s", msg.ID)
	}
	if ctx != nil {
		stop := context.AfterFunc(ctx, func() {
			if actual, loaded := s.pending.LoadAndDelete(msg.ID); loaded {
				actual.(*pendingRequest).cancel()
			}
		})
		req.setStopCancel(stop)
	}
	if err := s.send(ctx, msg); err != nil {
		if actual, loaded := s.pending.LoadAndDelete(msg.ID); loaded {
			req := actual.(*pendingRequest)
			req.close()
		}
		return nil, err
	}
	return req.ch, nil
}

func (s *session) cleanup(cause error) {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.pending.Range(func(key, value any) bool {
			if actual, loaded := s.pending.LoadAndDelete(key); loaded {
				req := actual.(*pendingRequest)
				req.cancelWithError(cause)
			}
			return true
		})
		if s.conn != nil {
			_ = s.conn.Close()
		}
		if s.manager != nil {
			s.manager.handleSessionClosed(s, cause)
		}
	})
}
