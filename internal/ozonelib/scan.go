package ozonelib

import (
	"context"
	"time"
)

// Scan listens on port for d and returns every server heard, with the
// counters. It shares the port with any running agent. It dials nothing.
func Scan(ctx context.Context, port int, d time.Duration) ([]Candidate, Counters, error) {
	conn, err := Listen(ctx, port)
	if err != nil {
		return nil, Counters{}, err
	}
	t := NewTable(time.Now)
	done := make(chan struct{})
	go func() {
		defer close(done)
		readLoop(conn, t, localLinks, nil)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	_ = conn.Close()
	<-done
	return t.Candidates(), t.Counters(), nil
}
