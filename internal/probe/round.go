package probe

import (
	"context"
	"time"
)

// StopGrace is how long a round in flight may still run once the probe was
// told to stop.
const StopGrace = time.Second

// RoundContext returns the context of one round of reads for a probe that
// holds a connection. It does not end with the probe's context: a command
// that is cancelled halfway makes the driver cut the connection, the server
// answers a cut connection with a reset, and a port-forward or a tunnel on
// the way goes down with it. A round in flight when the probe stops gets
// StopGrace to finish instead, and every round is bounded by timeout.
func RoundContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	stop := context.AfterFunc(ctx, func() {
		t := time.NewTimer(StopGrace)
		defer t.Stop()
		select {
		case <-t.C:
			cancel()
		case <-rctx.Done():
		}
	})
	return rctx, func() {
		stop()
		cancel()
	}
}

// closed is what Done returns for a probe that never started.
var closed = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Lifetime implements Closer for a probe that runs one loop: embed it and
// start the loop with Go. The runtime waits for Done before the process
// exits, so a connection is not cut before it was released in order.
type Lifetime struct {
	done chan struct{}
}

// Done implements Closer.
func (l *Lifetime) Done() <-chan struct{} {
	if l.done == nil {
		return closed
	}
	return l.done
}

// Go runs loop in a goroutine and closes Done when it returns. loop releases
// what the probe holds before it returns.
func (l *Lifetime) Go(loop func()) {
	done := make(chan struct{})
	l.done = done
	go func() {
		defer close(done)
		loop()
	}()
}
