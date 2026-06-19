package source

import "context"

// csvSource reads rows from a CSV file. It is a stub pending implementation.
type csvSource struct {
	table string
	path  string
}

// NewCSVSource builds a CSV-backed source. Expected params: "path" (string).
func NewCSVSource(table string, params map[string]any) (Source, error) {
	path, _ := params["path"].(string)
	return &csvSource{table: table, path: path}, nil
}

func (s *csvSource) Schema(_ context.Context) (TableSchema, error) {
	return TableSchema{}, ErrNotImplemented
}

func (s *csvSource) Fetch(_ context.Context) ([][]any, error) {
	return nil, ErrNotImplemented
}
