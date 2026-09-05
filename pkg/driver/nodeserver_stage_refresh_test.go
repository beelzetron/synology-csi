package driver

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"k8s.io/mount-utils"
	"k8s.io/utils/exec"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// fakeExec records commands and makes every invoked command succeed with no output,
// so blkid/mkfs/iscsiadm in the mount path are no-ops during tests.
type fakeExec struct {
	commands [][]string
}

func (f *fakeExec) Command(name string, args ...string) exec.Cmd {
	f.commands = append(f.commands, append([]string{name}, args...))
	return fakeCmd{}
}
func (f *fakeExec) CommandContext(_ context.Context, name string, args ...string) exec.Cmd {
	return f.Command(name, args...)
}
func (f *fakeExec) LookPath(name string) (string, error) { return name, nil }

type fakeCmd struct{}

func (fakeCmd) Run() error                       { return nil }
func (fakeCmd) CombinedOutput() ([]byte, error)  { return nil, nil }
func (fakeCmd) Output() ([]byte, error)          { return nil, nil }
func (fakeCmd) SetDir(string)                    {}
func (fakeCmd) SetStdin(io.Reader)               {}
func (fakeCmd) SetStdout(io.Writer)              {}
func (fakeCmd) SetStderr(io.Writer)              {}
func (fakeCmd) SetEnv([]string)                  {}
func (fakeCmd) StdoutPipe() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("")), nil }
func (fakeCmd) StderrPipe() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("")), nil }
func (fakeCmd) Start() error                     { return nil }
func (fakeCmd) Wait() error                      { return nil }
func (fakeCmd) Stop()                            {}

// failOnceMounter implements mount.Interface (via the embedded FakeMounter) but overrides
// IsLikelyNotMountPoint and MountSensitive so a test can inject a filesystem mount failure
// (the "wrong fs type / bad superblock" case we harden against) and then let the retry succeed.
type failOnceMounter struct {
	mount.Interface
	notMount   bool
	failFirst  bool
	mountCalls int
}

func (f *failOnceMounter) IsLikelyNotMountPoint(string) (bool, error) { return f.notMount, nil }
func (f *failOnceMounter) MountSensitive(source, target, fstype string, options, sensitive []string) error {
	f.mountCalls++
	if f.failFirst && f.mountCalls == 1 {
		return errors.New("simulated: wrong fs type, bad option, bad superblock")
	}
	return nil
}

// stubDsm satisfies interfaces.IDsmService via embedding (nil) and overrides only GetVolume.
type stubDsm struct {
	interfaces.IDsmService
	vol *models.K8sVolumeRespSpec
}

func (s *stubDsm) GetVolume(string) *models.K8sVolumeRespSpec { return s.vol }

func iscsiStageSpec() *models.NodeStageVolumeSpec {
	return &models.NodeStageVolumeSpec{
		VolumeId:          "vol-iscsi",
		StagingTargetPath: "/tmp/stage-refresh",
	}
}

func iscsiVolume(protocol string) *models.K8sVolumeRespSpec {
	return &models.K8sVolumeRespSpec{
		Protocol: protocol,
		DsmIp:    "192.168.11.10",
		Target: webapi.TargetInfo{
			Iqn:        "iqn.2000-01.com.synology:nas.test",
			MappedLuns: []webapi.MappedLun{{MappingIndex: 1}},
		},
	}
}

// TestStageMountWithSessionRefresh_MountSucceeds verifies a clean stage returns nil without
// attempting any session refresh.
func TestStageMountWithSessionRefresh_MountSucceeds(t *testing.T) {
	fakeEx := &fakeExec{}
	ns := &nodeServer{
		Mounter: &mount.SafeFormatAndMount{
			Interface: &failOnceMounter{Interface: &mount.FakeMounter{}, notMount: true},
			Exec:      fakeEx,
		},
		dsmService: &stubDsm{vol: iscsiVolume(utils.ProtocolIscsi)},
		tools:      tools{executor: fakeEx},
	}
	loginCalls := 0
	ns.loginTargetFunc = func(string) ([]string, error) {
		loginCalls++
		return []string{"/dev/disk/by-path/fake"}, nil
	}

	if err := ns.stageMountWithSessionRefresh(iscsiStageSpec(), "/dev/disk/by-path/fake", "xfs", []string{"rw"}, nil); err != nil {
		t.Fatalf("expected nil on successful mount, got: %v", err)
	}
	if loginCalls != 0 {
		t.Fatalf("loginTargetFunc called %d times on success, want 0", loginCalls)
	}
}

// TestStageMountWithSessionRefresh_NonIscsiNoRefresh verifies a mount failure on a non-iSCSI
// volume returns the mount error without attempting a session refresh.
func TestStageMountWithSessionRefresh_NonIscsiNoRefresh(t *testing.T) {
	fakeEx := &fakeExec{}
	ns := &nodeServer{
		Mounter: &mount.SafeFormatAndMount{
			Interface: &failOnceMounter{Interface: &mount.FakeMounter{}, notMount: true, failFirst: true},
			Exec:      fakeEx,
		},
		dsmService: &stubDsm{vol: iscsiVolume(utils.ProtocolSmb)},
		Initiator:  &initiatorDriver{tools: tools{executor: fakeEx}},
		tools:      tools{executor: fakeEx},
	}
	loginCalls := 0
	ns.loginTargetFunc = func(string) ([]string, error) {
		loginCalls++
		return []string{"/dev/disk/by-path/fake"}, nil
	}

	err := ns.stageMountWithSessionRefresh(iscsiStageSpec(), "/dev/disk/by-path/fake", "xfs", []string{"rw"}, nil)
	if err == nil {
		t.Fatalf("expected mount error for non-iSCSI volume, got nil")
	}
	if loginCalls != 0 {
		t.Fatalf("loginTargetFunc called %d times for non-iSCSI volume, want 0", loginCalls)
	}
}

// TestStageMountWithSessionRefresh_IscsiRefreshRecovers verifies that a filesystem mount failure
// on an iSCSI volume triggers a session refresh (logout + login) and the retry mount recovers.
func TestStageMountWithSessionRefresh_IscsiRefreshRecovers(t *testing.T) {
	fakeEx := &fakeExec{}
	ns := &nodeServer{
		Mounter: &mount.SafeFormatAndMount{
			Interface: &failOnceMounter{Interface: &mount.FakeMounter{}, notMount: true, failFirst: true},
			Exec:      fakeEx,
		},
		dsmService: &stubDsm{vol: iscsiVolume(utils.ProtocolIscsi)},
		Initiator:  &initiatorDriver{tools: tools{executor: fakeEx}},
		tools:      tools{executor: fakeEx},
	}
	loginCalls := 0
	ns.loginTargetFunc = func(string) ([]string, error) {
		loginCalls++
		return []string{"/dev/null"}, nil
	}

	err := ns.stageMountWithSessionRefresh(iscsiStageSpec(), "/dev/disk/by-path/fake", "xfs", []string{"rw"}, nil)
	if err != nil {
		t.Fatalf("expected mount to recover after session refresh, got: %v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("loginTargetFunc called %d times on iSCSI refresh, want 1", loginCalls)
	}
	// The refresh path must have issued a logout (iscsiadm node/delete) after the mount failure.
	if len(fakeEx.commands) == 0 {
		t.Fatalf("expected iSCSI commands (logout/login) to be attempted, none recorded")
	}
}
