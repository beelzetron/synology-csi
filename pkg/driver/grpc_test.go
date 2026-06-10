/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package driver

import (
	"testing"
	"time"
)

func waitForGRPCServerStop(t *testing.T, srv NonBlockingGRPCServer, stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		stop()
		srv.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestNonBlockingGRPCServerImmediateStop(t *testing.T) {
	srv := NewNonBlockingGRPCServer()
	srv.Start("tcp://127.0.0.1:0", nil, nil, nil)
	waitForGRPCServerStop(t, srv, srv.Stop)
}

func TestNonBlockingGRPCServerImmediateForceStop(t *testing.T) {
	srv := NewNonBlockingGRPCServer()
	srv.Start("tcp://127.0.0.1:0", nil, nil, nil)
	waitForGRPCServerStop(t, srv, srv.ForceStop)
}
