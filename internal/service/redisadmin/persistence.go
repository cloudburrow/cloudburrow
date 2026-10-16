package redisadmin

import (
	"context"
	"strconv"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// persistence is an instance's persistence_config (#1384): RDB snapshots
// every Period from Start, or DISABLED.
//
// RDB is carried out by the server's own save point: `save <period> 1`, a
// snapshot once the period has passed with a change, and one more when
// the server stops, in place of the append-only file a persistent
// CloudBurrow instance otherwise keeps. In persistent mode the snapshot is
// on the server's volume, so the data comes back with the server. Valkey
// counts the period from its last snapshot rather than aligning it to
// rdb_snapshot_start_time; rdb_next_snapshot_time is the aligned time
// Memorystore documents.
type persistence struct {
	Mode   string    `json:"mode"`
	Period string    `json:"period,omitempty"`
	Start  time.Time `json:"start,omitempty"`
}

var periods = map[redispb.PersistenceConfig_SnapshotPeriod]time.Duration{
	redispb.PersistenceConfig_ONE_HOUR:          time.Hour,
	redispb.PersistenceConfig_SIX_HOURS:         6 * time.Hour,
	redispb.PersistenceConfig_TWELVE_HOURS:      12 * time.Hour,
	redispb.PersistenceConfig_TWENTY_FOUR_HOURS: 24 * time.Hour,
}

// newPersistence is p as kept, over old: a field p leaves out keeps its
// value, as Memorystore documents ("if not provided, the existing value
// will be used"), and an RDB period or start time not given defaults to
// TWENTY_FOUR_HOURS and now.
func newPersistence(p *redispb.PersistenceConfig, old *persistence, now time.Time) (*persistence, error) {
	out := &persistence{Mode: redispb.PersistenceConfig_DISABLED.String()}
	if old != nil {
		*out = *old
	}
	switch p.GetPersistenceMode() {
	case redispb.PersistenceConfig_PERSISTENCE_MODE_UNSPECIFIED:
		if old == nil && (p.GetRdbSnapshotPeriod() != 0 || p.GetRdbSnapshotStartTime() != nil) {
			return nil, apierror.InvalidArgument("persistence_config.persistence_mode is required with an RDB snapshot period or start time")
		}
	case redispb.PersistenceConfig_DISABLED:
		return &persistence{Mode: redispb.PersistenceConfig_DISABLED.String()}, nil
	case redispb.PersistenceConfig_RDB:
		out.Mode = redispb.PersistenceConfig_RDB.String()
	default:
		return nil, apierror.InvalidArgument("persistence_config.persistence_mode %v is not DISABLED or RDB", p.GetPersistenceMode())
	}
	if pp := p.GetRdbSnapshotPeriod(); pp != redispb.PersistenceConfig_SNAPSHOT_PERIOD_UNSPECIFIED {
		if _, ok := periods[pp]; !ok {
			return nil, apierror.InvalidArgument("persistence_config.rdb_snapshot_period %v is not a snapshot period", pp)
		}
		out.Period = pp.String()
	}
	if t := p.GetRdbSnapshotStartTime(); t != nil {
		if err := t.CheckValid(); err != nil {
			return nil, apierror.InvalidArgument("persistence_config.rdb_snapshot_start_time: %v", err)
		}
		out.Start = t.AsTime()
	}
	if out.Mode == redispb.PersistenceConfig_RDB.String() {
		if out.Period == "" {
			out.Period = redispb.PersistenceConfig_TWENTY_FOUR_HOURS.String()
		}
		if out.Start.IsZero() {
			out.Start = now.UTC().Truncate(time.Second)
		}
	}
	return out, nil
}

func (p *persistence) period() time.Duration {
	if p == nil || p.Mode != redispb.PersistenceConfig_RDB.String() {
		return 0
	}
	return periods[redispb.PersistenceConfig_SnapshotPeriod(redispb.PersistenceConfig_SnapshotPeriod_value[p.Period])]
}

// seconds is the server's save point, 0 without RDB persistence.
func (p *persistence) seconds() int { return int(p.period() / time.Second) }

// render is p as Google shapes it, with the next snapshot after now.
func (p *persistence) render(now time.Time) *redispb.PersistenceConfig {
	if p == nil {
		return nil
	}
	out := &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_PersistenceMode(redispb.PersistenceConfig_PersistenceMode_value[p.Mode])}
	if d := p.period(); d > 0 {
		out.RdbSnapshotPeriod = redispb.PersistenceConfig_SnapshotPeriod(redispb.PersistenceConfig_SnapshotPeriod_value[p.Period])
		out.RdbSnapshotStartTime = timestamppb.New(p.Start)
		next := p.Start
		if now.After(next) {
			next = next.Add((now.Sub(next)/d + 1) * d)
		}
		out.RdbNextSnapshotTime = timestamppb.New(next)
	}
	return out
}

// persist applies p to r's running server: RDB's save point, without the
// append-only file, and a snapshot now, so the file on disk has the data
// at once; or, DISABLED, no save point and the mode's own AOF back.
func (c *instances) persist(ctx context.Context, r *record, p *persistence) error {
	dp := c.dial(r.addr, r.AuthString)
	settings := map[string]string{"save": "", "appendonly": "no"}
	dpAll := func(ctx context.Context, s map[string]string) error { return c.configAll(ctx, r, s) }
	if s := p.seconds(); s > 0 {
		settings["save"] = strconv.Itoa(s) + " 1"
	} else if c.aof {
		settings["appendonly"] = "yes"
	}
	if err := dpAll(ctx, settings); err != nil {
		return apierror.InvalidArgument("the server refused the change: %v", err)
	}
	if p.seconds() > 0 {
		if _, err := dp.Save(ctx); err != nil {
			return apierror.Internal(err, "snapshot instance %s", r.Name)
		}
	}
	return nil
}
