package state

// StateError mirrors rag_mcp.state.repositories.StateError: a persistence
// failure that the caller may report as a state-database error.
type StateError struct{ Message string }

// Error implements error.
func (e *StateError) Error() string { return e.Message }

// StateConflict mirrors StateConflict (a StateError in Python): a unique key
// already exists.
type StateConflict struct{ Message string }

// Error implements error.
func (e *StateConflict) Error() string { return e.Message }

// LookupError mirrors the built-in LookupError raised by the repositories when
// a record does not exist (Python's KeyError base class; the messages are the
// user-visible Chinese ones).
type LookupError struct{ Message string }

// Error implements error.
func (e *LookupError) Error() string { return e.Message }

// PermissionError mirrors the built-in PermissionError raised for disabled or
// unauthorised credentials.
type PermissionError struct{ Message string }

// Error implements error.
func (e *PermissionError) Error() string { return e.Message }

// ValueError mirrors the built-in ValueError raised by the repositories for
// stored values that cannot be returned as requested.
type ValueError struct{ Message string }

// Error implements error.
func (e *ValueError) Error() string { return e.Message }

func newStateError(message string) *StateError       { return &StateError{Message: message} }
func newStateConflict(message string) *StateConflict { return &StateConflict{Message: message} }
func newLookupError(message string) *LookupError     { return &LookupError{Message: message} }
func newPermissionError(message string) *PermissionError {
	return &PermissionError{Message: message}
}
func newValueError(message string) *ValueError { return &ValueError{Message: message} }
