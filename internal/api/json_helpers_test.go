package api

import (
	"encoding/json"
	"io"
)

func jsonNewDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }
