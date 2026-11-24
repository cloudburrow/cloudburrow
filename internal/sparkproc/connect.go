package sparkproc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ConnectPort is the Spark Connect server's port in its container, Spark's
// default (spark.connect.grpc.binding.port).
const ConnectPort = "15002"

// connectReady is the line Spark's SparkConnectServer logs once it serves.
const connectReady = "Spark Connect server started"

// Connect is a Spark Connect server an interactive session of Serverless
// for Apache Spark runs (#2156): the runtime's Spark, the BigQuery
// connector on its class path, in a container on its own --internal
// network, reached through its relay's port on the host's loopback.
type Connect struct {
	// Addr is the server's address on the host (127.0.0.1:port).
	Addr string

	p      *prepared
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	res    Result
}

// Address is Addr.
func (c *Connect) Address() string { return c.Addr }

// Done is closed when the server has stopped.
func (c *Connect) Done() <-chan struct{} { return c.done }

// Result is how the server ended, once Done is closed: Failed with its
// error when it stopped by itself.
func (c *Connect) Result() Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.res
}

// Stop stops the server and removes its containers and network.
func (c *Connect) Stop() {
	c.once.Do(func() { c.cancel() })
	<-c.done
}

// StartConnect starts job (KindConnect) and returns once its server
// answers, or how it failed to.
func (d *Docker) StartConnect(ctx context.Context, job Job) (*Connect, error) {
	job.Kind = KindConnect
	if job.KMSKey != "" {
		if res, refused := d.checkKey(ctx, job); refused {
			return nil, errors.New(res.KeyRefused)
		}
	}
	p, failed, err := d.prepare(ctx, job, ConnectPort)
	if err != nil {
		return nil, err
	}
	if failed != nil {
		return nil, errors.New(failed.Message)
	}
	out, err := d.docker(ctx, "port", p.relay, ConnectPort)
	if err != nil {
		p.close()
		return nil, &Unavailable{Msg: "find the Spark Connect server's port: " + out}
	}
	addr := strings.TrimSpace(strings.Split(out, "\n")[0])
	d.logf("spark: starting the Spark Connect server of %s on runtime %s in %s, on %s", job.SparkJobID, p.version, p.image, addr)
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &Connect{Addr: addr, p: p, cancel: cancel, done: make(chan struct{})}
	cmd := exec.CommandContext(rctx, "docker", p.run...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		p.close()
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		p.close()
		return nil, &Unavailable{Msg: "start the Spark container: " + err.Error()}
	}
	ready := make(chan struct{})
	var readyOnce sync.Once
	go func() {
		defer close(c.done)
		tail, _ := d.follow(job, stdout, func(line string) {
			if strings.Contains(line, connectReady) {
				readyOnce.Do(func() { close(ready) })
			}
		})
		waitErr := cmd.Wait()
		p.close()
		c.mu.Lock()
		if rctx.Err() == nil && waitErr != nil {
			c.res = Result{Failed: true, Message: failure(tail())}
		}
		c.mu.Unlock()
		cancel()
	}()
	select {
	case <-ready:
		return c, nil
	case <-c.done:
		res := c.Result()
		if res.Message == "" {
			res.Message = "the Spark Connect server stopped before it started"
		}
		return nil, errors.New(res.Message)
	case <-ctx.Done():
		c.Stop()
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		c.Stop()
		return nil, fmt.Errorf("the Spark Connect server did not start in 10 minutes")
	}
}
