package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Change history retention (#1503).
//
// BigQuery: "The data returned by the CHANGES [APPENDS] function is
// limited to the time travel window of the table", seven days by default
// and as short as two, the dataset's max_time_travel_hours
// (https://cloud.google.com/bigquery/docs/time-travel); a lengthened window
// does not bring back what the shorter one let go ("tables deleted before
// that change are still only recoverable for two days"). The engine keeps
// seven days of every change (googlesqlite patch 0036). So the front lets
// go of what is older than the dataset's window: when a read of the
// history (CHANGES, APPENDS) finds the window, and, with the window it
// had, before a datasets.patch or datasets.update changes it, so that
// a longer window after a shorter one reads nothing from before.
//
// What is let go are the changes older than the window, but for the row
// each of them leaves in the table at the window's start: time travel
// (timetravel.go) reads a row's last change at or before a time, so a
// row inserted before the window and still there keeps its last change,
// made a CARRY (changehistorycarry.go): in the table from then on, and no
// change APPENDS or CHANGES reads. A row deleted before the window, and
// the START row's time (the history's start), are kept so.

// pruneHistory lets go of the changes of dataset.table older than earliest,
// microseconds since the epoch (above), or says why it could not.
func (f front) pruneHistory(r *http.Request, dataset, table string, earliest int64) string {
	changes := quotePath([]string{projectOf(f.base), dataset, table + changeTableSuffix})
	old := fmt.Sprintf("googlesqlite_change_micros < %d AND _CHANGE_TYPE != 'START'", earliest)
	if n, ok := f.scalarInt(r, "SELECT COUNTIF(_CHANGE_TYPE != '"+changeCarry+"') FROM "+changes+" WHERE "+old); !ok || n == 0 {
		return ""
	}
	for _, q := range []string{
		// The last change of each row before earliest stays, unless it
		// deleted the row; every other change before it goes.
		fmt.Sprintf("DELETE FROM %s WHERE %s AND rowid NOT IN (SELECT cloudburrow_order FROM (SELECT rowid AS cloudburrow_order, "+
			"_CHANGE_TYPE AS cloudburrow_change, ROW_NUMBER() OVER (PARTITION BY googlesqlite_change_rowid ORDER BY rowid DESC) AS cloudburrow_last "+
			"FROM %s WHERE %s) WHERE cloudburrow_last = 1 AND cloudburrow_change NOT IN ('DELETE', '%s'))", changes, old, changes, old, changeEnd),
		fmt.Sprintf("UPDATE %s SET _CHANGE_TYPE = '%s', _CHANGE_IS_FOR_UPDATE = FALSE WHERE %s AND _CHANGE_TYPE != '%s'",
			changes, changeCarry, old, changeCarry),
	} {
		if msg := f.runDML(r, q); msg != "" {
			return msg
		}
	}
	return ""
}

// pruneDataset lets go of the history older than hours before now of
// every table of dataset (above).
func (f front) pruneDataset(r *http.Request, dataset string, hours int64) {
	now, ok := f.scalarInt(r, "SELECT UNIX_MICROS(CURRENT_TIMESTAMP())")
	if !ok {
		return
	}
	earliest := now - hours*int64(time.Hour/time.Microsecond)
	token := ""
	for {
		path := "/datasets/" + url.PathEscape(dataset) + "/tables?maxResults=1000"
		if token != "" {
			path += "&pageToken=" + url.QueryEscape(token)
		}
		st, got := f.get(r, path)
		var list struct {
			Tables []struct {
				Type           string `json:"type"`
				TableReference struct {
					TableID string `json:"tableId"`
				} `json:"tableReference"`
			} `json:"tables"`
			NextPageToken string `json:"nextPageToken"`
		}
		if st != http.StatusOK || json.Unmarshal(got, &list) != nil {
			return
		}
		for _, t := range list.Tables {
			if t.Type == "" || t.Type == "TABLE" {
				// One that cannot be let go of is still read only
				// within the window (changeWindow).
				_ = f.pruneHistory(r, dataset, t.TableReference.TableID, earliest)
			}
		}
		if list.NextPageToken == "" || list.NextPageToken == token {
			return
		}
		token = list.NextPageToken
	}
}
