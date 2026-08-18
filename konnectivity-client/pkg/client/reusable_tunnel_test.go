/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/grpc"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client/metrics"
	metricstest "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics/testing"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

// fakeProxyClient implements client.ProxyServiceClient for tests. By default,
// each Proxy() call allocates a fresh in-memory pipe and starts a proxyServer
// on the server side, returning the client side as the stream. This lets a
// reusableGrpcTunnel exercise the real per-dial stream lifecycle without
// touching any network or real gRPC ClientConn.
//
// The hooks let tests override behavior:
//   - onProxy preempts stream creation entirely (e.g. to return an error or
//     to block on the request context).
//   - customizeNext mutates the next-allocated proxyServer (e.g. to install a
//     custom DIAL_REQ handler).
//   - customStream lets a test return its own ProxyService_ProxyClient
//     instead of the default pipe-backed stream.
type fakeProxyClient struct {
	mu            sync.Mutex
	proxyCalls    int
	servers       []*proxyServer
	customizeNext func(srv *proxyServer)
	onProxy       func(ctx context.Context, callNo int) (preempt bool, err error)
	customStream  func(ctx context.Context, callNo int) (client.ProxyService_ProxyClient, error)
}

// Proxy implements client.ProxyServiceClient.
func (f *fakeProxyClient) Proxy(ctx context.Context, opts ...grpc.CallOption) (client.ProxyService_ProxyClient, error) {
	f.mu.Lock()
	f.proxyCalls++
	callNo := f.proxyCalls
	hook := f.onProxy
	custom := f.customizeNext
	customStream := f.customStream
	f.customizeNext = nil
	f.mu.Unlock()

	if hook != nil {
		preempt, err := hook(ctx, callNo)
		if preempt {
			return nil, err
		}
	}

	if customStream != nil {
		return customStream(ctx, callNo)
	}

	clientSide, serverSide := pipeWithContext(ctx)
	srv := testServer(serverSide, int64(callNo))
	if custom != nil {
		custom(srv)
	}

	f.mu.Lock()
	f.servers = append(f.servers, srv)
	f.mu.Unlock()

	go srv.serve()
	return clientSide, nil
}

func (f *fakeProxyClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.proxyCalls
}

// closeTrackingConn implements clientConn for tests. Close is idempotent:
// it counts every call and signals closeCh on the first one.
type closeTrackingConn struct {
	closeCh chan struct{}
	calls   atomic.Int32
	err     error
}

func newCloseTrackingConn() *closeTrackingConn {
	return &closeTrackingConn{closeCh: make(chan struct{})}
}

func (c *closeTrackingConn) Close() error {
	if c.calls.Add(1) == 1 {
		close(c.closeCh)
	}
	return c.err
}

// releaseRecvStream is a fake ProxyService_ProxyClient whose Recv() blocks on
// a test-owned channel rather than on its stream context. This is what makes
// the late-establishment test (TestReusableTunnel_LateEstablishmentWaited)
// actually pin the contract: with a normal pipe-backed stream, cancelling
// the per-dial streamCtx would unblock Recv() immediately and the test would
// pass even if the implementation incorrectly fired children.Done() early.
type releaseRecvStream struct {
	grpc.ClientStream
	release <-chan struct{}
}

func (s *releaseRecvStream) Send(*client.Packet) error { return nil }
func (s *releaseRecvStream) CloseSend() error          { return nil }
func (s *releaseRecvStream) Context() context.Context  { return context.Background() }
func (s *releaseRecvStream) Recv() (*client.Packet, error) {
	<-s.release
	return nil, errors.New("released")
}

// sendFailingStream is a fake ProxyService_ProxyClient whose Send() always
// errors. It models the inner.DialContext failure path where DIAL_REQ Send
// fails AFTER the Proxy stream has been successfully established:
// specifically the path the existing grpcTunnel.dialContext does NOT close
// itself, motivating the streamCancel() in reusableGrpcTunnel.DialContext on
// inner.DialContext error.
//
// Recv() honors ctx (mirrors real gRPC stream behavior) so that when
// reusableGrpcTunnel.DialContext calls streamCancel() on the Send error,
// the serve goroutine actually exits.
type sendFailingStream struct {
	grpc.ClientStream
	ctx context.Context
}

func (s *sendFailingStream) Send(*client.Packet) error { return errors.New("send failed") }
func (s *sendFailingStream) CloseSend() error          { return nil }
func (s *sendFailingStream) Context() context.Context  { return s.ctx }
func (s *sendFailingStream) Recv() (*client.Packet, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

// --- Tests ---

// 1. Single dial via a reusable tunnel succeeds end-to-end.
func TestReusableTunnel_SingleDial(t *testing.T) {
	expectCleanShutdown(t)

	fc := newCloseTrackingConn()
	fp := &fakeProxyClient{}
	tun := newReusableGrpcTunnel(fc, fp)

	conn, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
	conn.Close()

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
	if got := fc.calls.Load(); got != 1 {
		t.Errorf("clientConn.Close called %d times, want 1", got)
	}
	if got := fp.callCount(); got != 1 {
		t.Errorf("Proxy() called %d times, want 1", got)
	}
}

// 2. Many sequential dials reuse the shared *grpc.ClientConn: the fake conn
// is closed exactly once (on Close), but Proxy() is invoked N times.
func TestReusableTunnel_ManySequentialDials_OneClientConn(t *testing.T) {
	expectCleanShutdown(t)

	const N = 8
	fc := newCloseTrackingConn()
	fp := &fakeProxyClient{}
	tun := newReusableGrpcTunnel(fc, fp)

	for i := 0; i < N; i++ {
		c, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		c.Close()
	}

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
	if got := fp.callCount(); got != N {
		t.Errorf("Proxy() called %d times, want %d", got, N)
	}
	if got := fc.calls.Load(); got != 1 {
		t.Errorf("clientConn.Close called %d times, want 1", got)
	}
}

// 3. Concurrent dials succeed and are isolated: closing one returned conn
// does not affect siblings.
func TestReusableTunnel_ConcurrentDials_Isolated(t *testing.T) {
	expectCleanShutdown(t)

	const N = 16
	fc := newCloseTrackingConn()
	fp := &fakeProxyClient{}
	tun := newReusableGrpcTunnel(fc, fp)

	var wg sync.WaitGroup
	conns := make([]net.Conn, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			conns[i], errs[i] = tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("dial %d: %v", i, err)
		}
	}

	conns[0].Close()
	for i := 1; i < N; i++ {
		_, err := conns[i].Write([]byte("ping"))
		if err != nil {
			t.Errorf("write to sibling %d after closing 0: %v", i, err)
		}
	}
	for _, c := range conns[1:] {
		c.Close()
	}

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
	if got := fp.callCount(); got != N {
		t.Errorf("Proxy() called %d times, want %d", got, N)
	}
}

// 4. Per-dial cancellation isolation: block the first dial's DIAL_RSP, cancel
// its requestCtx, assert DialFailureContext, then prove sibling and fresh
// dials still succeed and the shared tunnel remains usable.
func TestReusableTunnel_PerDialCancellationIsolation(t *testing.T) {
	expectCleanShutdown(t)

	gotDialReq := make(chan struct{}, 1)
	fp := &fakeProxyClient{}
	fp.mu.Lock()
	fp.customizeNext = func(srv *proxyServer) {
		// First dial: receive DIAL_REQ but never reply.
		srv.handlers[client.PacketType_DIAL_REQ] = func(_ *client.Packet) *client.Packet {
			select {
			case gotDialReq <- struct{}{}:
			default:
			}
			return nil
		}
	}
	fp.mu.Unlock()

	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	// First dial: cancel its requestCtx after the server has received DIAL_REQ.
	reqCtx, cancel := context.WithCancel(context.Background())
	dial1Done := make(chan error, 1)
	go func() {
		_, err := tun.DialContext(reqCtx, "tcp", "127.0.0.1:80")
		dial1Done <- err
	}()
	select {
	case <-gotDialReq:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received DIAL_REQ")
	}
	cancel()
	err := <-dial1Done
	if err == nil {
		t.Fatal("expected error from cancelled DialContext")
	}
	isDF, reason := GetDialFailureReason(err)
	if !isDF || reason != metrics.DialFailureContext {
		t.Errorf("got isDialFailure=%v reason=%v, want DialFailureContext", isDF, reason)
	}

	// Sibling and fresh dials must still succeed on the same shared transport.
	c2, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("sibling dial: %v", err)
	}
	c3, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("fresh dial: %v", err)
	}
	c2.Close()
	c3.Close()

	metrics.Metrics.Reset()
	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
}

// 5. requestCtx is honored during stream establishment: when Proxy() blocks
// on ctx.Done(), DialContext returns within the requestCtx deadline with a
// typed DialFailureContext, and the metric is observed.
func TestReusableTunnel_RequestCtxHonoredDuringEstablishment(t *testing.T) {
	expectCleanShutdown(t)

	fp := &fakeProxyClient{
		onProxy: func(ctx context.Context, _ int) (bool, error) {
			<-ctx.Done()
			return true, ctx.Err()
		},
	}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	reqCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := tun.DialContext(reqCtx, "tcp", "127.0.0.1:80")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("DialContext took %v; expected to honor 100ms request timeout", elapsed)
	}
	isDF, reason := GetDialFailureReason(err)
	if !isDF || reason != metrics.DialFailureContext {
		t.Errorf("got isDialFailure=%v reason=%v, want DialFailureContext", isDF, reason)
	}
	if err := metricstest.ExpectClientDialFailure(metrics.DialFailureContext, 1); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset()

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
}

// 5b. Late-arriving inner tunnel after requestCtx fires must be tracked to
// Done(). Pins the exact bug: if the implementation incorrectly called
// children.Done() immediately when it saw a late non-nil inner tunnel,
// Close() would return before we release Recv().
//
// Sequence (deterministic, no time-based racing):
//  1. Test cancels requestCtx; DialContext takes the requestCtx branch.
//  2. requestCtx branch calls streamCancel(); onProxy was blocked on
//     streamCtx.Done(), so it now returns and Proxy() yields a
//     releaseRecvStream whose Recv() blocks on a test-owned channel
//     (independent of streamCtx). This is the "late establishment"
//     scenario by construction.
//  3. Test starts tun.Close() concurrently; asserts it does NOT return
//     and Done() does NOT fire before streamRelease.
//  4. Test closes streamRelease; Recv() returns; serve() exits;
//     children.Done() fires; Close() returns; Done() fires.
func TestReusableTunnel_LateEstablishmentWaited(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	metrics.Metrics.Reset()
	defer metrics.Metrics.Reset()

	streamRelease := make(chan struct{})

	// onProxy blocks until streamCtx fires (i.e., until DialContext takes the
	// requestCtx branch and calls streamCancel). Then customStream runs and
	// returns a releaseRecvStream, so the inner tunnel comes into existence
	// AFTER streamCtx is already cancelled. This is the "late establishment"
	// scenario by construction; no time-based racing.
	fp := &fakeProxyClient{
		onProxy: func(ctx context.Context, _ int) (bool, error) {
			<-ctx.Done()
			return false, nil
		},
		customStream: func(_ context.Context, _ int) (client.ProxyService_ProxyClient, error) {
			return &releaseRecvStream{release: streamRelease}, nil
		},
	}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	// Start the dial in a goroutine; cancel its requestCtx. cancel() makes
	// requestCtx.Done() fire; reusableGrpcTunnel.DialContext takes the
	// requestCtx branch and calls streamCancel; that unblocks onProxy; then
	// customStream returns the late stream; the goroutine sends to resCh; the
	// requestCtx branch sees a non-nil res.t and tracks it via res.t.Done().
	reqCtx, cancel := context.WithCancel(context.Background())
	dialDone := make(chan error, 1)
	go func() {
		_, err := tun.DialContext(reqCtx, "tcp", "127.0.0.1:80")
		dialDone <- err
	}()
	cancel()

	err := <-dialDone
	if err == nil {
		t.Fatal("expected error from cancelled DialContext")
	}
	isDF, reason := GetDialFailureReason(err)
	if !isDF || reason != metrics.DialFailureContext {
		t.Errorf("got isDialFailure=%v reason=%v, want DialFailureContext", isDF, reason)
	}

	// At this point the inner tunnel is "late": Proxy() returned a stream
	// after requestCtx fired and after streamCtx was cancelled. The inner
	// serve goroutine is running but blocked in Recv() on streamRelease,
	// independent of streamCtx.

	// Start Close() in a goroutine; it must wait for the late child.
	closeDone := make(chan error, 1)
	go func() { closeDone <- tun.Close() }()

	// Verify Close() and Done() are still pending. This proves the implementation
	// is waiting on the late inner tunnel.
	select {
	case <-closeDone:
		t.Fatal("Close() returned before late child was released")
	case <-tun.Done():
		t.Fatal("Done() fired before late child was released")
	case <-time.After(200 * time.Millisecond):
	}

	// Release the late child. Close() must now finish; Done() must fire.
	close(streamRelease)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return after late child release")
	}
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not fire after Close")
	}
}

// 5c. Unsupported protocol short-circuits: returns "protocol not supported"
// without calling Proxy(), and observes DialFailureUnknown.
func TestReusableTunnel_UnsupportedProtocolShortCircuits(t *testing.T) {
	expectCleanShutdown(t)

	fp := &fakeProxyClient{}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	_, err := tun.DialContext(context.Background(), "udp", "127.0.0.1:80")
	if err == nil || err.Error() != "protocol not supported" {
		t.Fatalf("expected protocol not supported, got %v", err)
	}
	if got := fp.callCount(); got != 0 {
		t.Errorf("Proxy() called %d times for unsupported protocol, want 0", got)
	}
	if err := metricstest.ExpectClientDialFailure(metrics.DialFailureUnknown, 1); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset()

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
}

// 5d. inner.DialContext failure path: DIAL_REQ Send fails AFTER stream is
// established. The existing grpcTunnel.dialContext path does NOT close the
// tunnel itself in this case, so reusableGrpcTunnel.DialContext must
// streamCancel() to guarantee the per-dial serve goroutine exits; otherwise
// Close() would hang on children.Wait().
func TestReusableTunnel_InnerDialContextSendFailureCleansUp(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	metrics.Metrics.Reset()
	defer metrics.Metrics.Reset()

	fp := &fakeProxyClient{
		customStream: func(ctx context.Context, _ int) (client.ProxyService_ProxyClient, error) {
			return &sendFailingStream{ctx: ctx}, nil
		},
	}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	_, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected error from DIAL_REQ Send failure")
	}

	// Without streamCancel() on inner.DialContext error, the per-dial serve
	// goroutine would still be blocked in Recv() on the stream context,
	// and Close() would deadlock on children.Wait(). Asserting Close()
	// returns within a bounded time proves streamCancel() ran.
	closeDone := make(chan error, 1)
	go func() { closeDone <- tun.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() hung; inner.DialContext error path did not clean up the per-dial stream")
	}
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not fire after Close")
	}
}

// 5e. After Close, DialContext returns typed DialFailureTunnelClosed.
func TestReusableTunnel_DialAfterCloseTyped(t *testing.T) {
	expectCleanShutdown(t)

	fp := &fakeProxyClient{}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}

	_, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected error after Close")
	}
	isDF, reason := GetDialFailureReason(err)
	if !isDF || reason != metrics.DialFailureTunnelClosed {
		t.Errorf("got isDialFailure=%v reason=%v, want DialFailureTunnelClosed", isDF, reason)
	}
	if err := metricstest.ExpectClientDialFailure(metrics.DialFailureTunnelClosed, 1); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset()
}

// Close error-return contract: the first caller observes any error from the
// underlying clientConn.Close(); subsequent (and concurrent) callers return nil.
func TestReusableTunnel_CloseErrorReturnedToFirstCallerOnly(t *testing.T) {
	t.Run("Single", func(t *testing.T) {
		expectCleanShutdown(t)
		wantErr := errors.New("conn close failed")
		fc := &closeTrackingConn{closeCh: make(chan struct{}), err: wantErr}
		tun := newReusableGrpcTunnel(fc, &fakeProxyClient{})
		if err := tun.Close(); !errors.Is(err, wantErr) {
			t.Errorf("first Close: got %v, want %v", err, wantErr)
		}
		if err := tun.Close(); err != nil {
			t.Errorf("second Close: got %v, want nil", err)
		}
	})

	t.Run("Concurrent", func(t *testing.T) {
		expectCleanShutdown(t)
		wantErr := errors.New("conn close failed")
		fc := &closeTrackingConn{closeCh: make(chan struct{}), err: wantErr}
		tun := newReusableGrpcTunnel(fc, &fakeProxyClient{})

		const M = 8
		results := make(chan error, M)
		for i := 0; i < M; i++ {
			go func() { results <- tun.Close() }()
		}
		var wantErrCount, nilCount int
		for i := 0; i < M; i++ {
			err := <-results
			switch {
			case errors.Is(err, wantErr):
				wantErrCount++
			case err == nil:
				nilCount++
			default:
				t.Errorf("unexpected Close error: %v", err)
			}
		}
		if wantErrCount != 1 {
			t.Errorf("wantErr observed %d times, want exactly 1", wantErrCount)
		}
		if nilCount != M-1 {
			t.Errorf("nil observed %d times, want %d", nilCount, M-1)
		}
		if got := fc.calls.Load(); got != 1 {
			t.Errorf("clientConn.Close called %d times, want 1", got)
		}
	})
}

// 7. Concurrent Close calls all wait for in-flight child drain. Holds a
// child stream's serve blocked, starts M Close() calls, asserts none return
// and Done() does not fire until release; after release, all return and
// clientConn.Close is called once.
func TestReusableTunnel_ConcurrentClose_WaitsForDrain(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	metrics.Metrics.Reset()
	defer metrics.Metrics.Reset()

	streamRelease := make(chan struct{})
	dialResp := make(chan struct{})

	// Custom stream: serves DIAL_RSP once, then blocks Recv() on streamRelease
	// so the per-dial serve goroutine stays alive past dial completion.
	fp := &fakeProxyClient{
		customStream: func(_ context.Context, callNo int) (client.ProxyService_ProxyClient, error) {
			recvCh := make(chan *client.Packet, 1)
			var sendOnce sync.Once
			s := &controllableStream{
				recv: func() (*client.Packet, error) {
					select {
					case pkt := <-recvCh:
						return pkt, nil
					case <-streamRelease:
						return nil, errors.New("released")
					}
				},
				send: func(pkt *client.Packet) error {
					if pkt.Type == client.PacketType_DIAL_REQ {
						sendOnce.Do(func() {
							recvCh <- &client.Packet{
								Type: client.PacketType_DIAL_RSP,
								Payload: &client.Packet_DialResponse{
									DialResponse: &client.DialResponse{
										Random:    pkt.GetDialRequest().Random,
										ConnectID: int64(callNo),
									},
								},
							}
							close(dialResp)
						})
					}
					return nil
				},
			}
			return s, nil
		},
	}
	fc := newCloseTrackingConn()
	tun := newReusableGrpcTunnel(fc, fp)

	conn, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	<-dialResp
	// Note: do NOT close conn; the child must stay alive (serve blocked on
	// streamRelease) so Close() has real work to wait for.
	_ = conn

	const M = 8
	closeReturns := make(chan error, M)
	for i := 0; i < M; i++ {
		go func() { closeReturns <- tun.Close() }()
	}

	// Assert Close() and Done() are pending while the child is held.
	select {
	case <-closeReturns:
		t.Fatal("Close() returned before child drained")
	case <-tun.Done():
		t.Fatal("Done() fired before child drained")
	case <-time.After(200 * time.Millisecond):
	}

	// Release the child. All Close() callers must now return; Done must fire.
	close(streamRelease)
	for i := 0; i < M; i++ {
		select {
		case err := <-closeReturns:
			if err != nil {
				t.Errorf("Close[%d]: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close() did not return after child release")
		}
	}
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not fire after Close")
	}
	if got := fc.calls.Load(); got != 1 {
		t.Errorf("clientConn.Close called %d times under concurrent Close, want 1", got)
	}
}

// 8. Close-vs-DialContext stress: high-concurrency dialers and one Close
// mid-flight. Asserts no panic / deadlock / leak; post-Close dials return
// typed DialFailureTunnelClosed; mid-flight dials may return any of
// {success, DialFailureTunnelClosed, context/stream errors} depending on
// timing. We don't overconstrain.
func TestReusableTunnel_CloseRace(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	metrics.Metrics.Reset()
	defer metrics.Metrics.Reset()

	const N = 64
	fp := &fakeProxyClient{}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			c, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
			if err == nil && c != nil {
				c.Close()
			}
		}()
	}
	// Close partway through.
	time.Sleep(2 * time.Millisecond)
	if err := tun.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	wg.Wait()

	// Post-Close dial must return typed DialFailureTunnelClosed.
	_, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected error after Close")
	}
	isDF, reason := GetDialFailureReason(err)
	if !isDF || reason != metrics.DialFailureTunnelClosed {
		t.Errorf("post-Close: got isDialFailure=%v reason=%v, want DialFailureTunnelClosed", isDF, reason)
	}
}

// 9. Done does NOT fire on remote disappearance alone; only Close fires it.
// Simulates the remote stream going away from the client's perspective by
// making Recv() return an error after a test-owned signal. The reusable
// tunnel's Done must remain pending; only an explicit tun.Close() may close it.
func TestReusableTunnel_DoneRequiresClose(t *testing.T) {
	expectCleanShutdown(t)

	remoteGone := make(chan struct{})
	dialResp := make(chan struct{})
	fp := &fakeProxyClient{
		customStream: func(_ context.Context, callNo int) (client.ProxyService_ProxyClient, error) {
			recvCh := make(chan *client.Packet, 1)
			var sendOnce sync.Once
			return &controllableStream{
				recv: func() (*client.Packet, error) {
					select {
					case pkt := <-recvCh:
						return pkt, nil
					case <-remoteGone:
						return nil, errors.New("remote stream closed (simulated)")
					}
				},
				send: func(pkt *client.Packet) error {
					if pkt.Type == client.PacketType_DIAL_REQ {
						sendOnce.Do(func() {
							recvCh <- &client.Packet{
								Type: client.PacketType_DIAL_RSP,
								Payload: &client.Packet_DialResponse{
									DialResponse: &client.DialResponse{
										Random:    pkt.GetDialRequest().Random,
										ConnectID: int64(callNo),
									},
								},
							}
							close(dialResp)
						})
					}
					return nil
				},
			}, nil
		},
	}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	conn, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	<-dialResp
	// Do NOT close conn; we want the per-dial stream alive so the simulated
	// remote-gone error reaches its serve goroutine.
	_ = conn

	// Simulate remote disappearance. The per-dial stream's serve goroutine
	// will observe the Recv error and exit. The reusable tunnel itself must
	// remain unclosed: it does not implement remote-failure detection,
	// callers do.
	close(remoteGone)

	select {
	case <-tun.Done():
		t.Fatal("Done fired without tun.Close() being called (remote disappearance alone)")
	case <-time.After(200 * time.Millisecond):
	}

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not fire after Close")
	}
}

// 12. Stream establishment failure is typed DialFailureStreamSetup and observed.
func TestReusableTunnel_StreamSetupFailureTyped(t *testing.T) {
	expectCleanShutdown(t)

	wantErr := errors.New("stream setup boom")
	fp := &fakeProxyClient{
		onProxy: func(_ context.Context, _ int) (bool, error) {
			return true, wantErr
		},
	}
	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	_, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected error")
	}
	isDF, reason := GetDialFailureReason(err)
	if !isDF || reason != metrics.DialFailureStreamSetup {
		t.Errorf("got isDialFailure=%v reason=%v, want DialFailureStreamSetup", isDF, reason)
	}
	if err := metricstest.ExpectClientDialFailure(metrics.DialFailureStreamSetup, 1); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset()

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
}

// 13. Backend dial failures are typed DialFailureEndpoint, NOT
// DialFailureStreamSetup. Pins the contract the kube-apiserver connector
// depends on for selective invalidation.
func TestReusableTunnel_BackendErrorIsNotStreamSetup(t *testing.T) {
	expectCleanShutdown(t)

	fp := &fakeProxyClient{}
	fp.mu.Lock()
	fp.customizeNext = func(srv *proxyServer) {
		srv.handlers[client.PacketType_DIAL_REQ] = func(pkt *client.Packet) *client.Packet {
			return &client.Packet{
				Type: client.PacketType_DIAL_RSP,
				Payload: &client.Packet_DialResponse{
					DialResponse: &client.DialResponse{
						Random: pkt.GetDialRequest().Random,
						Error:  "fake backend error",
					},
				},
			}
		}
	}
	fp.mu.Unlock()

	tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)

	_, err := tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected error")
	}
	isDF, reason := GetDialFailureReason(err)
	if !isDF {
		t.Fatalf("expected dial failure, got %v", err)
	}
	if reason == metrics.DialFailureStreamSetup {
		t.Errorf("backend error must not be classified as DialFailureStreamSetup")
	}
	if reason != metrics.DialFailureEndpoint {
		t.Errorf("got reason=%v, want DialFailureEndpoint", reason)
	}
	if err := metricstest.ExpectClientDialFailure(metrics.DialFailureEndpoint, 1); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset()

	if err := tun.Close(); err != nil {
		t.Fatalf("tunnel Close: %v", err)
	}
}

// 14. Goroutine leak check across the failure paths.
func TestReusableTunnel_NoGoroutineLeaks(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	metrics.Metrics.Reset()
	defer metrics.Metrics.Reset()

	// Stream-setup failure path.
	{
		fp := &fakeProxyClient{
			onProxy: func(_ context.Context, _ int) (bool, error) {
				return true, errors.New("setup failed")
			},
		}
		tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)
		_, _ = tun.DialContext(context.Background(), "tcp", "127.0.0.1:80")
		_ = tun.Close()
	}

	// Unsupported protocol short-circuit.
	{
		tun := newReusableGrpcTunnel(newCloseTrackingConn(), &fakeProxyClient{})
		_, _ = tun.DialContext(context.Background(), "udp", "127.0.0.1:80")
		_ = tun.Close()
	}

	// Request-context cancellation during establishment.
	{
		fp := &fakeProxyClient{
			onProxy: func(ctx context.Context, _ int) (bool, error) {
				<-ctx.Done()
				return true, ctx.Err()
			},
		}
		tun := newReusableGrpcTunnel(newCloseTrackingConn(), fp)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, _ = tun.DialContext(ctx, "tcp", "127.0.0.1:80")
		cancel()
		_ = tun.Close()
	}
}

// CreateGRPCTunnel against an unreachable address does not leak.
func TestCreateGRPCTunnel_NoLeakOnFailure(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	tun, err := CreateGRPCTunnel(context.Background(), "127.0.0.1:12345",
		grpc.WithInsecure(), grpc.WithBlock(), grpc.WithReturnConnectionError(),
		grpc.WithTimeout(100*time.Millisecond))
	if err == nil {
		t.Fatal("expected dial error")
	}
	if tun != nil {
		t.Fatal("expected nil tunnel on dial failure")
	}
}

// controllableStream is a fully test-controlled fake ProxyService_ProxyClient
// for the concurrent-close test, where we need both a working DIAL_RSP and a
// blockable Recv() lifetime.
type controllableStream struct {
	grpc.ClientStream
	send func(*client.Packet) error
	recv func() (*client.Packet, error)
}

func (s *controllableStream) Send(p *client.Packet) error   { return s.send(p) }
func (s *controllableStream) CloseSend() error              { return nil }
func (s *controllableStream) Context() context.Context      { return context.Background() }
func (s *controllableStream) Recv() (*client.Packet, error) { return s.recv() }
