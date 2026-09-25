package server

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	bigqueryv2 "google.golang.org/api/bigquery/v2"
)

// csvFieldDelimiter resolves BigQuery's fieldDelimiter option. BigQuery
// accepts a single character, the escape "\t" and the word "tab".
func csvFieldDelimiter(s string) (rune, error) {
	switch s {
	case "":
		return ',', nil
	case `\t`, "tab", "TAB":
		return '\t', nil
	}
	r := []rune(s)
	if len(r) != 1 {
		return 0, fmt.Errorf("fieldDelimiter must be a single character")
	}
	return r[0], nil
}

// decodeCSVBytes converts the payload to UTF-8 according to the load job's
// encoding option and strips a leading byte-order mark.
func decodeCSVBytes(b []byte, encoding string) ([]byte, error) {
	switch strings.ToUpper(strings.ReplaceAll(encoding, "_", "-")) {
	case "", "UTF-8", "UTF8":
		return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), nil
	case "ISO-8859-1", "LATIN1":
		var buf strings.Builder
		buf.Grow(len(b))
		for _, c := range b {
			buf.WriteRune(rune(c))
		}
		return []byte(buf.String()), nil
	default:
		return nil, fmt.Errorf("unsupported encoding: %s", encoding)
	}
}

// readLoadCSV parses a CSV load payload honoring fieldDelimiter, quote and
// encoding. The standard double-quote character goes through encoding/csv
// (which always accepts quoted newlines); any other quote character, or an
// empty quote that disables quoting, uses a small dedicated parser.
func readLoadCSV(reader io.Reader, load *bigqueryv2.JobConfigurationLoad) ([][]string, error) {
	delim, err := csvFieldDelimiter(load.FieldDelimiter)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read csv: %w", err)
	}
	b, err := decodeCSVBytes(raw, load.Encoding)
	if err != nil {
		return nil, err
	}
	quote := '"'
	if load.Quote != nil {
		q := []rune(*load.Quote)
		switch len(q) {
		case 0:
			quote = 0
		case 1:
			quote = q[0]
		default:
			return nil, fmt.Errorf("quote must be a single character")
		}
	}
	if quote == '"' {
		r := csv.NewReader(bytes.NewReader(b))
		r.FieldsPerRecord = -1
		r.Comma = delim
		records, err := r.ReadAll()
		if err != nil {
			return nil, fmt.Errorf("failed to read csv: %w", err)
		}
		return records, nil
	}
	return parseCSVWithQuote(string(b), delim, quote)
}

// parseCSVWithQuote parses CSV with an arbitrary quote rune; quote 0 means
// quoting is disabled. A doubled quote inside a quoted field is a literal.
func parseCSVWithQuote(s string, delim, quote rune) ([][]string, error) {
	var (
		records [][]string
		record  []string
		field   strings.Builder
		inQuote bool
	)
	endRecord := func() {
		record = append(record, field.String())
		field.Reset()
		records = append(records, record)
		record = nil
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if inQuote {
			if r == quote {
				if next, n := utf8.DecodeRuneInString(s[i:]); i < len(s) && next == quote {
					field.WriteRune(quote)
					i += n
					continue
				}
				inQuote = false
				continue
			}
			field.WriteRune(r)
			continue
		}
		switch {
		case quote != 0 && r == quote && field.Len() == 0:
			inQuote = true
		case r == delim:
			record = append(record, field.String())
			field.Reset()
		case r == '\r' && strings.HasPrefix(s[i:], "\n"):
			// handled by the following '\n'
		case r == '\n':
			endRecord()
		default:
			field.WriteRune(r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("failed to read csv: unterminated quoted field")
	}
	if field.Len() > 0 || len(record) > 0 {
		endRecord()
	}
	return records, nil
}

// csvCellIsNull reports whether a CSV cell loads as NULL. A cell equal to
// the null marker is NULL. An empty cell is NULL unless a custom null marker
// is set and the column is a STRING, in which case it is the empty string.
func csvCellIsNull(value, nullMarker, columnType string) bool {
	if value == nullMarker {
		return true
	}
	if value != "" {
		return false
	}
	return nullMarker == "" || !strings.EqualFold(columnType, "STRING")
}
