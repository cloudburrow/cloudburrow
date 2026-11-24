package main

import (
	"testing"
	"time"
)

// The pooler's log is counted by database (#2163): each "login attempt"
// an attempt, each "pooler error" an error, the console's own sign-ins
// and errors before a database is known left out, and a second read
// counting only the lines after the first's last.
func TestCountPoolerLog(t *testing.T) {
	log := `2026-11-22T12:00:00.100000000Z 2026-11-22 12:00:00.100 UTC [1] LOG C-0x55d0c5e6a9b0: app/alice@127.0.0.1:41234 login attempt: db=app user=alice tls=no replication=no
2026-11-22T12:00:00.200000000Z 2026-11-22 12:00:00.200 UTC [1] WARNING C-0x55d0c5e6a9c0: app/bob@127.0.0.1:41236 pooler error: password authentication failed
2026-11-22T12:00:00.300000000Z 2026-11-22 12:00:00.300 UTC [1] LOG C-0x55d0c5e6a9d0: pgbouncer/cloudburrow_stats@127.0.0.1:41238 login attempt: db=pgbouncer user=cloudburrow_stats tls=no replication=no
2026-11-22T12:00:00.400000000Z 2026-11-22 12:00:00.400 UTC [1] WARNING C-0x55d0c5e6a9e0: (nodb)/(nouser)@127.0.0.1:41240 pooler error: client_login_timeout
2026-11-22T12:00:01.500000000Z 2026-11-22 12:00:01.500 UTC [1] LOG C-0x55d0c5e6a9f0: other/alice@127.0.0.1:41242 login attempt: db=other user=alice tls=no replication=no
`
	counts := map[string][2]uint64{}
	last := countPoolerLog(log, time.Time{}, counts)
	if counts["app"] != [2]uint64{1, 1} || counts["other"] != [2]uint64{1, 0} || len(counts) != 2 {
		t.Errorf("counts %v; want app 1 attempt 1 error, other 1 attempt", counts)
	}
	if want := time.Date(2026, 11, 22, 12, 0, 1, 5e8, time.UTC); !last.Equal(want) {
		t.Errorf("last %v; want %v", last, want)
	}
	// The next read starts at the last line's second: its lines again.
	more := log[len(log)-len(`2026-11-22T12:00:01.500000000Z 2026-11-22 12:00:01.500 UTC [1] LOG C-0x55d0c5e6a9f0: other/alice@127.0.0.1:41242 login attempt: db=other user=alice tls=no replication=no
`):] + `2026-11-22T12:00:02.000000000Z 2026-11-22 12:00:02.000 UTC [1] LOG C-0x55d0c5e6aa00: app/alice@127.0.0.1:41244 login attempt: db=app user=alice tls=no replication=no
`
	countPoolerLog(more, last, counts)
	if counts["app"] != [2]uint64{2, 1} || counts["other"] != [2]uint64{1, 0} {
		t.Errorf("after the second read counts %v; want app 2 attempts, other 1", counts)
	}
}
