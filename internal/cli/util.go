package cli

import (
	"encoding/json"
	"errors"
	"io"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc
}

func asErr[T error](err error, target *T) bool { return errors.As(err, target) }
