package parallel

import (
	"runtime"
	"sync"
	"sync/atomic"
)

const smallJob = 1024

func Workers(limit int) int {
	n := runtime.GOMAXPROCS(0)
	if n > limit {
		n = limit
	}
	if n < 1 {
		n = 1
	}
	return n
}

func For(n, chunk int, fn func(lo, hi int)) {
	if n <= 0 {
		return
	}
	if chunk < 1 {
		chunk = 1
	}
	chunks := (n + chunk - 1) / chunk
	workers := Workers(chunks)
	if workers <= 1 {
		fn(0, n)
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for {
				c := int(next.Add(1) - 1)
				if c >= chunks {
					return
				}
				lo := c * chunk
				fn(lo, min(lo+chunk, n))
			}
		}()
	}
	wg.Wait()
}

func Wavefront(rows, cols, lag int, fn func(x, y int)) {
	lanes := Workers(rows)
	if rows*cols < smallJob {
		lanes = 1
	}
	Rows(lanes, rows, cols, lag, func(y int) int { return y - 1 }, func(_, y int, wait, done func(x int)) {
		for x := 0; x < cols; x++ {
			wait(x)
			fn(x, y)
			done(x)
		}
	})
}

func Rows(lanes, rows, cols, lag int, trail func(y int) int, fn func(lane, y int, wait, done func(x int))) {
	if rows <= 0 || cols <= 0 {
		return
	}
	lanes = min(lanes, rows)
	if lanes <= 1 {
		idle := func(int) {}
		for y := 0; y < rows; y++ {
			fn(0, y, idle, idle)
		}
		return
	}
	gates := make([]gate, rows)
	for i := range gates {
		gates[i].wake = make(chan struct{}, 1)
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(lanes)
	for lane := 0; lane < lanes; lane++ {
		go func() {
			defer wg.Done()
			for {
				y := int(next.Add(1) - 1)
				if y >= rows {
					return
				}
				var above *gate
				if t := trail(y); t >= 0 {
					above = &gates[t]
				}
				wait := func(x int) {
					if above != nil {
						above.await(int32(min(x+lag, cols)))
					}
				}
				done := func(x int) { gates[y].advance(int32(x + 1)) }
				fn(lane, y, wait, done)
				gates[y].advance(int32(cols))
			}
		}()
	}
	wg.Wait()
}

const spinChecks = 64

type gate struct {
	progress atomic.Int32
	want     atomic.Int32
	wake     chan struct{}
}

func (g *gate) await(need int32) {
	for i := 0; i < spinChecks; i++ {
		if g.progress.Load() >= need {
			return
		}
	}
	g.want.Store(need)
	if g.progress.Load() >= need && g.want.CompareAndSwap(need, 0) {
		return
	}
	<-g.wake
}

func (g *gate) advance(to int32) {
	g.progress.Store(to)
	if w := g.want.Load(); w != 0 && to >= w && g.want.CompareAndSwap(w, 0) {
		g.wake <- struct{}{}
	}
}
