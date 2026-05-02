package conntrack

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
)

// recordingRunner replaces the package-level runner for the duration
// of a test. Returns a slice of (cmd, args...) calls and a cleanup
// that restores the original runner.
func recordingRunner(t *testing.T, fail error) (*[][]string, func()) {
	t.Helper()
	calls := make([][]string, 0, 4)
	old := runner
	runner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		c := append([]string{name}, args...)
		calls = append(calls, c)
		return nil, fail
	}
	return &calls, func() { runner = old }
}

func TestFlushDestination_NilIPIsNoop(t *testing.T) {
	calls, restore := recordingRunner(t, nil)
	defer restore()

	FlushDestination(context.Background(), nil)

	if len(*calls) != 0 {
		t.Errorf("nil IP must not invoke runner, got %d calls", len(*calls))
	}
}

func TestFlushDestination_V4Command(t *testing.T) {
	calls, restore := recordingRunner(t, nil)
	defer restore()

	FlushDestination(context.Background(), net.ParseIP("172.30.0.5"))

	want := []string{"conntrack", "-d", "172.30.0.5", "-D"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Errorf("v4 cmd: got %v, want %v", *calls, want)
	}
}

func TestFlushDestination_V6Command(t *testing.T) {
	calls, restore := recordingRunner(t, nil)
	defer restore()

	FlushDestination(context.Background(), net.ParseIP("fd30::5"))

	want := []string{"conntrack", "-d", "fd30::5", "-D"}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Errorf("v6 cmd: got %v, want %v", *calls, want)
	}
}

func TestFlushDestination_NonzeroExitIsSilent(t *testing.T) {
	// `conntrack -d X -D` exits 1 when nothing matches. We must not
	// blow up — just log at debug and return.
	calls, restore := recordingRunner(t, errors.New("exit status 1"))
	defer restore()

	// Must not panic. The function returns nothing; success means it
	// completes.
	FlushDestination(context.Background(), net.ParseIP("172.30.0.5"))

	if len(*calls) != 1 {
		t.Errorf("expected exactly one runner call, got %d", len(*calls))
	}
}
