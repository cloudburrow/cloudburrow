package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/spanner"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Spanner Studio's read-write mode (#798).
//
// The editor reads by default: every statement runs in a read-only snapshot,
// as it always has. Switching it to Read-write sends the statement with Mode
// "read-write", which reaches Write below and nothing else, and Write runs
// exactly one INSERT, UPDATE or DELETE in a read-write transaction through the
// official client, which commits it. What each mode accepts is decided by
// classifySpannerStatement, so a DELETE never reaches the read-only path's
// Spanner call and a SELECT is never wrapped in a write transaction.

// maxSpannerDMLBytes bounds a read-write statement. Chosen, not measured: well
// past any statement typed into an editor, and far under the query route's
// 64 KiB request body, so the refusal names this limit rather than the body's.
const maxSpannerDMLBytes = 16 << 10

type spannerStatementKind int

const (
	// spannerOther is a statement the classifier does not recognise. The
	// read-only path sends it, and the read-only transaction decides; the
	// read-write path refuses it.
	spannerOther spannerStatementKind = iota
	spannerQuery
	spannerDML
	spannerDDL
)

// spannerStatement is what classifySpannerStatement found.
type spannerStatement struct {
	// Kinds and Keywords are each statement's, in order, one per statement
	// the text holds once semicolons outside quotes and comments split it.
	Kinds    []spannerStatementKind
	Keywords []string
}

// classifySpannerStatement names what each statement in sql is, by its first
// keyword.
//
// It uses the GoogleSQL tokenizer the BigQuery editor uses, so a keyword or a
// semicolon inside a string literal, a quoted identifier or a comment does not
// count. A leading statement hint, "@{LOCK_SCANNED_RANGES=exclusive} UPDATE",
// is stepped over, and so are leading parentheses, "(SELECT 1)".
func classifySpannerStatement(sql string) (spannerStatement, error) {
	tokens, err := bigQueryTokens(sql)
	if err != nil {
		return spannerStatement{}, err
	}
	var out spannerStatement
	add := func(stmt []string) {
		if len(stmt) == 0 {
			return
		}
		kw := firstSpannerKeyword(stmt)
		out.Keywords = append(out.Keywords, kw)
		out.Kinds = append(out.Kinds, spannerKindOf(kw))
	}
	var cur []string
	for _, tok := range tokens {
		if tok == ";" {
			add(cur)
			cur = nil
			continue
		}
		cur = append(cur, tok)
	}
	add(cur)
	if len(out.Kinds) == 0 {
		return spannerStatement{}, errors.New("a statement is required")
	}
	return out, nil
}

func firstSpannerKeyword(tokens []string) string {
	i := 0
	if len(tokens) > 1 && tokens[0] == "@" && tokens[1] == "{" {
		for i < len(tokens) && tokens[i] != "}" {
			i++
		}
		i++
	}
	for i < len(tokens) && tokens[i] == "(" {
		i++
	}
	if i >= len(tokens) {
		return ""
	}
	return strings.ToUpper(tokens[i])
}

func spannerKindOf(keyword string) spannerStatementKind {
	switch keyword {
	case "SELECT", "WITH", "GRAPH", "FROM":
		return spannerQuery
	case "INSERT", "UPDATE", "DELETE":
		return spannerDML
	case "CREATE", "ALTER", "DROP", "RENAME", "GRANT", "REVOKE", "ANALYZE":
		return spannerDDL
	}
	return spannerOther
}

// ddlRefusal is the one message for DDL, in either mode.
func ddlRefusal(keyword string) error {
	return fmt.Errorf("%s is DDL, a schema change, which the query editor does not run: "+
		"a new database's first table is set in Create database", keyword)
}

// readOnlySpanner refuses, before anything is sent, a statement the read-only
// editor recognises as a write: DML with the switch that runs it, DDL with
// where schema is set instead. Anything else goes to the read-only
// transaction, which refuses what the classifier did not recognise.
func readOnlySpanner(sql string) error {
	st, err := classifySpannerStatement(sql)
	if err != nil {
		// Not the classifier's to refuse: an unterminated literal is a syntax
		// error, and Spanner's own message says where it is.
		return nil
	}
	for i, kind := range st.Kinds {
		switch kind {
		case spannerDML:
			return fmt.Errorf("%s changes data, and the editor is read-only: "+
				"switch it to Read-write to run it in a read-write transaction", st.Keywords[i])
		case spannerDDL:
			return ddlRefusal(st.Keywords[i])
		}
	}
	return nil
}

// writableSpanner accepts exactly one INSERT, UPDATE or DELETE and returns it
// without its trailing semicolons.
//
// One, because batch DML (ExecuteBatchDml) is not verified in CloudBurrow
// (docs/coverage/spanner.md), so running several statements would mean either
// an unverified call or several transactions where the user wrote one.
func writableSpanner(sql string) (string, error) {
	sql = strings.TrimSpace(sql)
	if len(sql) > maxSpannerDMLBytes {
		return "", fmt.Errorf("the statement is %d bytes; the read-write editor accepts at most %d",
			len(sql), maxSpannerDMLBytes)
	}
	st, err := classifySpannerStatement(sql)
	if err != nil {
		return "", err
	}
	if len(st.Kinds) > 1 {
		return "", fmt.Errorf("the read-write editor runs one DML statement at a time, and this is %d: "+
			"remove everything after the first semicolon (batch DML is not offered)", len(st.Kinds))
	}
	switch st.Kinds[0] {
	case spannerDML:
	case spannerDDL:
		return "", ddlRefusal(st.Keywords[0])
	case spannerQuery:
		return "", fmt.Errorf("%s reads data: switch the editor to Read-only, "+
			"where it runs in a read-only transaction", st.Keywords[0])
	default:
		kw := st.Keywords[0]
		if kw == "" {
			kw = "this statement"
		}
		return "", fmt.Errorf("only an INSERT, UPDATE or DELETE statement runs in Read-write, not %s", kw)
	}
	// Outside quotes and comments the tokenizer found at most one statement,
	// so what follows the last non-space byte, if it is semicolons, is the
	// terminator and not part of the statement.
	return strings.TrimRight(sql, "; \t\r\n"), nil
}

// WriteSpec implements console.StatementWriter: a database's page, and its
// tables' pages, which query the same database. An instance holds no rows.
func (spannerProvider) WriteSpec(path []string) *console.WriteSpec {
	if len(path) < 2 {
		return nil
	}
	return &console.WriteSpec{
		Label: "Read-write",
		Hint: "Writes data. One INSERT, UPDATE or DELETE runs in a read-write " +
			"transaction and is committed; the result is the number of rows it changed. " +
			"DDL and several statements at once are refused.",
		Target: fmt.Sprintf("database %s in instance %s", path[1], path[0]),
	}
}

// Write implements console.StatementWriter: one DML statement, committed.
func (p spannerProvider) Write(ctx context.Context, project string, path []string, statement string) (int64, error) {
	if project == "" {
		return 0, errors.New("choose a project first")
	}
	if len(path) < 2 {
		return 0, errors.New("a Spanner write runs against a database: open one from its instance")
	}
	sql, err := writableSpanner(statement)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, path[0], path[1])
	c, err := spanner.NewClient(ctx, dbName, localOpts(p.endpoint)...)
	if err != nil {
		return 0, fmt.Errorf("cannot open the database: %w", err)
	}
	defer c.Close()

	var rows int64
	_, err = c.ReadWriteTransaction(ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
		// Set on every attempt: the client retries the function when Spanner
		// aborts the transaction, and only the committed attempt's count is
		// the answer.
		n, err := txn.Update(ctx, spanner.Statement{SQL: sql})
		rows = n
		return err
	})
	if err != nil {
		// Spanner's own message: a constraint violation names the row.
		return 0, err
	}
	return rows, nil
}
