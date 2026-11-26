// Package packaging embeds the portable workstation helper for guided enrollment.
package packaging

import _ "embed"

//go:embed macos/workstation.py
var WorkstationHelper []byte
