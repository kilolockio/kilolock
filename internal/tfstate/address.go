package tfstate

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidProviderRef is returned by ParseProviderRef for inputs
// that don't match the canonical wire form Terraform writes.
var ErrInvalidProviderRef = errors.New("invalid provider reference")

// ParseProviderRef breaks a state-file provider reference into its
// source address and (optional) HCL alias.
//
// Inputs follow Terraform's wire format:
//
//	provider["registry.terraform.io/hashicorp/aws"]
//	provider["registry.terraform.io/hashicorp/aws"].west
//	module.foo.provider["registry.terraform.io/hashicorp/aws"]
//
// The leading module.<...> segment, when present, is discarded —
// providers are always resolved to a root-level (source, alias) pair
// regardless of the consuming module path. Refresh callers ultimately
// only need to know "which binary, configured how"; the module path
// is not part of that identity.
//
// Returns ErrInvalidProviderRef wrapped with context for any input
// that does not parse.
func ParseProviderRef(s string) (source, alias string, err error) {
	rest := s

	// Strip leading module.<...> path. The provider keyword is the
	// boundary; everything before it is module navigation.
	if idx := strings.Index(rest, "provider["); idx > 0 {
		// Defensive: only accept ".provider" or "provider" (start)
		// to reject inputs like "fooprovider[...]".
		if rest[idx-1] != '.' {
			return "", "", fmt.Errorf("%w: %q (unexpected text before provider keyword)", ErrInvalidProviderRef, s)
		}
		rest = rest[idx:]
	}

	if !strings.HasPrefix(rest, "provider[") {
		return "", "", fmt.Errorf("%w: %q (missing provider[ prefix)", ErrInvalidProviderRef, s)
	}
	rest = strings.TrimPrefix(rest, "provider[")

	// rest is now: "<quoted-source>]" or "<quoted-source>].<alias>"
	close := strings.Index(rest, "]")
	if close < 0 {
		return "", "", fmt.Errorf("%w: %q (missing closing ])", ErrInvalidProviderRef, s)
	}
	quoted := rest[:close]
	src, ok := unquoteProviderSource(quoted)
	if !ok {
		return "", "", fmt.Errorf("%w: %q (source must be a double-quoted string)", ErrInvalidProviderRef, s)
	}
	if src == "" {
		return "", "", fmt.Errorf("%w: %q (empty source)", ErrInvalidProviderRef, s)
	}

	tail := rest[close+1:]
	switch {
	case tail == "":
		return src, "", nil
	case strings.HasPrefix(tail, "."):
		al := tail[1:]
		if al == "" {
			return "", "", fmt.Errorf("%w: %q (trailing dot with no alias)", ErrInvalidProviderRef, s)
		}
		return src, al, nil
	default:
		return "", "", fmt.Errorf("%w: %q (unexpected text after closing ])", ErrInvalidProviderRef, s)
	}
}

// unquoteProviderSource accepts the quoted form Terraform writes
// (e.g. "registry.terraform.io/hashicorp/aws") and returns the inner
// string. Backslash escapes are not expected here — provider sources
// are URL-shaped tokens with no quoting needs — but they are decoded
// via strconv.Unquote anyway so any future provider with an unusual
// character round-trips correctly.
func unquoteProviderSource(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", false
	}
	out, err := strconv.Unquote(s)
	if err != nil {
		return "", false
	}
	return out, true
}

// InstanceAddress returns the canonical Terraform address for an instance
// of the given resource. Examples:
//
//	aws_vpc.main                                 (root, no index)
//	aws_instance.web[0]                          (root, int index)
//	aws_instance.web["api"]                      (root, string index)
//	data.aws_ami.ubuntu                          (root data source)
//	module.vpc.aws_subnet.private[0]             (nested module)
//
// The function returns an error only when the index_key is malformed; for
// any well-formed instance the address is unambiguous.
func InstanceAddress(r Resource, inst ResourceInstance) (string, error) {
	var b strings.Builder

	if r.Module != "" {
		// r.Module is already in canonical "module.foo.module.bar" form.
		b.WriteString(r.Module)
		b.WriteByte('.')
	}

	if r.Mode == "data" {
		b.WriteString("data.")
	}

	b.WriteString(r.Type)
	b.WriteByte('.')
	b.WriteString(r.Name)

	kind, val, err := inst.DecodeIndex()
	if err != nil {
		return "", fmt.Errorf("instance %s.%s: %w", r.Type, r.Name, err)
	}
	switch kind {
	case IndexNone:
		// no suffix
	case IndexInt:
		b.WriteByte('[')
		b.WriteString(val)
		b.WriteByte(']')
	case IndexString:
		b.WriteByte('[')
		b.WriteString(strconv.Quote(val))
		b.WriteByte(']')
	}

	return b.String(), nil
}
