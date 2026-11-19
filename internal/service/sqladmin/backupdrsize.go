package sqladmin

import (
	"io/fs"
	"path/filepath"
)

// BackupDRRunSize is the size in bytes of a Backup and DR backup run's
// dump: what its vault stores (#1921). 0 when the run has no dump.
func (s *Server) BackupDRRunSize(id string) int64 {
	if _, ok := s.BackupDRRun(id); !ok {
		return 0
	}
	var n int64
	_ = filepath.WalkDir(s.backups.runDir(id), func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}
