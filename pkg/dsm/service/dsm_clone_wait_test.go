package service

import (
	"os"
	"testing"
	"time"
)

func TestCloneWaitMaxElapsed_default(t *testing.T) {
	t.Cleanup(func() { _ = os.Unsetenv("SYNOLOGY_CSI_CLONE_WAIT_TIMEOUT") })
	_ = os.Unsetenv("SYNOLOGY_CSI_CLONE_WAIT_TIMEOUT")
	if got := cloneWaitMaxElapsed(); got != 60*time.Minute {
		t.Fatalf("default: got %v want 60m", got)
	}
}

func TestCloneWaitMaxElapsed_env(t *testing.T) {
	t.Setenv("SYNOLOGY_CSI_CLONE_WAIT_TIMEOUT", "90m")
	if got := cloneWaitMaxElapsed(); got != 90*time.Minute {
		t.Fatalf("env: got %v want 90m", got)
	}
}

func TestCloneWaitMaxElapsed_invalidFallsBack(t *testing.T) {
	t.Setenv("SYNOLOGY_CSI_CLONE_WAIT_TIMEOUT", "not-a-duration")
	if got := cloneWaitMaxElapsed(); got != 60*time.Minute {
		t.Fatalf("invalid: got %v want 60m", got)
	}
}
