// Package mysqlpool is Cloud SQL for MySQL's Managed Connection Pooling
// (#2141), as Google documents it
// (https://docs.cloud.google.com/sql/docs/mysql/managed-connection-pooling,
// read 2026-11-22): "a server-side thread pool rather than an external
// connection multiplexer", in which "each open client connection still
// maintains a dedicated server session and counts toward the
// max_connections limit", and max_pool_size "controls the amount of
// concurrency".
//
// The community MySQL server CloudBurrow runs has no thread pool, so this
// front stands in the server's pod in front of mysqld, on every path a
// client takes to it (the instance's Service: its host tunnel, the
// connectors' 3307, its private IP and its Private Service Connect
// endpoints). Each client connection gets a server session of its own, as
// Google's does, and a statement runs only while it holds one of
// max_pool_size threads: from the command's first byte to the last packet
// of its answer, read by the MySQL client/server protocol. A connection
// waits in the queue for a thread (the pending connections) between
// statements, never holding one while it is idle, and one inside a
// transaction is served first, as MySQL's thread pools give an open
// transaction's statements priority. Replication sessions (binary log
// dumps) and the connection phase hold no thread.
//
// TLS is terminated here, with a certificate the front makes when it
// starts, as mysqld makes its own; the front signs in to the server over
// TLS too, so the server sees a secure connection where the client made
// one.
package mysqlpool

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Pool is max_pool_size threads that the connections' statements take
// turns on.
type Pool struct {
	mu     sync.Mutex
	size   int
	active int
	// high are waiters inside a transaction, low the others; each is a
	// list of chan struct{}, closed when given a thread.
	high, low *list.List

	// Stats.
	waited    time.Duration
	waits     int64
	maxActive int
}

// NewPool is a pool of size threads (at least 1).
func NewPool(size int) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{size: size, high: list.New(), low: list.New()}
}

// Size is max_pool_size.
func (p *Pool) Size() int { return p.size }

// Acquire waits for a thread, before others when inTx, and answers the
// function that gives it back.
func (p *Pool) Acquire(ctx context.Context, inTx bool) (release func(), err error) {
	start := time.Now()
	p.mu.Lock()
	if p.active < p.size && p.high.Len() == 0 && p.low.Len() == 0 {
		p.took(0)
		p.mu.Unlock()
		return p.releaser(), nil
	}
	ch := make(chan struct{})
	q := p.low
	if inTx {
		q = p.high
	}
	e := q.PushBack(ch)
	p.mu.Unlock()
	select {
	case <-ch:
		p.mu.Lock()
		p.waited += time.Since(start)
		p.waits++
		p.mu.Unlock()
		return p.releaser(), nil
	case <-ctx.Done():
		p.mu.Lock()
		select {
		case <-ch:
			// Handed a thread as the wait ended: give it on.
			p.mu.Unlock()
			p.releaser()()
		default:
			q.Remove(e)
			p.mu.Unlock()
		}
		return nil, ctx.Err()
	}
}

// took counts a thread taken, with p.mu held.
func (p *Pool) took(wait time.Duration) {
	p.active++
	p.maxActive = max(p.maxActive, p.active)
	if wait >= 0 {
		p.waits++
		p.waited += wait
	}
}

func (p *Pool) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.active--
			for _, q := range []*list.List{p.high, p.low} {
				if e := q.Front(); e != nil {
					q.Remove(e)
					p.active++
					p.maxActive = max(p.maxActive, p.active)
					close(e.Value.(chan struct{}))
					return
				}
			}
		})
	}
}

// Stats are the pool's figures, which Google's metrics name: the threads
// active and idle, the connections waiting for one (pending_connection),
// and the average time a statement waited (avg_wait_time); Waits and
// Waited are the statements given a thread since the pool started and the
// time they waited in all, from which Cloud Monitoring's avg_wait_time is
// each sample's average (#2163).
type Stats struct {
	Size, Active, Idle, Pending, MaxActive int
	AvgWait                                time.Duration
	Waits                                  int64
	Waited                                 time.Duration
}

// Stats answers the pool's figures now.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Stats{Size: p.size, Active: p.active, Idle: p.size - p.active, Pending: p.high.Len() + p.low.Len(), MaxActive: p.maxActive,
		Waits: p.waits, Waited: p.waited}
	if s.Idle < 0 {
		s.Idle = 0
	}
	if p.waits > 0 {
		s.AvgWait = p.waited / time.Duration(p.waits)
	}
	return s
}
