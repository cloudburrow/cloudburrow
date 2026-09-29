package bigqueryfront

// The client's texts of the views' queries (viewTexts, #1014), kept in a
// file on the front's state directory, beside the job texts (#1028).
//
// They were in the front's memory alone, so after a restart of the front's
// container, the emulator still running with the views, tables.get of a
// view made by CREATE VIEW answered the emulator's rewritten SQL again.
// Kept here, they outlive a restart of the container and go with the pod,
// as the emulator's views do; the emulator's restart (reset) empties the
// file too.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// viewTextsStateFile is the file the view texts are kept in, in dir.
func viewTextsStateFile(dir string) string { return filepath.Join(dir, "viewtexts.json") }

// savedViewText is a viewText as the file keeps it.
type savedViewText struct {
	Key    string `json:"key"`
	Engine string `json:"engine"`
	Client string `json:"client"`
}

// keep restores the view texts from path, when it holds them, and from then
// on keeps them there. It must be called before the front serves. A file
// that cannot be read is logged and replaced.
func (v *viewTexts) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved []savedViewText
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the view texts it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the view texts it kept", path)
		saved = nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, s := range saved {
		if s.Key == "" {
			continue
		}
		if v.texts == nil {
			v.texts = map[string]viewText{}
		}
		if _, ok := v.texts[s.Key]; !ok {
			v.order = append(v.order, s.Key)
		}
		v.texts[s.Key] = viewText{engine: s.Engine, client: s.Client}
	}
	v.trimLocked()
	if len(saved) > 0 {
		logf("bigquery front: restored %d view texts from %s", len(v.order), path)
	}
	v.path, v.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	v.saveLocked()
}

// saveLocked writes the view texts, when they are kept; v.mu is held.
func (v *viewTexts) saveLocked() {
	if v == nil || v.path == "" {
		return
	}
	saved := make([]savedViewText, 0, len(v.order))
	for _, k := range v.order {
		t := v.texts[k]
		saved = append(saved, savedViewText{Key: k, Engine: t.engine, Client: t.client})
	}
	b, err := json.Marshal(saved)
	if err == nil {
		err = writeFileAtomic(v.path, b)
	}
	if err != nil && v.logf != nil {
		v.logf("bigquery front: keeping the view texts in %s: %v", v.path, err)
	}
}
