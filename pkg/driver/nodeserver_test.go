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
