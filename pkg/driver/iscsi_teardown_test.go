/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package driver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCsiPluginDataDirFromEndpoint(t *testing.T) {
	t.Parallel()
	if got := csiPluginDataDirFromEndpoint("unix://csi/csi.sock"); got != "/csi" {
		t.Fatalf("unexpected plugin dir: %q", got)
	}
	if got := csiPluginDataDirFromEndpoint("unix:///var/lib/kubelet/plugins/csi.san.synology.com/csi.sock"); got != "/var/lib/kubelet/plugins/csi.san.synology.com" {
		t.Fatalf("unexpected plugin dir: %q", got)
	}
}

func TestScsiLUNFromSysfsDevicePath(t *testing.T) {
	t.Parallel()
	p := "/sys/devices/pci/host4/session4/target4:0:0/4:0:0:7/block/sdb"
	if got := scsiLUNFromSysfsDevicePath(p); got != 7 {
		t.Fatalf("lun want 7 got %d", got)
	}
}

func TestLunFromHBTL(t *testing.T) {
	t.Parallel()
	if got := lunFromHBTL("4:0:0:2"); got != 2 {
		t.Fatalf("want 2 got %d", got)
	}
	if got := lunFromHBTL("bad"); got >= 0 {
		t.Fatalf("expected negative, got %d", got)
	}
}

func TestISCITeardownStateRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "csi.sock")
	endpoint := "unix://" + sock
	if err := os.MkdirAll(filepath.Dir(sock), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}

	drv, err := NewControllerAndNodeDriver("node1", endpoint, nil, tools{}, "")
	if err != nil {
		t.Fatal(err)
	}
	ns := &nodeServer{Driver: drv}

	volID := "abc-def-000"
	rec := &iscsiTeardownRecord{
		TargetIQN:     "iqn.2000-01.com.synology:test",
		DsmIP:         "10.0.0.1",
		MappingIndex:  1,
		UsedMultipath: true,
	}
	ns.saveISCITeardownRecord(volID, rec)

	loaded, err := ns.loadISCITeardownRecord(volID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("expected record")
	}
	if loaded.TargetIQN != rec.TargetIQN || loaded.MappingIndex != rec.MappingIndex || !loaded.UsedMultipath {
		t.Fatalf("mismatch: %+v", loaded)
	}

	ns.removeISCITeardownRecord(volID)
	if _, err := os.Stat(ns.iscsiTeardownStatePath(volID)); !os.IsNotExist(err) {
		t.Fatalf("state file should be removed: %v", err)
	}
}
