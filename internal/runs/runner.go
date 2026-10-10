package runs

import _ "embed"

// SessionRunner is the program a session's driver runs (runner/session_runner.py): one
// SparkSession, statements from the backend only, over a bearer-checked HTTP port.
//
//go:embed runner/session_runner.py
var SessionRunner string
