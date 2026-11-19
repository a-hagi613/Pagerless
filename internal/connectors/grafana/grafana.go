// Package grafana decodes the frames that Grafana's unified query API returns
// and builds its request bodies. The Grafana log and metrics Connectors share
// it; it exposes no Grafana write API.
package grafana

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// QueryBody builds a unified query request for one datasource expression.
func QueryBody(datasource, expr string, extra map[string]any, since, until time.Time) ([]byte, error) {
	query := map[string]any{
		"refId":      "A",
		"datasource": map[string]string{"uid": datasource},
	}
	if expr != "" {
		query["expr"] = expr
	}
	maps.Copy(query, extra)
	request := map[string]any{
		"queries": []any{query},
		"from":    strconv.FormatInt(since.UnixMilli(), 10),
		"to":      strconv.FormatInt(until.UnixMilli(), 10),
	}
	return json.Marshal(request)
}

// Field is one frame field.
type Field struct {
	// Name is the field name, such as Time or Line.
	Name string
	// Type is the Grafana field type.
	Type string
	// Labels are the field's identity labels.
	Labels map[string]string
}

// Frame is one columnar result table.
type Frame struct {
	// Fields describe the columns.
	Fields []Field
	// Columns hold the values; Columns[i][row] is field i at row.
	Columns [][]json.RawMessage
}

// RowCount returns the frame's row count.
func (f Frame) RowCount() int {
	rows := 0
	for _, column := range f.Columns {
		if len(column) > rows {
			rows = len(column)
		}
	}
	return rows
}

// Index returns the index of the first field with one of the names.
func (f Frame) Index(names ...string) int {
	for index, field := range f.Fields {
		for _, name := range names {
			if strings.EqualFold(field.Name, name) {
				return index
			}
		}
	}
	return -1
}

// Value returns the raw value of a field at a row.
func (f Frame) Value(field, row int) json.RawMessage {
	if field < 0 || field >= len(f.Columns) || row < 0 || row >= len(f.Columns[field]) {
		return nil
	}
	return f.Columns[field][row]
}

// Decode parses a unified query response. A per-query error becomes an error,
// so the Connector degrades rather than storing an empty success.
func Decode(raw []byte) ([]Frame, error) {
	var decoded struct {
		Results map[string]struct {
			Error  string `json:"error"`
			Frames []struct {
				Schema struct {
					Fields []struct {
						Name   string            `json:"name"`
						Type   string            `json:"type"`
						Labels map[string]string `json:"labels"`
					} `json:"fields"`
				} `json:"schema"`
				Data struct {
					Values [][]json.RawMessage `json:"values"`
				} `json:"data"`
			} `json:"frames"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode the Grafana query response: %w", err)
	}
	keys := make([]string, 0, len(decoded.Results))
	for key := range decoded.Results {
		keys = append(keys, key)
	}
	// Sort the result keys so the decoded frames are deterministic.
	slices.Sort(keys)
	frames := make([]Frame, 0, len(keys))
	for _, key := range keys {
		result := decoded.Results[key]
		if result.Error != "" {
			return nil, fmt.Errorf("grafana query %s failed: %s", key, result.Error)
		}
		for _, frame := range result.Frames {
			parsed := Frame{Columns: frame.Data.Values, Fields: make([]Field, 0, len(frame.Schema.Fields))}
			for _, field := range frame.Schema.Fields {
				parsed.Fields = append(parsed.Fields, Field{Name: field.Name, Type: field.Type, Labels: field.Labels})
			}
			frames = append(frames, parsed)
		}
	}
	return frames, nil
}

// String decodes a JSON value as a string.
func String(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return strings.Trim(string(raw), `"`)
}

// Number decodes a JSON value as a float64.
func Number(raw json.RawMessage) (float64, bool) {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		parsed, err := strconv.ParseFloat(text, 64)
		return parsed, err == nil
	}
	return 0, false
}

// Time decodes a Grafana time value, which is epoch milliseconds or RFC3339.
func Time(raw json.RawMessage) time.Time {
	if value, ok := Number(raw); ok {
		return time.UnixMilli(int64(value)).UTC()
	}
	text := String(raw)
	if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return parsed.UTC()
	}
	if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
		if parsed > 1e15 {
			return time.Unix(0, parsed).UTC()
		}
		return time.UnixMilli(parsed).UTC()
	}
	return time.Time{}
}
