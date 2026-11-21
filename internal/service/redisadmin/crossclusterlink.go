package redisadmin

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
)

// Cross-region (cross-instance) replication in CLUSTER mode (#1577), as
// Google documents it for Memorystore for Valkey: a secondary has its
// primary's shard count, serves reads of the primary's data and refuses
// writes until a switchover or a detach.
//
// A cluster-mode node refuses REPLICAOF and follows only a primary of its
// own cluster, so each shard of the secondary is fed by a link that
// CloudBurrow runs: it replicates the primary's shard as a replica does
// (PSYNC, then the replication stream), and applies what it receives to
// the secondary's shard that holds the same slots. The data that exists
// when a link starts is copied while the source shard's writes are paused
// (CLIENT PAUSE WRITE): the link's PSYNC is answered at the paused offset,
// each key is copied with DUMP and RESTORE, functions with FUNCTION DUMP
// and RESTORE, and the writes resume, so the stream that follows starts
// exactly where the copy ends and no write is lost or applied twice. The
// link acknowledges its offset (REPLCONF ACK) as a replica does; a link
// that breaks (a failover of either shard, a restart) starts again with a
// full copy. The secondary's clients are refused writes by ACL (-@write on
// every user, the default one included), and the link writes as a user of
// its own.

// crrUser is the ACL user a cross-region replication link writes as on a
// secondary's nodes.
const crrUser = "cloudburrow-replication"

// crrLink is the replication of a primary into a CLUSTER mode secondary.
type crrLink struct {
	primary string
	cancel  context.CancelFunc
	done    chan struct{}

	mu     sync.Mutex
	shards int // shards of the current attempt
	synced map[int]bool
	err    error
}

func (l *crrLink) setSynced(i int) {
	l.mu.Lock()
	l.synced[i] = true
	l.mu.Unlock()
}

// linked is whether every shard of the current attempt is copied and
// streaming, and the last error otherwise.
func (l *crrLink) linked() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shards > 0 && len(l.synced) == l.shards, l.err
}

// shardEnd is one end of a shard link: a primary node's server.
type shardEnd struct {
	addr, user, password, ca string
	ranges                   [][2]int64
}

// crossClusterLink links r, a CLUSTER mode secondary, to its primary p and
// waits until every shard is copied and streaming.
func (c *clusters) crossClusterLink(ctx context.Context, r, p string) error {
	c.mu.Lock()
	if c.links == nil {
		c.links = map[string]*crrLink{}
	}
	l := c.links[r]
	if l == nil || l.primary != p {
		if l != nil {
			l.cancel()
		}
		lctx, cancel := context.WithCancel(c.bg)
		l = &crrLink{primary: p, cancel: cancel, done: make(chan struct{}), synced: map[int]bool{}}
		c.links[r] = l
		go c.runLink(lctx, l, r, p)
	}
	c.mu.Unlock()
	return c.until(ctx, "the replication of "+p+"'s shards into "+r, func() bool {
		ok, _ := l.linked()
		return ok
	})
}

// crossClusterUnlink stops r's links, if any, and waits until they have.
func (c *clusters) crossClusterUnlink(r string) {
	c.mu.Lock()
	l := c.links[r]
	delete(c.links, r)
	c.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
	}
}

// runLink keeps l's shard links running until ctx ends: each attempt
// pairs the shards and runs one link per pair, and starts again a second
// after any of them fails.
func (c *clusters) runLink(ctx context.Context, l *crrLink, r, p string) {
	defer close(l.done)
	for ctx.Err() == nil {
		pairs, err := c.shardPairs(ctx, r, p)
		l.mu.Lock()
		l.shards, l.synced, l.err = len(pairs), map[int]bool{}, err
		l.mu.Unlock()
		if err == nil {
			actx, cancel := context.WithCancel(ctx)
			errs := make(chan error, len(pairs))
			for i, pr := range pairs {
				go func() { errs <- linkShard(actx, pr[0], pr[1], func() { l.setSynced(i) }) }()
			}
			err = <-errs
			cancel()
			for range pairs[1:] {
				<-errs
			}
			l.mu.Lock()
			l.err = err
			l.mu.Unlock()
		}
		if ctx.Err() == nil {
			log.Printf("memorystore: %s: replication of %s: %v; starting again", r, p, err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// shardPairs pairs each primary node of p with the primary node of r that
// holds the same slots.
func (c *clusters) shardPairs(ctx context.Context, r, p string) ([][2]shardEnd, error) {
	c.mu.Lock()
	sr, pr := c.byName[r], c.byName[p]
	if sr == nil || pr == nil || sr.Valkey == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("instance %s or %s is gone", r, p)
	}
	snode := append([]*clusterNode(nil), sr.Nodes...)
	pnode := append([]*clusterNode(nil), pr.Nodes...)
	sto := shardEnd{user: crrUser, password: sr.Valkey.LinkPassword, ca: sr.TransitCA}
	from := shardEnd{password: pr.password(), ca: pr.TransitCA}
	spass, sca := sr.password(), sr.TransitCA
	c.mu.Unlock()
	src, err := c.primaryRanges(ctx, pnode, from.password, from.ca)
	if err != nil {
		return nil, fmt.Errorf("primary %s: %w", p, err)
	}
	dst, err := c.primaryRanges(ctx, snode, spass, sca)
	if err != nil {
		return nil, fmt.Errorf("secondary %s: %w", r, err)
	}
	byRanges := map[string]string{}
	for addr, rs := range dst {
		byRanges[fmt.Sprint(rs)] = addr
	}
	var out [][2]shardEnd
	for addr, rs := range src {
		to, ok := byRanges[fmt.Sprint(rs)]
		if !ok {
			return nil, fmt.Errorf("no shard of %s holds the slots %v of %s's shard at %s", r, rs, p, addr)
		}
		a, b := from, sto
		a.addr, a.ranges, b.addr, b.ranges = addr, rs, to, rs
		out = append(out, [2]shardEnd{a, b})
	}
	if len(out) != len(dst) {
		return nil, fmt.Errorf("%s has %d shards with slots and %s %d", p, len(out), r, len(dst))
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0].ranges[0][0] < out[j][0].ranges[0][0] })
	return out, nil
}

// primaryRanges are the slot ranges of each primary node among nodes, by
// its address: CLUSTER SLOTS names each range's primary by its node ID,
// and each node's own ID (CLUSTER MYID) is matched to its address.
func (c *clusters) primaryRanges(ctx context.Context, nodes []*clusterNode, password, ca string) (map[string][][2]int64, error) {
	ids := map[string]string{}
	var slots any
	for _, n := range nodes {
		if n.addr == "" {
			continue
		}
		cl := c.dial(n.addr, password, ca)
		id, err := cl.Do(ctx, "CLUSTER", "MYID")
		if err == nil && slots == nil {
			slots, err = cl.Do(ctx, "CLUSTER", "SLOTS")
		}
		cl.Close()
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Server, err)
		}
		ids[fmt.Sprint(id)] = n.addr
	}
	out := map[string][][2]int64{}
	list, _ := slots.([]any)
	for _, e := range list {
		row, _ := e.([]any)
		if len(row) < 3 {
			continue
		}
		owner, _ := row[2].([]any)
		if len(owner) < 3 {
			continue
		}
		addr, ok := ids[fmt.Sprint(owner[2])]
		if !ok {
			return nil, fmt.Errorf("the primary of slots %v-%v is not a node of the instance", row[0], row[1])
		}
		lo, _ := strconv.ParseInt(fmt.Sprint(row[0]), 10, 64)
		hi, _ := strconv.ParseInt(fmt.Sprint(row[1]), 10, 64)
		out[addr] = append(out[addr], [2]int64{lo, hi})
	}
	if len(out) == 0 {
		return nil, errors.New("no slots are assigned")
	}
	for a := range out {
		sort.Slice(out[a], func(i, j int) bool { return out[a][i][0] < out[a][j][0] })
	}
	return out, nil
}

// endClient is a go-redis client of one end.
func endClient(e shardEnd) (*redis.Client, error) {
	tc, err := ClientTLS(e.ca)
	if err != nil {
		return nil, err
	}
	return redis.NewClient(&redis.Options{Addr: e.addr, Username: e.user, Password: e.password, MaxRetries: -1, PoolSize: 1,
		TLSConfig: tc, ReadTimeout: clusterNodeTimeout, WriteTimeout: clusterNodeTimeout}), nil
}

// dialEnd opens a raw connection to one end, authenticated.
func dialEnd(ctx context.Context, e shardEnd) (net.Conn, *bufio.Reader, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if e.ca != "" {
		tc, terr := ClientTLS(e.ca)
		if terr != nil {
			return nil, nil, terr
		}
		host, _, _ := net.SplitHostPort(e.addr)
		tc.ServerName = host
		conn, err = (&tls.Dialer{NetDialer: d, Config: tc}).DialContext(ctx, "tcp", e.addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", e.addr)
	}
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	if e.password != "" {
		args := []string{"AUTH", e.password}
		if e.user != "" {
			args = []string{"AUTH", e.user, e.password}
		}
		if _, err := conn.Write(respCommand(args)); err != nil {
			conn.Close()
			return nil, nil, err
		}
		if _, err := readLineReply(br); err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("AUTH: %w", err)
		}
	}
	return conn, br, nil
}

// linkShard replicates the shard whose primary is from into the shard
// whose primary is to, until ctx ends or the link breaks; synced is called
// once the data is copied and the stream is being applied.
func linkShard(ctx context.Context, from, to shardEnd, synced func()) error {
	admin, err := endClient(from)
	if err != nil {
		return err
	}
	defer admin.Close()
	dst, err := endClient(to)
	if err != nil {
		return err
	}
	defer dst.Close()
	src, br, err := dialEnd(ctx, from)
	if err != nil {
		return fmt.Errorf("dial %s: %w", from.addr, err)
	}
	defer src.Close()
	stop := context.AfterFunc(ctx, func() { _ = src.Close() })
	defer stop()

	// Pause the source's writes, so its data and the offset PSYNC
	// answers with stay as they are until the copy is made.
	if err := admin.Do(ctx, "CLIENT", "PAUSE", 300000, "WRITE").Err(); err != nil {
		return fmt.Errorf("CLIENT PAUSE on %s: %w", from.addr, err)
	}
	paused := true
	unpause := func() {
		if paused {
			paused = false
			_ = admin.Do(context.Background(), "CLIENT", "UNPAUSE").Err()
		}
	}
	defer unpause()
	offset, err := fullSync(src, br)
	if err != nil {
		return fmt.Errorf("PSYNC from %s: %w", from.addr, err)
	}
	if err := copyShard(ctx, admin, dst); err != nil {
		return fmt.Errorf("copy %s into %s: %w", from.addr, to.addr, err)
	}
	unpause()

	out, obr, err := dialEnd(ctx, to)
	if err != nil {
		return fmt.Errorf("dial %s: %w", to.addr, err)
	}
	defer out.Close()
	stopOut := context.AfterFunc(ctx, func() { _ = out.Close() })
	defer stopOut()
	go drainReplies(obr, to.addr)
	synced()
	return applyStream(ctx, src, br, out, offset)
}

// fullSync asks the source for a full resynchronization and reads past the
// RDB payload it sends, returning the offset the stream starts at.
func fullSync(src net.Conn, br *bufio.Reader) (int64, error) {
	if _, err := src.Write(respCommand([]string{"REPLCONF", "capa", "psync2"})); err != nil {
		return 0, err
	}
	if _, err := readLineReply(br); err != nil {
		return 0, fmt.Errorf("REPLCONF capa: %w", err)
	}
	if _, err := src.Write(respCommand([]string{"PSYNC", "?", "-1"})); err != nil {
		return 0, err
	}
	line, err := readLineReply(br)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != "FULLRESYNC" {
		return 0, fmt.Errorf("PSYNC answered %q", line)
	}
	offset, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("PSYNC answered %q", line)
	}
	// The RDB payload: newlines while it is being made, then $<length>.
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b == '\n' {
			continue
		}
		if b != '$' {
			return 0, fmt.Errorf("unexpected %q before the RDB payload", b)
		}
		break
	}
	l, err := br.ReadString('\n')
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(l), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("RDB payload length %q", l)
	}
	if _, err := io.CopyN(io.Discard, br, n); err != nil {
		return 0, err
	}
	return offset, nil
}

// copyShard copies every key of the source node, with its TTL, and its
// functions into the target node, which it empties first.
func copyShard(ctx context.Context, src, dst *redis.Client) error {
	if err := dst.Do(ctx, "FLUSHALL", "SYNC").Err(); err != nil {
		return fmt.Errorf("FLUSHALL: %w", err)
	}
	fn, err := src.Do(ctx, "FUNCTION", "DUMP").Text()
	if err != nil {
		return fmt.Errorf("FUNCTION DUMP: %w", err)
	}
	if err := dst.Do(ctx, "FUNCTION", "RESTORE", fn, "FLUSH").Err(); err != nil {
		return fmt.Errorf("FUNCTION RESTORE: %w", err)
	}
	var cursor uint64
	for {
		keys, next, err := src.Scan(ctx, cursor, "", 500).Result()
		if err != nil {
			return fmt.Errorf("SCAN: %w", err)
		}
		if len(keys) > 0 {
			p := src.Pipeline()
			ttls := make([]*redis.Cmd, len(keys))
			dumps := make([]*redis.StringCmd, len(keys))
			for i, k := range keys {
				ttls[i] = p.Do(ctx, "PTTL", k)
				dumps[i] = p.Dump(ctx, k)
			}
			_, _ = p.Exec(ctx)
			w := dst.Pipeline()
			n := 0
			for i, k := range keys {
				payload, err := dumps[i].Result()
				if err != nil {
					continue // gone since the scan
				}
				// PTTL: -2 for a key gone since the scan, -1 for none.
				ttl, err := ttls[i].Int64()
				if err != nil || ttl == -2 {
					continue
				}
				ms := max(ttl, 0)
				if ttl == 0 {
					ms = 1 // expiring now; 0 would keep it forever
				}
				w.Do(ctx, "RESTORE", k, ms, payload, "REPLACE")
				n++
			}
			if n > 0 {
				cmds, err := w.Exec(ctx)
				if err != nil {
					for _, c := range cmds {
						if c.Err() != nil {
							return fmt.Errorf("RESTORE: %w", c.Err())
						}
					}
					return fmt.Errorf("RESTORE: %w", err)
				}
			}
		}
		if cursor = next; cursor == 0 {
			return nil
		}
	}
}

// applyStream applies the source's replication stream, from offset, to
// out, acknowledging the offset each second and when asked (GETACK).
func applyStream(ctx context.Context, src net.Conn, br *bufio.Reader, out net.Conn, offset int64) error {
	var wmu sync.Mutex
	var off int64 = offset
	ack := func() error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = src.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, err := src.Write(respCommand([]string{"REPLCONF", "ACK", strconv.FormatInt(off, 10)}))
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if ack() != nil {
					return
				}
			}
		}
	}()
	w := bufio.NewWriter(out)
	for {
		args, n, err := readCommand(br)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("replication stream: %w", err)
		}
		wmu.Lock()
		off += n
		wmu.Unlock()
		if len(args) == 0 {
			continue
		}
		switch strings.ToLower(args[0]) {
		case "ping", "select":
			continue
		case "replconf":
			if len(args) > 1 && strings.EqualFold(args[1], "getack") {
				if err := ack(); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := w.Write(respCommand(args)); err != nil {
			return err
		}
		if br.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return err
			}
		}
	}
}

// drainReplies reads the target's replies to the applied commands, and
// logs the errors among them.
func drainReplies(br *bufio.Reader, addr string) {
	logged := 0
	for {
		v, err := readReply(br)
		if err != nil {
			return
		}
		if e, ok := v.(replyError); ok && logged < 10 {
			logged++
			log.Printf("memorystore: replication into %s: %s", addr, string(e))
		}
	}
}

// respCommand encodes args as a RESP array of bulk strings.
func respCommand(args []string) []byte {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	return []byte(b.String())
}

type replyError string

// readLineReply reads a simple string reply, or its error.
func readLineReply(br *bufio.Reader) (string, error) {
	v, err := readReply(br)
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case replyError:
		return "", errors.New(string(x))
	case string:
		return x, nil
	}
	return fmt.Sprint(v), nil
}

// readReply reads one RESP2 reply.
func readReply(br *bufio.Reader) (any, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return nil, errors.New("empty reply line")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return replyError(line[1:]), nil
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		out := make([]any, n)
		for i := range out {
			if out[i], err = readReply(br); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unexpected reply %q", line)
}

// readCommand reads one command of a replication stream, a RESP array of
// bulk strings, and the bytes it took.
func readCommand(br *bufio.Reader) ([]string, int64, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, 0, err
	}
	n := int64(len(line))
	t := strings.TrimRight(line, "\r\n")
	if t == "" {
		return nil, n, nil // a keepalive newline
	}
	if t[0] != '*' {
		return strings.Fields(t), n, nil // an inline command
	}
	count, err := strconv.Atoi(t[1:])
	if err != nil {
		return nil, n, fmt.Errorf("bad array header %q", t)
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, n, err
		}
		n += int64(len(h))
		h = strings.TrimRight(h, "\r\n")
		if h == "" || h[0] != '$' {
			return nil, n, fmt.Errorf("bad bulk header %q", h)
		}
		l, err := strconv.Atoi(h[1:])
		if err != nil || l < 0 {
			return nil, n, fmt.Errorf("bad bulk length %q", h)
		}
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, n, err
		}
		n += int64(l + 2)
		args = append(args, string(buf[:l]))
	}
	return args, n, nil
}

// crrSecondary is whether r is a CLUSTER mode secondary of cross-instance
// replication, whose clients are refused writes.
func (r *clusterRecord) crrSecondary() bool {
	return r.Valkey != nil && !r.standalone() && r.Valkey.role() == memorystorepb.CrossInstanceReplicationConfig_SECONDARY.String()
}

// crrUsers are r's ACL users with writes refused (block) or allowed: in
// CLUSTER mode every user, the default one included, is refused writes on
// a secondary, and the link's own user is there to write.
func (c *clusters) crrUsers(r *clusterRecord, block bool) map[string][]any {
	users := c.aclUsersOf(r)
	if r.Valkey == nil || r.standalone() {
		return users
	}
	out := map[string][]any{}
	for u, rule := range users {
		out[u] = append([]any(nil), rule...)
	}
	def := out["default"]
	if def == nil {
		def = []any{"on"}
	}
	out["default"] = append(def, "+@all")
	if block {
		for u := range out {
			out[u] = append(out[u], "-@write")
		}
		out[crrUser] = []any{"reset", "on", ">" + r.Valkey.LinkPassword, "~*", "&*", "+@all"}
	}
	return out
}

// setWrites refuses (block) or allows the writes of r's clients on nodes.
func (c *clusters) setWrites(ctx context.Context, r *clusterRecord, nodes []*clusterNode, block bool) error {
	c.mu.Lock()
	if block && r.Valkey.LinkPassword == "" {
		r.Valkey.LinkPassword = newUID() + newUID()
	}
	users := c.crrUsers(r, block)
	c.mu.Unlock()
	return c.syncUsers(ctx, r, users, nodes)
}

// relinkClusters is a switchover or detach in CLUSTER mode: each of unlink
// stops replicating and takes writes again, then each of link refuses
// writes and replicates primary.
func (c *clusters) relinkClusters(ctx context.Context, snaps map[*clusterRecord]*clusterRecord, unlink, link []*clusterRecord, primary *clusterRecord) error {
	for _, x := range unlink {
		c.crossClusterUnlink(x.Name)
		if err := c.setWrites(ctx, snaps[x], snaps[x].Nodes, false); err != nil {
			return fmt.Errorf("let %s take writes: %w", x.Name, err)
		}
	}
	for _, x := range link {
		c.crossClusterUnlink(x.Name)
		if err := c.setWrites(ctx, snaps[x], snaps[x].Nodes, true); err != nil {
			return fmt.Errorf("refuse %s's writes: %w", x.Name, err)
		}
		if err := c.crossClusterLink(ctx, x.Name, primary.Name); err != nil {
			return fmt.Errorf("replicate %s into %s: %w", primary.Name, x.Name, err)
		}
	}
	return nil
}
