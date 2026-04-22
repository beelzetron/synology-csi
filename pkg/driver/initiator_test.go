/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package driver

import (
	"context"
	"errors"
	"io"
	"testing"

	utilexec "k8s.io/utils/exec"
)

type exitCodeErr struct {
	code int
}

func (e exitCodeErr) Error() string   { return "iscsiadm" }
func (e exitCodeErr) ExitStatus() int { return e.code }

type stubCmd struct {
	out []byte
	err error
}

func (c *stubCmd) Run() error                         { _, err := c.CombinedOutput(); return err }
func (c *stubCmd) CombinedOutput() ([]byte, error)    { return c.out, c.err }
func (c *stubCmd) Output() ([]byte, error)            { return c.CombinedOutput() }
func (c *stubCmd) SetDir(string)                      {}
func (c *stubCmd) SetStdin(io.Reader)                 {}
func (c *stubCmd) SetStdout(io.Writer)                {}
func (c *stubCmd) SetStderr(io.Writer)                {}
func (c *stubCmd) SetEnv([]string)                    {}
func (c *stubCmd) StdoutPipe() (io.ReadCloser, error) { return nil, errors.New("not implemented") }
func (c *stubCmd) StderrPipe() (io.ReadCloser, error) { return nil, errors.New("not implemented") }
func (c *stubCmd) Start() error                       { return errors.New("not implemented") }
func (c *stubCmd) Wait() error                        { return errors.New("not implemented") }
func (c *stubCmd) Stop()                              {}

type seqFakeExecutor struct {
	cfg   []stubCmd
	calls [][]string
}

func (s *seqFakeExecutor) Command(cmd string, args ...string) utilexec.Cmd {
	s.calls = append(s.calls, append([]string{cmd}, args...))
	idx := len(s.calls) - 1
	var c stubCmd
	if idx < len(s.cfg) {
		c = s.cfg[idx]
	}
	return &c
}

func (s *seqFakeExecutor) CommandContext(ctx context.Context, cmd string, args ...string) utilexec.Cmd {
	return s.Command(cmd, args...)
}

func TestLogoutWithSessionRunsLogoutThenNodeDelete(t *testing.T) {
	t.Parallel()
	iqn := "iqn.2000-01.com.synology:nas.pvc-test"
	sessionLine := "tcp: [1] 192.168.1.10:3260,1 " + iqn + "\n"
	ex := &seqFakeExecutor{
		cfg: []stubCmd{
			{out: []byte(sessionLine), err: nil},
			{out: nil, err: nil},
			{out: nil, err: nil},
		},
	}
	d := &initiatorDriver{tools: NewTools(ex)}
	if err := d.logout(iqn, "192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"iscsiadm", "-m", "session"},
		{"iscsiadm", "-m", "node", "--targetname", iqn, "--logout"},
		{"iscsiadm", "-m", "node", "--targetname", iqn, "--op", "delete"},
	}
	if len(ex.calls) != len(want) {
		t.Fatalf("calls: got %d want %d: %#v", len(ex.calls), len(want), ex.calls)
	}
	for i := range want {
		if len(ex.calls[i]) != len(want[i]) {
			t.Fatalf("call %d len: got %v want %v", i, ex.calls[i], want[i])
		}
		for j := range want[i] {
			if ex.calls[i][j] != want[i][j] {
				t.Fatalf("call %d arg %d: got %q want %q", i, j, ex.calls[i][j], want[i][j])
			}
		}
	}
}

func TestLogoutWithoutSessionStillDeletesNodeRecords(t *testing.T) {
	t.Parallel()
	iqn := "iqn.2000-01.com.synology:nas.pvc-orphan"
	ex := &seqFakeExecutor{
		cfg: []stubCmd{
			{out: nil, err: exitCodeErr{code: 21}},
			{out: nil, err: nil},
		},
	}
	d := &initiatorDriver{tools: NewTools(ex)}
	if err := d.logout(iqn, "192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"iscsiadm", "-m", "session"},
		{"iscsiadm", "-m", "node", "--targetname", iqn, "--op", "delete"},
	}
	if len(ex.calls) != len(want) {
		t.Fatalf("calls: got %d want %d: %#v", len(ex.calls), len(want), ex.calls)
	}
	for i := range want {
		if len(ex.calls[i]) != len(want[i]) {
			t.Fatalf("call %d len: got %v want %v", i, ex.calls[i], want[i])
		}
		for j := range want[i] {
			if ex.calls[i][j] != want[i][j] {
				t.Fatalf("call %d arg %d: got %q want %q", i, j, ex.calls[i][j], want[i][j])
			}
		}
	}
}
