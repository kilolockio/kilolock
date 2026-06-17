package backend

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/kilolockio/kilolock/internal/routing"
)

func TestIsEnvironmentUnavailableError(t *testing.T) {
	if !isEnvironmentUnavailableError(errors.New("connection refused")) {
		t.Fatal("expected true for connection refused")
	}
	if !isEnvironmentUnavailableError(routing.ErrEnvironmentUnavailable) {
		t.Fatal("expected true for routing unavailable")
	}
	if !isEnvironmentUnavailableError(&net.DNSError{Err: "no such host"}) {
		t.Fatal("expected true for dns error")
	}
	if !isEnvironmentUnavailableError(&os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}) {
		t.Fatal("expected true for syscall connection error")
	}
	if isEnvironmentUnavailableError(errors.New("syntax error")) {
		t.Fatal("expected false for unrelated error")
	}
}
