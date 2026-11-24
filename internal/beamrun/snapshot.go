package beamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Snapshots (#2215) are Apache Flink savepoints of a streaming job's Flink
// job, taken through the embedded Flink cluster's REST API
// (POST /jobs/:jobid/savepoints, without cancelling it) by the job's
// launcher, which reaches it on the job's network, into a directory of
// Docker.Snapshots mounted in each job server at snapshotMount. A job
// created from a snapshot is restored from it: the Flink runner's
// savepoint_path option.
const (
	snapshotMount  = "/cb-snapshots"
	flinkRESTPort  = 8081
	flinkConfig    = "rest.bind-address: 0.0.0.0\nrest.bind-port: 8081\nrest.address: 0.0.0.0\nrest.port: 8081\n"
	snapshotWait   = 10 * time.Minute
	locationFile   = "location"
	savepointCtlRE = `^[0-9a-f-]{8,64}$`
)

var snapshotIDRE = regexp.MustCompile(savepointCtlRE)

// Snapshot takes a savepoint of the running job jobID's state, as
// snapshot id, and answers its size on disk.
func (d *Docker) Snapshot(ctx context.Context, jobID, id string) (int64, error) {
	if d.Snapshots == "" {
		return 0, errors.New("this instance keeps no snapshots")
	}
	if !snapshotIDRE.MatchString(id) {
		return 0, fmt.Errorf("invalid snapshot ID %q", id)
	}
	d.runMu.Lock()
	dir := d.running[jobID]
	d.runMu.Unlock()
	if dir == "" {
		return 0, fmt.Errorf("job %s is not running", jobID)
	}
	target := filepath.Join(d.Snapshots, id)
	if err := os.MkdirAll(target, 0o777); err != nil {
		return 0, err
	}
	_ = os.Chmod(target, 0o777)
	req := filepath.Join(dir, "ctl", "savepoint-"+id)
	if err := os.WriteFile(req, nil, 0o644); err != nil {
		return 0, err
	}
	res := req + ".json"
	deadline := time.Now().Add(snapshotWait)
	for {
		b, err := os.ReadFile(res)
		if err == nil {
			var r struct{ Location, Error string }
			if err := json.Unmarshal(b, &r); err != nil {
				return 0, fmt.Errorf("the launcher's savepoint answer: %v", err)
			}
			if r.Error != "" || r.Location == "" {
				return 0, fmt.Errorf("the Flink savepoint failed: %s", r.Error)
			}
			loc := strings.TrimPrefix(r.Location, "file:")
			if err := os.WriteFile(filepath.Join(target, locationFile), []byte(loc), 0o644); err != nil {
				return 0, err
			}
			return dirSize(target), nil
		}
		if time.Now().After(deadline) {
			return 0, errors.New("the Flink savepoint did not complete")
		}
		d.runMu.Lock()
		still := d.running[jobID] == dir
		d.runMu.Unlock()
		if !still {
			return 0, fmt.Errorf("job %s stopped before its savepoint completed", jobID)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// DeleteSnapshot removes snapshot id's savepoint.
func (d *Docker) DeleteSnapshot(id string) error {
	if d.Snapshots == "" || !snapshotIDRE.MatchString(id) {
		return nil
	}
	return os.RemoveAll(filepath.Join(d.Snapshots, id))
}

// snapshotLocation is snapshot id's savepoint, as the job server sees it.
func (d *Docker) snapshotLocation(id string) (string, error) {
	if d.Snapshots == "" || !snapshotIDRE.MatchString(id) {
		return "", fmt.Errorf("the job's snapshot %s is not kept by this instance", id)
	}
	b, err := os.ReadFile(filepath.Join(d.Snapshots, id, locationFile))
	if err != nil {
		return "", fmt.Errorf("the job's snapshot %s has no savepoint: %v", id, err)
	}
	loc := strings.TrimSpace(string(b))
	if !strings.HasPrefix(loc, snapshotMount+"/"+id+"/") {
		return "", fmt.Errorf("the job's snapshot %s has an unexpected savepoint %q", id, loc)
	}
	return loc, nil
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
