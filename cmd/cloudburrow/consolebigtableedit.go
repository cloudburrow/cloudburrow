package main

// Bigtable column families and row writes (#797).
//
// BigtableTableAdmin/ModifyColumnFamilies and Bigtable/MutateRow are verified
// with the official clients, and a table's page could create and delete the
// table and nothing inside it. Every change here goes through the same
// official clients the page reads with, against the forwarded emulator port:
// a family through AdminClient (CreateColumnFamilyWithConfig, UpdateFamily
// via SetGCPolicy, DeleteColumnFamily), a cell or a row through Table.Apply.
// A refusal is the emulator's own message.
//
// Where each action lives:
//
//   - a table's page: Add column family, and Write cell once a family exists;
//   - a family's row in the Schema section: Edit GC policy and Delete column
//     family, addressed as [table, "families", family];
//   - a row's page: Write cell and Delete row;
//   - a cell's row on the row's page: Delete cells, addressed as
//     [table, row key, family:qualifier].
//
// Since #1198: a table's page offers Delete rows by prefix and Delete all
// rows (DropRowRange) and Edit deletion protection (UpdateTable); a cell
// offers Increment and Append value (ReadModifyWriteRow); Delete row and
// Delete cells are one CheckAndMutateRow each, applied only when the row or
// the column holds a cell.
//
// Row pages are [table, row key], so a family cannot be addressed as
// [table, family]: a row key can be any string, including a family's name.
// Three segments are unambiguous, because a cell's third segment is always
// "family:qualifier" and a family name cannot hold a colon.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"cloud.google.com/go/bigtable"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bigtableFamiliesSegment is the second segment of a column family's action
// path.
const bigtableFamiliesSegment = "families"

// bigtableFamilyPattern is Bigtable's own rule for a column family ID.
const bigtableFamilyPattern = `^[\-_.a-zA-Z0-9]+$`

// maxAgePattern is a whole number of days, hours or minutes: the units the
// client writes a max age back in (MaxAgeGCPolicy.GetDurationString), so an
// edit form prefilled with one saves the same age.
var maxAgePattern = regexp.MustCompile(`^([1-9][0-9]*)([dhm])$`)

// bigtableGCFields are the inputs of Add column family and Edit GC policy.
//
// A GC policy on the Google console is a max number of versions, a max age,
// or both, deleted when either or both are exceeded. That is exactly what the
// form offers; a nested policy an application set through the API is shown
// and offered no edit (formatGCPolicy).
func bigtableGCFields(versions, age string, both bool) []console.Field {
	return []console.Field{
		{Name: "maxVersions", Label: "Maximum versions", Type: "text", Default: versions,
			Pattern: `^[1-9][0-9]{0,8}$`, Section: "Garbage collection",
			Help: "Keep at most this many versions of each cell. Empty for no limit."},
		{Name: "maxAge", Label: "Maximum age", Type: "text", Default: age,
			Pattern: `^[1-9][0-9]{0,8}[dhm]$`, Section: "Garbage collection",
			Help: "Delete cells older than this: a whole number of days, hours or minutes, " +
				"such as 7d, 12h or 30m. Empty for no limit. With neither limit, cells never expire."},
		{Name: "both", Label: "Only when both limits are exceeded", Type: "checkbox",
			Default: strconv.FormatBool(both), Section: "Garbage collection",
			Help: "With both limits set, a cell is deleted when either is exceeded, " +
				"or with this checked only when both are."},
	}
}

// bigtableAddFamilyFields are Add column family's inputs.
func bigtableAddFamilyFields() []console.Field {
	return append([]console.Field{{
		Name: "family", Label: "Column family ID", Type: "text", Required: true,
		Pattern: bigtableFamilyPattern,
		Help:    "Letters, digits, hyphens, underscores and periods.",
	}}, bigtableGCFields("", "", false)...)
}

// parseGCPolicy reads the GC fields into a policy. Neither limit is no policy:
// cells are kept for ever.
func parseGCPolicy(values map[string]string) (bigtable.GCPolicy, error) {
	var parts []bigtable.GCPolicy
	if raw := strings.TrimSpace(values["maxVersions"]); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("maximum versions %q is not a whole number of at least 1", raw)
		}
		parts = append(parts, bigtable.MaxVersionsPolicy(n))
	}
	if raw := strings.TrimSpace(values["maxAge"]); raw != "" {
		d, err := parseMaxAge(raw)
		if err != nil {
			return nil, err
		}
		parts = append(parts, bigtable.MaxAgePolicy(d))
	}
	switch {
	case len(parts) == 0:
		return bigtable.NoGcPolicy(), nil
	case len(parts) == 1:
		return parts[0], nil
	case values["both"] == "true":
		return bigtable.IntersectionPolicy(parts...), nil
	default:
		return bigtable.UnionPolicy(parts...), nil
	}
}

func parseMaxAge(raw string) (time.Duration, error) {
	m := maxAgePattern.FindStringSubmatch(raw)
	if m == nil {
		return 0, fmt.Errorf("maximum age %q is not a whole number of days, hours or minutes, such as 7d, 12h or 30m", raw)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("maximum age %q: %w", raw, err)
	}
	unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}[m[2]]
	return time.Duration(n) * unit, nil
}

// formatGCPolicy is a stored policy as the GC fields hold it. ok is false for
// a policy the form cannot express — nested, or an age that is not a whole
// number of minutes — which is shown and offered no edit rather than an edit
// that would save a different policy.
func formatGCPolicy(p bigtable.GCPolicy) (versions, age string, both, ok bool) {
	one := func(p bigtable.GCPolicy) bool {
		switch v := p.(type) {
		case bigtable.MaxVersionsGCPolicy:
			if versions != "" {
				return false
			}
			versions = strconv.Itoa(int(v))
			return true
		case bigtable.MaxAgeGCPolicy:
			s := v.GetDurationString()
			if age != "" || !maxAgePattern.MatchString(s) {
				return false
			}
			age = s
			return true
		}
		return false
	}
	switch v := p.(type) {
	case nil:
		return "", "", false, true
	case bigtable.UnionGCPolicy, bigtable.IntersectionGCPolicy:
		var children []bigtable.GCPolicy
		if u, isUnion := v.(bigtable.UnionGCPolicy); isUnion {
			children = u.Children
		} else {
			children, both = v.(bigtable.IntersectionGCPolicy).Children, true
		}
		if len(children) != 2 {
			return "", "", false, false
		}
		for _, c := range children {
			if !one(c) {
				return "", "", false, false
			}
		}
		return versions, age, both, true
	}
	if p.String() == "" {
		// NoGcPolicy.
		return "", "", false, true
	}
	if !one(p) {
		return "", "", false, false
	}
	return versions, age, false, true
}

// bigtableWriteCellFields are Write cell's inputs. withRow is true on a
// table's page, where the row is named; a row's page writes to its own.
func bigtableWriteCellFields(withRow bool, family string) []console.Field {
	var fields []console.Field
	if withRow {
		fields = append(fields, console.Field{Name: "row", Label: "Row key", Type: "text", Required: true,
			Help: "An existing row, or a new one: a row exists once it has a cell."})
	}
	return append(fields,
		console.Field{Name: "family", Label: "Column family", Type: "text", Required: true, Default: family,
			Pattern: bigtableFamilyPattern, Help: "One of the table's column families, on its Schema tab."},
		console.Field{Name: "qualifier", Label: "Column qualifier", Type: "text",
			Help: "The column within the family. May be empty."},
		console.Field{Name: "value", Label: "Value", Type: "textarea",
			Help: "Stored as the bytes of this text."},
		console.Field{Name: "timestamp", Label: "Timestamp", Type: "text",
			Help: "RFC 3339, such as 2026-01-02T15:04:05.123Z, at millisecond precision: Bigtable " +
				"refuses finer. Empty for now. A cell written at an existing timestamp replaces that version."},
	)
}

// bigtableFamilies reads a table's column families through the admin client.
func (p bigtableProvider) bigtableFamilies(ctx context.Context, project, table string) ([]bigtable.FamilyInfo, error) {
	admin, err := p.newAdmin(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Bigtable admin API: %w", err)
	}
	defer admin.Close()
	info, err := admin.TableInfo(ctx, table)
	if err != nil {
		return nil, err
	}
	return info.FamilyInfos, nil
}

// familyActions are a family row's actions in the Schema section, the edit
// prefilled from its stored policy.
func familyActions(f bigtable.FamilyInfo) []console.Action {
	var out []console.Action
	if versions, age, both, ok := formatGCPolicy(f.FullGCPolicy); ok {
		out = append(out, console.Action{ID: "editgc", Label: "Edit GC policy",
			Fields: bigtableGCFields(versions, age, both)})
	}
	return append(out, console.Action{ID: "deletefamily", Label: "Delete column family", Destructive: true})
}

// familyTarget is the path a family row's actions address.
func familyTarget(table, family string) []string {
	return []string{table, bigtableFamiliesSegment, family}
}

// splitColumn reads a cell's "family:qualifier".
func splitColumn(column string) (family, qualifier string, ok bool) {
	family, qualifier, ok = strings.Cut(column, ":")
	return family, qualifier, ok && family != ""
}

// DetailActions implements console.PathActor.
func (p bigtableProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if project == "" {
		return nil
	}
	// A write names a family, so a table without one is offered no write: it
	// could only be refused.
	writeCell := func(withRow bool) []console.Action {
		ctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		families, err := p.bigtableFamilies(ctx, project, path[0])
		if err != nil || len(families) == 0 {
			return nil
		}
		return []console.Action{{ID: "writecell", Label: "Write cell",
			Fields: bigtableWriteCellFields(withRow, firstFamily(families))}}
	}
	switch len(path) {
	case 1:
		out := append([]console.Action{{ID: "addfamily", Label: "Add column family", Fields: bigtableAddFamilyFields()}},
			writeCell(true)...)
		return append(out, p.tableActions(ctx, project, path[0])...)
	case 2:
		return append(writeCell(false),
			console.Action{ID: "deleterow", Label: "Delete row", Destructive: true, Leaves: true})
	case 3:
		if _, _, isCell := splitColumn(path[2]); isCell {
			return cellActions()
		}
		if path[1] != bigtableFamiliesSegment {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, dbTimeout)
		defer cancel()
		families, err := p.bigtableFamilies(ctx, project, path[0])
		if err != nil {
			return nil
		}
		for _, f := range families {
			if f.Name == path[2] {
				return familyActions(f)
			}
		}
	}
	return nil
}

func firstFamily(families []bigtable.FamilyInfo) string {
	first := ""
	for _, f := range families {
		if first == "" || f.Name < first {
			first = f.Name
		}
	}
	return first
}

// ActAt implements console.PathActor.
func (p bigtableProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	switch {
	case action == "addfamily" && len(path) == 1:
		family := strings.TrimSpace(values["family"])
		if family == "" {
			return errors.New("a column family ID is required")
		}
		policy, err := parseGCPolicy(values)
		if err != nil {
			return err
		}
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			config := bigtable.Family{}
			if policy.String() != "" {
				config.GCPolicy = policy
			}
			return a.CreateColumnFamilyWithConfig(ctx, path[0], family, config)
		})
	case action == "editgc" && len(path) == 3:
		policy, err := parseGCPolicy(values)
		if err != nil {
			return err
		}
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.SetGCPolicy(ctx, path[0], path[2], policy)
		})
	case action == "deletefamily" && len(path) == 3:
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.DeleteColumnFamily(ctx, path[0], path[2])
		})
	case action == "writecell" && (len(path) == 1 || len(path) == 2):
		row := ""
		if len(path) == 2 {
			row = path[1]
		} else {
			row = values["row"]
		}
		if row == "" {
			return errors.New("a row key is required")
		}
		family := strings.TrimSpace(values["family"])
		if family == "" {
			return errors.New("a column family is required")
		}
		ts := bigtable.Now()
		if raw := strings.TrimSpace(values["timestamp"]); raw != "" {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return fmt.Errorf("timestamp %q is not RFC 3339, such as 2026-01-02T15:04:05.123Z", raw)
			}
			// Bigtable refuses a timestamp it was given at a finer precision
			// than its table's milliseconds; some emulator builds truncate it
			// instead, which would store a different time than the one typed.
			if t.Nanosecond()%int(time.Millisecond) != 0 {
				return fmt.Errorf("timestamp %q is finer than a millisecond, which Bigtable refuses", raw)
			}
			ts = bigtable.Time(t)
		}
		mut := bigtable.NewMutation()
		mut.Set(family, values["qualifier"], ts, []byte(values["value"]))
		return p.apply(ctx, project, path[0], row, mut)
	case action == "deleterow" && len(path) == 2:
		mut := bigtable.NewMutation()
		mut.DeleteRow()
		return p.applyIf(ctx, project, path[0], path[1], bigtable.StripValueFilter(), mut,
			fmt.Sprintf("no row with key %s in %s", path[1], path[0]))
	case action == "deletecells" && len(path) == 3:
		family, qualifier, ok := splitColumn(path[2])
		if !ok {
			return fmt.Errorf("%q is not a family:qualifier column", path[2])
		}
		mut := bigtable.NewMutation()
		mut.DeleteCellsInColumn(family, qualifier)
		return p.applyIf(ctx, project, path[0], path[1], columnFilter(family, qualifier), mut,
			fmt.Sprintf("row %s in %s has no cells in %s", path[1], path[0], path[2]))
	case (action == "increment" || action == "append") && len(path) == 3:
		return p.readModifyWrite(ctx, project, path, action, values)
	case action == "deleteprefix" && len(path) == 1:
		prefix := values["prefix"]
		if prefix == "" {
			return errors.New("a row key prefix is required; Delete all rows deletes every row")
		}
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.DropRowRange(ctx, path[0], prefix)
		})
	case action == "deleteallrows" && len(path) == 1:
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.DropAllRows(ctx, path[0])
		})
	case action == "backup" && len(path) == 1:
		days, err := strconv.Atoi(strings.TrimSpace(values["days"]))
		if err != nil || days < 1 || days > 90 {
			return fmt.Errorf("expire after %q days: want a whole number of days from 1 to 90", values["days"])
		}
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.CreateBackup(ctx, path[0], strings.TrimSpace(values["cluster"]), strings.TrimSpace(values["backup"]), time.Now().Add(time.Duration(days)*24*time.Hour))
		})
	case action == "restore" && len(path) == 1:
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.RestoreTable(ctx, strings.TrimSpace(values["table"]), strings.TrimSpace(values["cluster"]), strings.TrimSpace(values["backup"]))
		})
	case action == "authview" && len(path) == 1:
		sv, err := parseSubsetView(values["prefixes"], values["columns"])
		if err != nil {
			return err
		}
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.CreateAuthorizedView(ctx, &bigtable.AuthorizedViewConf{TableID: path[0], AuthorizedViewID: strings.TrimSpace(values["view"]), AuthorizedView: sv})
		})
	case action == "protection" && len(path) == 1:
		protection := bigtable.Unprotected
		if values["protected"] == "true" {
			protection = bigtable.Protected
		}
		return p.admin(ctx, project, func(a *bigtable.AdminClient) error {
			return a.UpdateTableWithDeletionProtection(ctx, path[0], protection)
		})
	}
	return fmt.Errorf("unknown action %q", action)
}

// bigtableBackupCluster is the cluster the console's backup forms offer.
const bigtableBackupCluster = "cloudburrow-c1"

// parseSubsetView reads an authorized view's subset from the console's
// comma-separated row prefixes and family:qualifier columns.
func parseSubsetView(prefixes, columns string) (*bigtable.SubsetViewConf, error) {
	sv := &bigtable.SubsetViewConf{}
	if strings.TrimSpace(prefixes) == "" {
		sv.AddRowPrefix([]byte(""))
	}
	for _, p := range strings.Split(prefixes, ",") {
		if p = strings.TrimSpace(p); p != "" {
			sv.AddRowPrefix([]byte(p))
		}
	}
	for _, c := range strings.Split(columns, ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		family, qualifier, ok := strings.Cut(c, ":")
		if !ok || family == "" {
			return nil, fmt.Errorf("column %q: want family:qualifier", c)
		}
		if q, isPrefix := strings.CutSuffix(qualifier, "*"); isPrefix {
			sv.AddFamilySubsetQualifierPrefix(family, []byte(q))
		} else {
			sv.AddFamilySubsetQualifier(family, []byte(qualifier))
		}
	}
	if len(sv.FamilySubsets) == 0 {
		return nil, errors.New("an authorized view needs at least one column")
	}
	return sv, nil
}

// admin runs one call with an admin client.
func (p bigtableProvider) admin(ctx context.Context, project string, call func(*bigtable.AdminClient) error) error {
	a, err := p.newAdmin(ctx, project)
	if err != nil {
		return fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer a.Close()
	return call(a)
}

// apply sends one MutateRow.
func (p bigtableProvider) apply(ctx context.Context, project, table, row string, mut *bigtable.Mutation) error {
	c, err := p.newData(ctx, project)
	if err != nil {
		return fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer c.Close()
	return c.Open(table).Apply(ctx, row, mut)
}

// applyIf sends one CheckAndMutateRow: mut is applied when the row holds a
// cell that predicate passes, in the same call, and refused with missing
// otherwise. Bigtable deletes a missing row or column without complaint,
// and a delete that reports success for nothing reads as a delete that
// worked; a read first and a delete after could also delete what another
// writer wrote between the two.
func (p bigtableProvider) applyIf(ctx context.Context, project, table, row string, predicate bigtable.Filter, mut *bigtable.Mutation, missing string) error {
	c, err := p.newData(ctx, project)
	if err != nil {
		return fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer c.Close()
	var matched bool
	if err := c.Open(table).Apply(ctx, row, bigtable.NewCondMutation(predicate, mut, nil),
		bigtable.GetCondMutationResult(&matched)); err != nil {
		return err
	}
	if !matched {
		return errors.New(missing)
	}
	return nil
}

// columnFilter passes the cells of one column, family and qualifier
// matched exactly: both filters are RE2 patterns that match the whole name.
func columnFilter(family, qualifier string) bigtable.Filter {
	return bigtable.ChainFilters(bigtable.FamilyFilter(regexp.QuoteMeta(family)),
		bigtable.ColumnFilter(regexp.QuoteMeta(qualifier)), bigtable.StripValueFilter())
}

// cellActions are a cell's actions on its row's page. Increment and Append
// value are Bigtable's ReadModifyWriteRow, which reads and writes the
// latest version of the cell in one atomic call.
func cellActions() []console.Action {
	return []console.Action{
		{ID: "increment", Label: "Increment", Fields: []console.Field{{
			Name: "amount", Label: "Amount", Type: "text", Required: true, Default: "1",
			Pattern: `^-?[0-9]{1,19}$`,
			Help: "A whole number, negative to decrease. Bigtable adds it to the latest value, " +
				"which must be a 64-bit big-endian integer (8 bytes), and writes the sum as a new version."}}},
		{ID: "append", Label: "Append value", Fields: []console.Field{{
			Name: "value", Label: "Value", Type: "textarea", Required: true,
			Help: "Bigtable appends the bytes of this text to the latest value and writes the result as a new version."}}},
		{ID: "deletecells", Label: "Delete cells", Destructive: true},
	}
}

// readModifyWrite sends Increment or Append value to one cell.
func (p bigtableProvider) readModifyWrite(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	family, qualifier, ok := splitColumn(path[2])
	if !ok {
		return fmt.Errorf("%q is not a family:qualifier column", path[2])
	}
	m := bigtable.NewReadModifyWrite()
	if action == "increment" {
		raw := strings.TrimSpace(values["amount"])
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("amount %q is not a whole number that fits in 64 bits", raw)
		}
		m.Increment(family, qualifier, n)
	} else {
		if values["value"] == "" {
			return errors.New("a value to append is required")
		}
		m.AppendValue(family, qualifier, []byte(values["value"]))
	}
	c, err := p.newData(ctx, project)
	if err != nil {
		return fmt.Errorf("cannot reach Bigtable: %w", err)
	}
	defer c.Close()
	_, err = c.Open(path[0]).ApplyReadModifyWrite(ctx, path[1], m)
	return err
}

// tableActions are a table page's own actions: Delete rows by prefix and
// Delete all rows (DropRowRange), and Edit deletion protection
// (UpdateTable), prefilled from the table as it reads.
func (p bigtableProvider) tableActions(ctx context.Context, project, table string) []console.Action {
	out := []console.Action{
		// Confirm: a destructive action with a form is otherwise sent on
		// submit, and the rows it deletes cannot be restored, so it asks for
		// the table's name back as Delete all rows does.
		{ID: "deleteprefix", Label: "Delete rows by prefix", Destructive: true,
			Confirm: "Every row whose key starts with the prefix is deleted, and cannot be restored; the column families are kept.",
			Fields: []console.Field{{
				Name: "prefix", Label: "Row key prefix", Type: "text", Required: true,
				Help: "Every row whose key starts with this is deleted."}}},
		{ID: "deleteallrows", Label: "Delete all rows", Destructive: true},
		// Backups and authorized views (#1499), which the Bigtable front
		// keeps beside the emulator's tables.
		{ID: "backup", Label: "Create backup", Fields: []console.Field{
			{Name: "cluster", Label: "Cluster ID", Type: "text", Required: true, Default: bigtableBackupCluster},
			{Name: "backup", Label: "Backup ID", Type: "text", Required: true},
			{Name: "days", Label: "Expire after (days)", Type: "text", Default: "7", Help: "Between 1 and 90 days."}}},
		{ID: "restore", Label: "Restore backup to a new table", Fields: []console.Field{
			{Name: "cluster", Label: "Cluster ID", Type: "text", Required: true, Default: bigtableBackupCluster},
			{Name: "backup", Label: "Backup ID", Type: "text", Required: true},
			{Name: "table", Label: "New table ID", Type: "text", Required: true}}},
		{ID: "authview", Label: "Create authorized view", Fields: []console.Field{
			{Name: "view", Label: "Authorized view ID", Type: "text", Required: true},
			{Name: "prefixes", Label: "Row key prefixes", Type: "text", Help: "Comma-separated; empty for every row."},
			{Name: "columns", Label: "Columns", Type: "text", Required: true,
				Help: "Comma-separated family:qualifier, or family:prefix* for a qualifier prefix, family:* for a whole family."}}},
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var info *bigtable.TableInfo
	if err := p.admin(ctx, project, func(a *bigtable.AdminClient) (err error) {
		info, err = a.TableInfo(ctx, table)
		return err
	}); err != nil {
		return out
	}
	return append(out, console.Action{ID: "protection", Label: "Edit deletion protection", Fields: []console.Field{{
		Name: "protected", Label: "Deletion protection", Type: "checkbox",
		Default: strconv.FormatBool(info.DeletionProtection == bigtable.Protected),
		Help:    "While it is on, Bigtable refuses to delete the table."}}})
}

var _ console.PathActor = bigtableProvider{}

// bigtableCellValue is how a cell's bytes read on the console (#1214).
//
// Bigtable stores bytes, not text. Valid UTF-8 made of printable characters
// (and whitespace) shows as that text; anything else shows as 0x-prefixed
// hex, so a binary value is never a run of unreadable characters. An 8-byte
// value also shows its big-endian int64, as Google's console does: that is
// the encoding ReadModifyWriteRow's Increment reads and writes.
func bigtableCellValue(v []byte) string {
	var s string
	if isPrintableText(v) {
		s = string(v)
	} else {
		s = "0x" + hex.EncodeToString(v)
	}
	if len(v) == 8 {
		s += fmt.Sprintf(" (int64: %d)", int64(binary.BigEndian.Uint64(v)))
	}
	return s
}

// isPrintableText reports whether v is valid UTF-8 with no control
// characters other than whitespace. An empty value is text.
func isPrintableText(v []byte) bool {
	if !utf8.Valid(v) {
		return false
	}
	for _, r := range string(v) {
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
