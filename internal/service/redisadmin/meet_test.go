package redisadmin

import "testing"

// TestKnowsAll (#1568): a node still in its handshake is listed under a
// random ID with the handshake flag, and does not count as known.
func TestKnowsAll(t *testing.T) {
	const nodes = "aaa 10.0.0.1:6379@16379 myself,master - 0 0 1 connected 0-8191\n" +
		"bbb 10.0.0.2:6379@16379 master - 0 1 2 connected 8192-16383\n" +
		"f00 10.0.0.3:6379@16379 handshake - 0 0 0 connected\n" +
		"ddd :0@0 master,noaddr - 0 0 0 disconnected\n"
	for _, c := range []struct {
		ids  []string
		want bool
	}{
		{[]string{"aaa", "bbb"}, true},
		{[]string{"aaa", "bbb", "ccc"}, false},
		{[]string{"aaa", "f00"}, false},
		{[]string{"ddd"}, false},
	} {
		if got := knowsAll(nodes, c.ids); got != c.want {
			t.Errorf("knowsAll(%v) = %v, want %v", c.ids, got, c.want)
		}
	}
}
