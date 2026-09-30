package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxJSONBody bounds a request's JSON body. The largest legitimate one — a
// network with every rule it could need — is a few kilobytes; file uploads
// are not JSON and do not come through here.
const maxJSONBody = 1 << 20

// decodeJSON reads r's body into v, strictly, and on failure writes the error
// response itself and returns false. A request the daemon would only partly
// understand is refused, never half-applied:
//
//   - an unknown field is a 400. A misspelt "alowed_egress" would otherwise
//     create a network with no rules and no error; an older daemon would
//     ignore a field a newer client relies on (an image digest, a limit).
//   - a key given twice in one object — also with different case, since field
//     matching ignores case — is a 400: {"intra":false,"INTRA":true} means
//     whatever the parser that reads it picks.
//   - anything after the object is a 400, and so is a body that is not an
//     object.
//   - a body over maxJSONBody is a 413.
//
// optional accepts an empty body as the zero value of v.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request body over %d bytes", tooBig.Limit))
		} else {
			writeError(w, http.StatusBadRequest, fmt.Errorf("reading the request body: %w", err))
		}
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if optional {
			return true
		}
		writeError(w, http.StatusBadRequest, errors.New("a JSON request body is required"))
		return false
	}
	if err := checkJSONType(r); err != nil {
		writeError(w, http.StatusUnsupportedMediaType, err)
		return false
	}
	if err := strictDecode(data, v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("request body: %w", err))
		return false
	}
	return true
}

// strictDecode is decodeJSON's parsing, without the HTTP.
func strictDecode(data []byte, v any) error {
	if err := checkShape(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// checkShape walks the tokens of data: it must be exactly one JSON object,
// and no object in it may name a key twice (compared without case).
func checkShape(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("must be a JSON object")
	}
	// The token reader reports a body cut off mid-object as a bare io.EOF,
	// which would reach the client as "request body: EOF".
	if err := walkObject(dec, ""); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("unexpected end of JSON input")
		}
		return err
	}
	return nil
}

// walkObject consumes an object whose '{' was just read, up to its '}'.
func walkObject(dec *json.Decoder, path string) error {
	seen := make(map[string]bool)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("unexpected %v in an object", tok)
		}
		folded := strings.ToLower(key)
		if seen[folded] {
			return fmt.Errorf("key %q given twice in %s", path+key, orTop(path))
		}
		seen[folded] = true
		if err := walkValue(dec, path+key+"."); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}

func walkValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar
	}
	switch d {
	case '{':
		return walkObject(dec, path)
	case '[':
		for i := 0; dec.More(); i++ {
			if err := walkValue(dec, fmt.Sprintf("%s%d.", path, i)); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ']'
		return err
	}
	return fmt.Errorf("unexpected %v", d)
}

func orTop(path string) string {
	if path == "" {
		return "the body"
	}
	return strings.TrimSuffix(path, ".")
}
