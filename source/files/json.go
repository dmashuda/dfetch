package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/dmashuda/dfetch/source"
)

// describeJSON infers the schema from a sample of objects: the columns are the
// union of keys in first-seen order; each value contributes to its key's type
// guess. lines selects JSONL (a stream of objects) over a top-level array.
func describeJSON(r io.Reader, lines bool) ([]source.Column, error) {
	dec := json.NewDecoder(stripBOM(r))
	dec.UseNumber()
	if !lines {
		if err := expectArray(dec); err != nil {
			return nil, err
		}
	}

	var keys []string
	guesses := map[string]*typeGuess{}
	for range sampleRows {
		more, objKeys, vals, err := nextObject(dec, lines)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		for _, k := range objKeys {
			g, ok := guesses[k]
			if !ok {
				g = &typeGuess{}
				guesses[k] = g
				keys = append(keys, k)
			}
			g.seeJSON(vals[k])
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("no keys to infer columns from (expected objects with at least one key)")
	}

	cols := make([]source.Column, len(keys))
	for i, k := range keys {
		cols[i] = source.Column{Name: k, Type: guesses[k].affinity()}
	}
	return cols, nil
}

// scanJSON streams the objects into w, mapping each object's keys onto the
// declared columns (missing keys become NULL; keys outside the inferred
// schema are dropped).
func scanJSON(r io.Reader, lines bool, cols []source.Column, w *rowWriter) error {
	dec := json.NewDecoder(stripBOM(r))
	dec.UseNumber()
	if !lines {
		if err := expectArray(dec); err != nil {
			return err
		}
	}

	index := make(map[string]int, len(cols))
	for i, col := range cols {
		index[col.Name] = i
	}
	for {
		more, _, vals, err := nextObject(dec, lines)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		row := make([]any, len(cols))
		for k, v := range vals {
			if i, ok := index[k]; ok {
				row[i] = jsonValue(v)
			}
		}
		done, err := w.add(row)
		if err != nil || done {
			return err
		}
	}
}

// expectArray consumes the opening '[' of a top-level array of objects.
func expectArray(dec *json.Decoder) error {
	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return errors.New("empty file (expected a top-level array of objects)")
	}
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected a top-level array of objects, got %v (for a stream of objects use .jsonl)", tok)
	}
	return nil
}

// nextObject decodes the next object — the next array element, or for lines
// mode the next value in the stream — returning its keys in document order
// and its decoded values. more is false at the end of the input.
func nextObject(dec *json.Decoder, lines bool) (more bool, keys []string, vals map[string]any, err error) {
	if !lines && !dec.More() {
		return false, nil, nil, nil
	}
	tok, err := dec.Token()
	if lines && errors.Is(err, io.EOF) {
		return false, nil, nil, nil
	}
	if err != nil {
		return false, nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return false, nil, nil, fmt.Errorf("expected a JSON object, got %v", tok)
	}

	vals = map[string]any{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return false, nil, nil, err
		}
		k, ok := kt.(string)
		if !ok {
			return false, nil, nil, fmt.Errorf("expected an object key, got %v", kt)
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return false, nil, nil, err
		}
		if _, dup := vals[k]; !dup {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return false, nil, nil, err
	}
	return true, keys, vals, nil
}

// jsonValue maps a decoded JSON value to a localdb-accepted Go value: scalars
// keep their natural type (integral numbers become int64, others float64) and
// nested objects/arrays are re-marshaled to a JSON TEXT value.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil, bool, string:
		return x
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(string(x), 64); err == nil {
			return f
		}
		return string(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}

// seeJSON records one decoded JSON value against the guess: null is ignored,
// bool fits INTEGER (stored 0/1), a number narrows by integralness, and
// anything else (string/object/array) forces TEXT.
func (g *typeGuess) seeJSON(v any) {
	switch x := v.(type) {
	case nil:
	case bool:
		g.seen = true
	case json.Number:
		g.seen = true
		if _, err := strconv.ParseInt(string(x), 10, 64); err != nil {
			g.notInt = true
		}
		if _, err := strconv.ParseFloat(string(x), 64); err != nil {
			g.notReal = true
		}
	default:
		g.seen = true
		g.notInt = true
		g.notReal = true
	}
}
