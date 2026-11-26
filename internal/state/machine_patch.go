package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Distinguish omitted settings on older clients from an explicit null reset.
// Retain strict unknown-field rejection when implementing custom decoding.
func (p *MachinePatch) UnmarshalJSON(b []byte) error {
	type plain MachinePatch
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing machine patch data")
	}
	*p = MachinePatch(decoded)
	var raw struct {
		Runner map[string]json.RawMessage `json:"runner"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if p.Runner != nil {
		_, present := raw.Runner["settings"]
		p.PreserveRunnerSettings = !present
	}
	return nil
}
