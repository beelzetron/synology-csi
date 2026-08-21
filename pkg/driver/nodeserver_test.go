/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package driver

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
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

func TestNodePublishVolumeLogsOutISCSIBlockWhenPublishFailsAfterLogin(t *testing.T) {
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
		VolumeCapability:  blockVolumeCapability(),
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

func TestNodePublishVolumeDoesNotLogoutISCSIFilesystemWhenBindFails(t *testing.T) {
	targetPath := t.TempDir() + "/target"
	stagingTargetPath := t.TempDir() + "/missing-stage"
	var loginCalls, logoutCalls int

	ns := &nodeServer{
		Mounter: &mount.SafeFormatAndMount{
			Interface: mount.New(""),
		},
		loginTargetFunc: func(volumeId string) ([]string, error) {
			loginCalls++
			return []string{"/dev/test"}, nil
		},
		logoutTargetFunc: func(volumeID string, stagingTargetPath string) {
			logoutCalls++
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
	if loginCalls != 0 {
		t.Fatalf("login calls = %d, want 0", loginCalls)
	}
	if logoutCalls != 0 {
		t.Fatalf("logout calls = %d, want 0", logoutCalls)
	}
}

// mockDsmServiceWithDsm returns a DSM stub wired to a live fake HTTP server,
// letting tests exercise setNFSVolumePermission's DSM request end to end.
type mockDsmServiceWithDsm struct {
	mockDsmService
	dsm *webapi.DSM
}

func (m *mockDsmServiceWithDsm) GetDsm(ip string) (*webapi.DSM, error) { return m.dsm, nil }

// TestSetNFSVolumePermissionGrantsSquashUser verifies that for NFS volumes
// under the all_admin/all_guest root_squash modes the driver grants a user RW
// ACE on the DSM share root (the server-side replacement for the manual NAS
// `chmod` previously required for non-root pods), and selects the correct
// target user per mode. It asserts the exact SYNO.Core.Share.Permission
// request DSM receives.
func TestSetNFSVolumePermissionGrantsSquashUser(t *testing.T) {
	tests := []struct {
		name       string
		rootSquash string
		wantUser   string
	}{
		{name: "all_admin grants admin RW", rootSquash: "all_admin", wantUser: "admin"},
		{name: "all_guest grants guest RW", rootSquash: "all_guest", wantUser: "guest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotQuery url.Values
			var gotCookieHost string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				gotCookieHost = r.Host
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"success":true}`))
			}))
			defer srv.Close()

			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			host, rawPort, err := net.SplitHostPort(u.Host)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(rawPort)
			if err != nil {
				t.Fatal(err)
			}

			dsm := &webapi.DSM{
				Ip:       host,
				Port:     port,
				Https:    false,
				Username: "test",
				Password: "test",
				Sid:      "test-sid",
			}
			// The DSM client must resolve the share on a per-request host; the
			// httptest host is the same instance we already captured.
			_ = gotCookieHost

			dsmSvc := &mockDsmServiceWithDsm{dsm: dsm}
			ns := &nodeServer{dsmService: dsmSvc}

			if err := ns.setNFSVolumePermission("//192.168.11.10/harnfs", tt.rootSquash); err != nil {
				t.Fatalf("setNFSVolumePermission error: %v", err)
			}

			if gotQuery.Get("api") != "SYNO.Core.Share.Permission" {
				t.Fatalf("api = %q, want SYNO.Core.Share.Permission", gotQuery.Get("api"))
			}
			if gotQuery.Get("method") != "set" {
				t.Fatalf("method = %q, want set", gotQuery.Get("method"))
			}
			if gotQuery.Get("version") != "1" {
				t.Fatalf("version = %q, want 1", gotQuery.Get("version"))
			}
			if gotQuery.Get("name") != `"harnfs"` {
				t.Fatalf("name = %q, want QUOTED harnfs", gotQuery.Get("name"))
			}
			if gotQuery.Get("user_group_type") != `"local_user"` {
				t.Fatalf("user_group_type = %q, want local_user", gotQuery.Get("user_group_type"))
			}
			wantJSON := `[{"name":"` + tt.wantUser + `","is_readonly":false,"is_writable":true,"is_deny":false}]`
			if got := gotQuery.Get("permissions"); got != wantJSON {
				t.Fatalf("permissions = %q, want %q", got, wantJSON)
			}
		})
	}
}

// TestSetNFSVolumePermissionMalformedSource ensures a bad NFS source fails
// before any DSM call (matching setNFSVolumePrivilege behaviour).
func TestSetNFSVolumePermissionMalformedSource(t *testing.T) {
	ns := &nodeServer{dsmService: &mockDsmService{}}
	if err := ns.setNFSVolumePermission("not-a-source", "all_guest"); err == nil {
		t.Fatal("expected error for malformed NFS source, got nil")
	}
}
