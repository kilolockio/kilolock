package provider

// Severity classifies a Diagnostic as fatal, advisory, or
// invalid (the third is reserved by the wire protocol for
// "we forgot to set this field" and should never appear from
// well-behaved providers).
type Severity uint8

const (
	SeverityInvalid Severity = iota
	SeverityError
	SeverityWarning
)

// String reports a short symbolic name used in default formatting.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "ERROR"
	case SeverityWarning:
		return "WARNING"
	default:
		return "INVALID"
	}
}

// Diagnostic is a single message returned by a provider alongside an
// RPC response. Providers return rich, often actionable text in
// these — e.g. "the IAM role arn:aws:iam::123:role/foo does not
// exist; check that you assumed the right account before running
// terraform". Callers should surface them verbatim where possible
// rather than wrapping them in their own error types.
type Diagnostic struct {
	Severity Severity
	Summary  string
	Detail   string

	// Attribute is the optional structured pointer into the
	// offending config attribute. Kept as raw proto bytes for
	// this commit; a future commit will define a typed
	// representation once a consumer needs it (probably during
	// plan diagnostics, not refresh).
	Attribute []byte
}

// Diagnostics is the list of provider-returned messages from a
// single RPC. Most refresh RPCs return zero diagnostics.
type Diagnostics []Diagnostic

// HasError reports whether any diagnostic has SeverityError.
func (d Diagnostics) HasError() bool {
	for _, x := range d {
		if x.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Errors returns the subset of d with SeverityError. The returned
// slice shares backing storage with d up to the filter point; do
// not append to it.
func (d Diagnostics) Errors() Diagnostics {
	out := make(Diagnostics, 0, len(d))
	for _, x := range d {
		if x.Severity == SeverityError {
			out = append(out, x)
		}
	}
	return out
}

// Warnings returns the subset of d with SeverityWarning.
func (d Diagnostics) Warnings() Diagnostics {
	out := make(Diagnostics, 0, len(d))
	for _, x := range d {
		if x.Severity == SeverityWarning {
			out = append(out, x)
		}
	}
	return out
}
