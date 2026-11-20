package bigtablefront

// A table's cluster_states (#1858): the emulator keeps no replicas and
// answers GetTable and ListTables without them, so the front fills them in
// for the views that carry them (REPLICATION_VIEW, ENCRYPTION_VIEW and
// FULL, as table.proto documents the field). Each cluster of the table's
// instance holds the table's one copy, which is written before the call
// that wrote it returns: every cluster is READY, never INITIALIZING or
// PLANNED_MAINTENANCE. The front encrypts nothing with a Cloud KMS key, so
// each cluster's encryption is Google-managed (GOOGLE_DEFAULT_ENCRYPTION).
// A table of an instance no call created (the emulator's own clients name
// one they never made) has no clusters to report, and gets none.

import (
	"strings"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
)

// clusterStatesView reports whether view carries cluster_states, and
// encryption whether it carries their encryption_info.
func clusterStatesView(view btapb.Table_View) (states, encryption bool) {
	switch view {
	case btapb.Table_REPLICATION_VIEW:
		return true, false
	case btapb.Table_ENCRYPTION_VIEW, btapb.Table_FULL:
		return true, true
	}
	return false, false
}

// withClusterStates sets t's cluster_states for view.
func (s *instances) withClusterStates(t *btapb.Table, view btapb.Table_View) {
	states, encryption := clusterStatesView(view)
	if !states {
		return
	}
	name := t.GetName()
	i := strings.Index(name, "/tables/")
	if i < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.byNm[name[:i]]
	if !ok || len(in.clusters) == 0 {
		return
	}
	t.ClusterStates = map[string]*btapb.Table_ClusterState{}
	for id := range in.clusters {
		cs := &btapb.Table_ClusterState{ReplicationState: btapb.Table_ClusterState_READY}
		if encryption {
			cs.EncryptionInfo = []*btapb.EncryptionInfo{{EncryptionType: btapb.EncryptionInfo_GOOGLE_DEFAULT_ENCRYPTION}}
		}
		t.ClusterStates[id] = cs
	}
}
