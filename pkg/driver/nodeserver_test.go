/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package driver

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

func blockVolumeCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{
			Block: &csi.VolumeCapability_BlockVolume{},
		},
	}
}

func mountVolumeCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
	}
}

func TestNodeStageVolumeRejectsNFSBlock(t *testing.T) {
	ns := &nodeServer{}
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-nfs-block",
		StagingTargetPath: "/tmp/stage",
		VolumeCapability:  blockVolumeCapability(),
		VolumeContext: map[string]string{
			"protocol": utils.ProtocolNfs,
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodeStageVolume error code = %v, want %v: %v", status.Code(err), codes.InvalidArgument, err)
	}
}

func TestDoNodeStageOnceCoalescesConcurrentCalls(t *testing.T) {
	ns := &nodeServer{}
	const callers = 5

	var ready sync.WaitGroup
	ready.Add(callers)
	var done sync.WaitGroup
	done.Add(callers)

	start := make(chan struct{})
	called := make(chan struct{})
	release := make(chan struct{})
	var calls int32
	errs := make(chan error, callers)

	for range callers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, err := ns.doNodeStageOnce("vol\x00stage", func() (*csi.NodeStageVolumeResponse, error) {
				if atomic.AddInt32(&calls, 1) == 1 {
					close(called)
				}
				<-release
				return &csi.NodeStageVolumeResponse{}, nil
			})
			errs <- err
		}()
	}

	ready.Wait()
	close(start)
	<-called
	time.Sleep(50 * time.Millisecond)
	close(release)
	done.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("doNodeStageOnce returned error: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("node stage calls = %d, want 1", got)
	}
}

func TestPathLockSetSerializesSharedPaths(t *testing.T) {
	locks := newPathLockSet()
	unlock := locks.lock("/stage-a")

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlockShared := locks.lock("/stage-a")
		close(locked)
		<-release
		unlockShared()
		close(done)
	}()

	select {
	case <-locked:
		t.Fatal("shared path lock was acquired before first lock was released")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("shared path lock did not unblock")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shared path lock holder did not finish")
	}
}

func TestPathLockSetAllowsDifferentPaths(t *testing.T) {
	locks := newPathLockSet()
	unlock := locks.lock("/stage-a")
	defer unlock()

	done := make(chan struct{})
	go func() {
		unlockOther := locks.lock("/stage-b")
		unlockOther()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("different path lock was blocked")
	}
}

func TestNodePublishVolumeWaitsForTargetPathLock(t *testing.T) {
	targetPath := t.TempDir() + "/target"
	stagingTargetPath := t.TempDir() + "/stage"
	ns := &nodeServer{}

	unlock := ns.lockNodePaths(targetPath)
	done := make(chan error, 1)
	go func() {
		_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
			VolumeId:          "vol-nfs",
			StagingTargetPath: stagingTargetPath,
			TargetPath:        targetPath,
			VolumeCapability:  mountVolumeCapability(),
			VolumeContext: map[string]string{
				"protocol": utils.ProtocolNfs,
			},
		})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("NodePublishVolume returned before target path lock was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("NodePublishVolume did not finish after target path lock was released")
	}
}

func TestNodeUnpublishVolumeWaitsForTargetPathLock(t *testing.T) {
	targetPath := t.TempDir() + "/missing"
	ns := &nodeServer{}

	unlock := ns.lockNodePaths(targetPath)
	done := make(chan error, 1)
	go func() {
		_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
			VolumeId:   "vol",
			TargetPath: targetPath,
		})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("NodeUnpublishVolume returned before target path lock was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("NodeUnpublishVolume did not finish after target path lock was released")
	}
}

func TestNodePublishVolumeRejectsNFSBlock(t *testing.T) {
	ns := &nodeServer{}
	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-nfs-block",
		StagingTargetPath: "/tmp/stage",
		TargetPath:        "/tmp/target",
		VolumeCapability:  blockVolumeCapability(),
		VolumeContext: map[string]string{
			"protocol": utils.ProtocolNfs,
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("NodePublishVolume error code = %v, want %v: %v", status.Code(err), codes.InvalidArgument, err)
	}
}

func TestNodePublishVolumeLogsOutISCSIWhenPublishFailsAfterLogin(t *testing.T) {
	targetPath := t.TempDir() + "/target"
	stagingTargetPath := t.TempDir() + "/stage"
	var logoutCalls int
	var logoutVolumeID, logoutStagingPath string

	ns := &nodeServer{
		Mounter: &mount.SafeFormatAndMount{
			Interface: mount.New(""),
		},
		loginTargetFunc: func(volumeId string) ([]string, error) {
			return nil, nil
		},
		logoutTargetFunc: func(volumeID string, stagingTargetPath string) {
			logoutCalls++
			logoutVolumeID = volumeID
			logoutStagingPath = stagingTargetPath
		},
	}

	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-iscsi",
		StagingTargetPath: stagingTargetPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountVolumeCapability(),
		VolumeContext: map[string]string{
			"protocol": utils.ProtocolIscsi,
		},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("NodePublishVolume error code = %v, want %v: %v", status.Code(err), codes.Internal, err)
	}
	if logoutCalls != 1 {
		t.Fatalf("logout calls = %d, want 1", logoutCalls)
	}
	if logoutVolumeID != "vol-iscsi" || logoutStagingPath != stagingTargetPath {
		t.Fatalf("logout args = (%q, %q), want (%q, %q)", logoutVolumeID, logoutStagingPath, "vol-iscsi", stagingTargetPath)
	}
}
