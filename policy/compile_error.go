package policy

// CompilationError identifies a rejection by the pure policy compiler. Its
// result cannot change without different compiler inputs; it is not an I/O or
// analyzer failure. Unwrap preserves the original diagnostic and error cause.
type CompilationError struct{ Err error }

func (e *CompilationError) Error() string { return e.Err.Error() }
func (e *CompilationError) Unwrap() error { return e.Err }
