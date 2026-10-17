package bigqueryfront

import (
	"net/http"
)

// A project's purge (#1540). Google drops a purged project's datasets for
// good: none can be undeleted, and no deleted table of them restored. So a
// datasets.delete that carries PurgeHeader keeps nothing for undelete
// (dropped.go): the dataset's routines and models go as a deletion takes
// them, and every deletion the front kept of the project, with its copy,
// is forgotten. CloudBurrow's project reset sends it, and deletes
// droppedDatasetID last.

// PurgeHeader marks a datasets.delete as a project's purge.
const PurgeHeader = "X-Cloudburrow-Purge"

// DroppedDatasetID is the front's dataset of the copies of deleted tables,
// which datasets.list leaves out and a purge deletes.
const DroppedDatasetID = droppedDatasetID

// deleteDatasetPurging serves a purge's datasets.delete.
func (f front) deleteDatasetPurging(w http.ResponseWriter, r *http.Request, dataset string) {
	project := projectOf(f.base)
	d := &droppedDataset{Project: project, Dataset: dataset}
	if dataset != droppedDatasetID {
		f.keepRoutines(r, d) // what dropRoutines drops
	}
	sw := &statusWriter{ResponseWriter: w}
	f.next.ServeHTTP(sw, r)
	if sw.status/100 != 2 && sw.status != 0 {
		return
	}
	if dataset != droppedDatasetID {
		f.dropRoutines(r, d)
	}
	if f.dropped == nil {
		return
	}
	for _, c := range f.dropped.forgetProject(project) {
		f.send(r, http.MethodDelete, tablePath(droppedDatasetID, c), nil)
	}
}

// forgetProject takes out every deletion kept of project and returns the
// copies to delete.
func (s *droppedStore) forgetProject(project string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var copies []string
	before := len(s.s.Tables) + len(s.s.Datasets)
	tables := s.s.Tables[:0]
	for _, t := range s.s.Tables {
		if t.Project == project {
			if t.Copy != "" {
				copies = append(copies, t.Copy)
			}
			continue
		}
		tables = append(tables, t)
	}
	s.s.Tables = tables
	datasets := s.s.Datasets[:0]
	for _, d := range s.s.Datasets {
		if d.Project != project {
			datasets = append(datasets, d)
		}
	}
	s.s.Datasets = datasets
	if len(s.s.Tables)+len(s.s.Datasets) != before {
		s.saveLocked()
	}
	return copies
}
