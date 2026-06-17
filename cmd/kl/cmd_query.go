package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davesade/kilolock/pkg/store"
)

// Postgres type OIDs we treat specially when formatting output.
// See https://www.postgresql.org/docs/current/datatype-oid.html and
// /usr/include/postgresql/server/catalog/pg_type_d.h.
const (
	oidJSON  = 114
	oidJSONB = 3802
)

func runQuery(args []string) int {
	if len(args) > 0 {
		switch strings.TrimSpace(args[0]) {
		case "resource":
			return runQueryResource(args[1:])
		case "resources":
			return runQueryResources(args[1:])
		case "history":
			return runQueryResourceHistory(args[1:])
		}
	}
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	file := fs.String("f", "", `read SQL from file ("-" for stdin)`)
	format := fs.String("format", "table", "output format: table | json | csv")
	timeout := fs.Duration("timeout", 30*time.Second, "statement timeout (e.g. 5s, 1m)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: kl query [flags] [SQL]

Runs a read-only SQL query against the Kilolock database. Queries
execute inside a transaction with SET TRANSACTION READ ONLY, so any
INSERT/UPDATE/DELETE/DDL is rejected by the server.

Backend-native query modes are also available:

  kl query resource [state] --address 'aws_instance.web'
  kl query resources [state] --address-glob 'module.*'
  kl query history [state] --address 'aws_instance.web'

The SQL is read in the following order, picking the first source
available:

  1. The flag value (e.g. --f path/to/query.sql, or --f - for stdin).
  2. A single positional argument.
  3. An error if neither is supplied.

Examples:
  kl query "SELECT name FROM states ORDER BY name"
  kl query -f docs/queries/inventory_by_type.sql --format csv
  echo "SELECT count(*) FROM resources" | kl query -f - --format json

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	sqlText, err := readQuerySource(*file, fs.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query:", err)
		fs.Usage()
		return 2
	}

	writer, err := newQueryWriter(*format, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query:", err)
		return 2
	}

	// Allow a small grace period beyond the statement timeout so the
	// "server-side timeout fired" error reaches us rather than the
	// context deadline canceling the connection.
	ctx, cancel := context.WithTimeout(cliContext(), *timeout+5*time.Second)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query:", err)
		return 1
	}
	var resp struct {
		Columns []store.ColumnInfo `json:"columns"`
		Rows    [][]string         `json:"rows"`
	}
	err = client.postJSON(ctx, "/admin/query", "", map[string]any{
		"sql":        sqlText,
		"timeout_ms": timeout.Milliseconds(),
	}, &resp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query:", err)
		return 1
	}
	if err := writer.OnColumns(resp.Columns); err != nil {
		fmt.Fprintln(os.Stderr, "kl query:", err)
		return 1
	}
	for _, row := range resp.Rows {
		values := make([]any, len(row))
		for i, v := range row {
			values[i] = v
		}
		if err := writer.OnRow(values); err != nil {
			fmt.Fprintln(os.Stderr, "kl query:", err)
			return 1
		}
	}
	if err := writer.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "kl query:", err)
		return 1
	}
	return 0
}

// readQuerySource resolves the SQL string from the given flag/positional
// arguments. Returns an error if no source is supplied or multiple
// positional arguments are present.
func readQuerySource(file string, positional []string) (string, error) {
	if file != "" {
		var src io.Reader
		if file == "-" {
			src = os.Stdin
		} else {
			f, err := os.Open(file)
			if err != nil {
				return "", fmt.Errorf("open %s: %w", file, err)
			}
			defer f.Close()
			src = f
		}
		b, err := io.ReadAll(src)
		if err != nil {
			return "", fmt.Errorf("read sql: %w", err)
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return "", errors.New("empty SQL input")
		}
		return s, nil
	}

	if len(positional) == 0 {
		return "", errors.New("no SQL supplied: pass it as an argument or use --f")
	}
	if len(positional) > 1 {
		return "", errors.New("expected exactly one SQL argument (quote it as a single string)")
	}
	s := strings.TrimSpace(positional[0])
	if s == "" {
		return "", errors.New("SQL argument is empty")
	}
	return s, nil
}

// queryWriter consumes the column metadata and per-row values from
// store.Query and renders them in some output format.
type queryWriter interface {
	OnColumns(cols []store.ColumnInfo) error
	OnRow(values []any) error
	Close() error
}

func newQueryWriter(format string, out io.Writer) (queryWriter, error) {
	switch format {
	case "table":
		return &tableQueryWriter{out: out, tw: tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)}, nil
	case "json":
		return &jsonQueryWriter{out: out, enc: json.NewEncoder(out), first: true}, nil
	case "csv":
		return &csvQueryWriter{out: out, w: csv.NewWriter(out)}, nil
	default:
		return nil, fmt.Errorf("unsupported format %q (expected table|json|csv)", format)
	}
}

// ---------------------------------------------------------------------------
// table format: buffered through tabwriter so columns align.
// ---------------------------------------------------------------------------

type tableQueryWriter struct {
	out      io.Writer
	tw       *tabwriter.Writer
	cols     []store.ColumnInfo
	rowCount int
}

func (w *tableQueryWriter) OnColumns(cols []store.ColumnInfo) error {
	w.cols = cols
	if len(cols) == 0 {
		return nil
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	if _, err := fmt.Fprintln(w.tw, strings.Join(names, "\t")); err != nil {
		return err
	}
	return nil
}

func (w *tableQueryWriter) OnRow(values []any) error {
	w.rowCount++
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = formatScalarForText(v, w.cols[i].TypeOID)
	}
	_, err := fmt.Fprintln(w.tw, strings.Join(parts, "\t"))
	return err
}

func (w *tableQueryWriter) Close() error {
	if err := w.tw.Flush(); err != nil {
		return err
	}
	if w.rowCount == 0 {
		fmt.Fprintln(w.out, "(0 rows)")
	} else {
		fmt.Fprintf(w.out, "(%d row%s)\n", w.rowCount, plural(w.rowCount))
	}
	return nil
}

// ---------------------------------------------------------------------------
// json format: streams an array of objects, one per row.
// ---------------------------------------------------------------------------

type jsonQueryWriter struct {
	out   io.Writer
	enc   *json.Encoder
	cols  []store.ColumnInfo
	first bool
}

func (w *jsonQueryWriter) OnColumns(cols []store.ColumnInfo) error {
	w.cols = cols
	if _, err := fmt.Fprint(w.out, "["); err != nil {
		return err
	}
	return nil
}

func (w *jsonQueryWriter) OnRow(values []any) error {
	obj := make(map[string]json.RawMessage, len(values))
	for i, v := range values {
		raw, err := marshalForJSON(v, w.cols[i].TypeOID)
		if err != nil {
			return err
		}
		obj[w.cols[i].Name] = raw
	}

	prefix := ""
	if w.first {
		w.first = false
	} else {
		prefix = ","
	}
	if _, err := fmt.Fprint(w.out, prefix); err != nil {
		return err
	}
	return w.enc.Encode(obj) // Encode writes a trailing newline; that's fine.
}

func (w *jsonQueryWriter) Close() error {
	_, err := fmt.Fprintln(w.out, "]")
	return err
}

// ---------------------------------------------------------------------------
// csv format: streams standard CSV with a header row.
// ---------------------------------------------------------------------------

type csvQueryWriter struct {
	out  io.Writer
	w    *csv.Writer
	cols []store.ColumnInfo
}

func (w *csvQueryWriter) OnColumns(cols []store.ColumnInfo) error {
	w.cols = cols
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return w.w.Write(names)
}

func (w *csvQueryWriter) OnRow(values []any) error {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = formatScalarForText(v, w.cols[i].TypeOID)
	}
	return w.w.Write(parts)
}

func (w *csvQueryWriter) Close() error {
	w.w.Flush()
	return w.w.Error()
}

// ---------------------------------------------------------------------------
// Value formatters.
// ---------------------------------------------------------------------------

// formatScalarForText renders a value for table/CSV output. JSONB values
// come through as []byte from pgx; we stringify directly since the
// bytes are valid UTF-8 JSON text. Other types use a small set of
// type-specific cases falling back to fmt.Sprintf("%v").
func formatScalarForText(v any, typeOID uint32) string {
	if v == nil {
		return ""
	}
	if typeOID == oidJSON || typeOID == oidJSONB {
		if b, ok := v.([]byte); ok {
			return string(b)
		}
	}
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// marshalForJSON returns a json.RawMessage encoding v in a way that
// preserves nested JSON for jsonb columns.
func marshalForJSON(v any, typeOID uint32) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	if typeOID == oidJSON || typeOID == oidJSONB {
		if b, ok := v.([]byte); ok {
			if json.Valid(b) {
				return json.RawMessage(b), nil
			}
		}
	}
	if t, ok := v.(time.Time); ok {
		return json.Marshal(t.UTC().Format(time.RFC3339Nano))
	}
	if b, ok := v.([]byte); ok {
		// Non-JSON bytes fall back to a JSON string.
		return json.Marshal(string(b))
	}
	return json.Marshal(v)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
