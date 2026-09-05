/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/container-storage-interface/spec/lib/go/csi"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

type nodeServer struct {
	csi.UnimplementedNodeServer
	Driver     *Driver
	Mounter    *mount.SafeFormatAndMount
	dsmService interfaces.IDsmService
	Initiator  *initiatorDriver
	Client     clientset.Interface
	tools      tools

	loginTargetFunc  func(volumeId string) ([]string, error)
	logoutTargetFunc func(volumeID string, stagingTargetPath string)
	nodeStageFlight  singleflight.Group
	pathLocksMu      sync.Mutex
	pathLocks        *pathLockSet
}

type pathLockSet struct {
	mu    sync.Mutex
	locks map[string]*pathLock
}

type pathLock struct {
	mu   sync.Mutex
	refs int
}

func newPathLockSet() *pathLockSet {
	return &pathLockSet{locks: make(map[string]*pathLock)}
}

func cleanLockPaths(paths ...string) []string {
	seen := make(map[string]struct{}, len(paths))
	keys := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		keys = append(keys, path)
	}
	sort.Strings(keys)
	return keys
}

func (locks *pathLockSet) lock(paths ...string) func() {
	keys := cleanLockPaths(paths...)
	if len(keys) == 0 {
		return func() {}
	}

	locks.mu.Lock()
	held := make([]*pathLock, 0, len(keys))
	for _, key := range keys {
		lock := locks.locks[key]
		if lock == nil {
			lock = &pathLock{}
			locks.locks[key] = lock
		}
		lock.refs++
		held = append(held, lock)
	}
	locks.mu.Unlock()

	for _, lock := range held {
		lock.mu.Lock()
	}

	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].mu.Unlock()
		}

		locks.mu.Lock()
		for _, key := range keys {
			lock := locks.locks[key]
			lock.refs--
			if lock.refs == 0 {
				delete(locks.locks, key)
			}
		}
		locks.mu.Unlock()
	}
}

func (ns *nodeServer) lockNodePaths(paths ...string) func() {
	ns.pathLocksMu.Lock()
	if ns.pathLocks == nil {
		ns.pathLocks = newPathLockSet()
	}
	locks := ns.pathLocks
	ns.pathLocksMu.Unlock()
	return locks.lock(paths...)
}

func waitForDevicePathToExist(path string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			exists, err := mount.PathExists(path)
			if err != nil {
				return err
			}
			if exists == true {
				return nil
			}
			log.Warnf("Device path [%s] doesn't exists yet, retrying in 1 second", path)
		case <-timer.C:
			return os.ErrNotExist
		}
	}
}

// for unstage, resize volume
func (t *tools) getExistedVolumeMountPath(targetIqn string, mappingIndex int) string {
	paths := []string{}

	sessions := t.listSessionsByIqn(targetIqn)
	for _, session := range sessions {
		paths = append(paths, fmt.Sprintf("%sip-%s-iscsi-%s-lun-%d", "/dev/disk/by-path/", session.Portal, targetIqn, mappingIndex))
	}

	return getVolumeMountPath(paths)
}

// for publish, stage volume
func getVolumeMountPath(iscsiDevPaths []string) string {
	var path string

	if len(iscsiDevPaths) > 1 { // check multipath exist
		devices, err := lsblk(iscsiDevPaths, true)
		if err != nil {
			log.Errorf("Failed to lsblk for iscsi devices: %v", err)
			return ""
		}

		multipathDevice, err := GetMultipathDevice(devices)
		if err != nil {
			log.Error(err)
			return ""
		}
		path = filepath.Join("/dev/mapper", multipathDevice.Name)
	} else if len(iscsiDevPaths) == 1 {
		path = iscsiDevPaths[0]
	} else {
		return ""
	}

	if err := waitForDevicePathToExist(path); err != nil {
		log.Errorf("Can't find device path [%s],: %v", path, err)
		return ""
	}

	return path
}

func createTargetMountPathNFS(mounter mount.Interface, mountPath string, mountPermissionsUint uint64) (bool, error) {
	notMount, err := mounter.IsLikelyNotMountPoint(mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(mountPath, os.FileMode(mountPermissionsUint)); err != nil {
				return notMount, err
			}
			notMount = true
		} else {
			return false, err
		}
	}
	return notMount, nil
}

func createTargetMountPath(mounter mount.Interface, mountPath string, isBlock bool) (bool, error) {
	notMount, err := mount.IsNotMountPoint(mounter, mountPath)
	if err != nil {
		if os.IsNotExist(err) {
			if isBlock {
				pathFile, err := os.OpenFile(mountPath, os.O_CREATE|os.O_RDWR, 0750)
				if err != nil {
					log.Errorf("Failed to create mountPath:%s with error: %v", mountPath, err)
					return notMount, err
				}
				if err = pathFile.Close(); err != nil {
					log.Errorf("Failed to close mountPath:%s with error: %v", mountPath, err)
					return notMount, err
				}
			} else {
				err = os.MkdirAll(mountPath, 0750)
				if err != nil {
					return notMount, err
				}
			}
			notMount = true
		} else {
			return false, err
		}
	}
	return notMount, nil
}

func (ns *nodeServer) getPortals(dsmIp string) []string {
	portals := []string{}

	dsm, err := ns.dsmService.GetDsm(dsmIp)
	if err != nil {
		log.Errorf("Failed to get DSM[%s]", dsmIp)
		return portals
	}

	ips, err := utils.LookupIPv4(dsmIp)
	if err != nil {
		log.Error(err)
		portals = append(portals, fmt.Sprintf("%s:%d", dsmIp, ISCSIPort))
	} else {
		portals = append(portals, fmt.Sprintf("%s:%d", ips[0], ISCSIPort)) //get the first ip
	}

	if dsm.IsUC() && ns.tools.IsMultipathEnabled() {
		dsm2, err := dsm.GetAnotherController()
		if err != nil {
			log.Errorf("[%s] UC failed to get another controller: %v", dsmIp, err)
		} else {
			portals = append(portals, fmt.Sprintf("%s:%d", dsm2.Ip, ISCSIPort))
		}
	}
	return portals
}

func (ns *nodeServer) loginTarget(volumeId string) ([]string, error) {
	paths := []string{}
	k8sVolume := ns.dsmService.GetVolume(volumeId)

	if k8sVolume == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume[%s] is not found", volumeId))
	}

	if len(k8sVolume.Target.MappedLuns) == 0 {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Volume[%s] has no mapped LUNs", volumeId))
	}

	portals := ns.getPortals(k8sVolume.DsmIp)
	if len(portals) == 0 {
		return nil, status.Error(codes.Internal, "Failed to get portals")
	}

	// Assume target and lun 1-1 mapping
	mappingIndex := k8sVolume.Target.MappedLuns[0].MappingIndex
	for _, portal := range portals {
		if err := ns.Initiator.login(k8sVolume.Target.Iqn, portal); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to login with target iqn [%s], err: %v", k8sVolume.Target.Iqn, err)
		}

		path := fmt.Sprintf("%sip-%s-iscsi-%s-lun-%d", "/dev/disk/by-path/", portal, k8sVolume.Target.Iqn, mappingIndex)
		if err := waitForDevicePathToExist(path); err != nil {
			log.Errorf("Can't find device path [%s]: %v", path, err)
			return nil, status.Errorf(codes.Internal, "Can't find device path [%s]: %v", path, err)
		}

		paths = append(paths, path)
	}

	ns.persistISCITeardownAfterLogin(volumeId, paths)
	return paths, nil
}

func checkGidPresentInMountFlags(volumeMountGroup string, mountFlags []string) (bool, error) {
	gidPresentInMountFlags := false
	for _, mountFlag := range mountFlags {
		if strings.HasPrefix(mountFlag, "gid") {
			gidPresentInMountFlags = true
			kvpair := strings.Split(mountFlag, "=")
			if volumeMountGroup != "" && len(kvpair) == 2 && !strings.EqualFold(volumeMountGroup, kvpair[1]) {
				return false, status.Error(codes.InvalidArgument, fmt.Sprintf("gid(%s) in storageClass and pod fsgroup(%s) are not equal", kvpair[1], volumeMountGroup))
			}
		}
	}
	return gidPresentInMountFlags, nil
}

func (ns *nodeServer) mountSensitiveWithRetry(ctx context.Context, sourcePath string, targetPath string, fsType string, options []string, sensitiveOptions []string) error {
	mountBackoff := backoff.NewExponentialBackOff()
	mountBackoff.InitialInterval = 1 * time.Second
	mountBackoff.Multiplier = 2
	mountBackoff.RandomizationFactor = 0.1
	mountBackoff.MaxElapsedTime = 5 * time.Second

	checkFinished := func() error {
		if err := ctx.Err(); err != nil {
			return backoff.Permanent(err)
		}
		return ns.Mounter.MountSensitive(sourcePath, targetPath, fsType, options, sensitiveOptions)
	}

	mountNotify := func(err error, duration time.Duration) {
		log.Infof("Retry MountSensitive, waiting %3.2f seconds .....", float64(duration.Seconds()))
	}

	if err := backoff.RetryNotify(checkFinished, mountBackoff, mountNotify); err != nil {
		log.Errorf("Could not finish mount after %3.2f seconds.", float64(mountBackoff.MaxElapsedTime.Seconds()))
		return err
	}

	log.Debugf("Mount successfully. source: %s, target: %s", sourcePath, targetPath)
	return nil
}

func getNodeAddress(ctx context.Context, client clientset.Interface) ([]string, error) {
	ips := []string{}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Errorf("Failed to list nodes, err: %v", err)
		return nil, err
	}

	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if address.Type == "InternalIP" {
				ips = append(ips, address.Address)
			}
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("Empty results")
	}
	return ips, nil
}

func (ns *nodeServer) setNFSVolumePrivilege(sourcePath string, hostnames []string, authType utils.AuthType, rootSquash string) error {
	// NFSTODO: fix the parsing rule
	s := strings.Split(strings.TrimPrefix(sourcePath, "//"), "/")
	if len(s) != 2 {
		return fmt.Errorf("Failed to parse dsmIp and shareName from source path")
	}
	dsmIp, shareName := s[0], s[1]

	dsm, err := ns.dsmService.GetDsm(dsmIp)
	if err != nil {
		return fmt.Errorf("Failed to get DSM[%s]", dsmIp)
	}

	// rootSquash is a per-StorageClass option (StorageClass parameter
	// "rootSquash"). An unset or invalid value falls back to the historical
	// behaviour ("root"). Setting "none" disables NFS root squash so kubelet's
	// fsGroup ownership chown is not squashed to an anonymous identity
	// server-side, allowing non-root pods to read/write the share.
	rootSquash = normalizeRootSquash(rootSquash)

	priv := webapi.SharePrivilege{
		ShareName: shareName,
	}

	for _, hostname := range hostnames {
		priv.Rule = append(priv.Rule, webapi.PrivilegeRule{
			Async:      true,
			Client:     hostname,
			Crossmnt:   true,
			Insecure:   true,
			Privilege:  string(authType),
			RootSquash: rootSquash,
			SecurityFlavor: webapi.SecurityFlavor{
				Kerbros:          false,
				KerbrosIntegrity: false,
				KerbrosPrivacy:   false,
				Sys:              true,
			},
		})
	}

	err = dsm.ShareNfsPrivilegeSave(priv)
	if err != nil {
		log.Printf("Failed to save share NFS privilege. Priv:%v. %v", priv, err)
		return err
	}
	return nil
}

func (ns *nodeServer) setNFSVolumePermission(sourcePath string, rootSquash string) error {
	// NFSTODO: fix the parsing rule (same as setNFSVolumePrivilege)
	s := strings.Split(strings.TrimPrefix(sourcePath, "//"), "/")
	if len(s) != 2 {
		return fmt.Errorf("Failed to parse dsmIp and shareName from source path")
	}
	dsmIp, shareName := s[0], s[1]

	dsm, err := ns.dsmService.GetDsm(dsmIp)
	if err != nil {
		return fmt.Errorf("Failed to get DSM[%s]", dsmIp)
	}

	permUser := "guest"
	if rootSquash == "all_admin" {
		permUser = "admin"
	}

	permission := webapi.SharePermission{
		Name:       permUser,
		IsWritable: true,
	}

	spec := webapi.SharePermissionSetSpec{
		Name:          shareName,
		UserGroupType: models.UserGroupTypeLocalUser,
		Permissions:   []*webapi.SharePermission{&permission},
	}

	return dsm.SharePermissionSet(spec)
}

func (ns *nodeServer) setSMBVolumePermission(sourcePath string, userName string, authType utils.AuthType) error {
	s := strings.Split(strings.TrimPrefix(sourcePath, "//"), "/")
	if len(s) != 2 {
		return fmt.Errorf("Failed to parse dsmIp and shareName from source path")
	}
	dsmIp, shareName := s[0], s[1]

	dsm, err := ns.dsmService.GetDsm(dsmIp)
	if err != nil {
		return fmt.Errorf("Failed to get DSM[%s]", dsmIp)
	}

	permission := webapi.SharePermission{
		Name: userName,
	}
	switch authType {
	case utils.AuthTypeReadWrite:
		permission.IsWritable = true
	case utils.AuthTypeReadOnly:
		permission.IsReadonly = true
	case utils.AuthTypeNoAccess:
		permission.IsDeny = true
	default:
		return fmt.Errorf("Unknown auth type: %s", string(authType))
	}

	permissions := append([]*webapi.SharePermission{}, &permission)
	spec := webapi.SharePermissionSetSpec{
		Name:          shareName,
		UserGroupType: models.UserGroupTypeLocalUser,
		Permissions:   permissions,
	}

	return dsm.SharePermissionSet(spec)
}

func (ns *nodeServer) nodeStageISCSIVolume(ctx context.Context, spec *models.NodeStageVolumeSpec) (*csi.NodeStageVolumeResponse, error) {
	key := spec.VolumeId + "\x00" + spec.StagingTargetPath
	return ns.doNodeStageOnce(key, func() (*csi.NodeStageVolumeResponse, error) {
		return ns.nodeStageISCSIVolumeLocked(ctx, spec)
	})
}

func (ns *nodeServer) doNodeStageOnce(key string, fn func() (*csi.NodeStageVolumeResponse, error)) (*csi.NodeStageVolumeResponse, error) {
	resp, err, _ := ns.nodeStageFlight.Do(key, func() (interface{}, error) {
		return fn()
	})
	if err != nil {
		return nil, err
	}
	return resp.(*csi.NodeStageVolumeResponse), nil
}

func (ns *nodeServer) nodeStageISCSIVolumeLocked(ctx context.Context, spec *models.NodeStageVolumeSpec) (*csi.NodeStageVolumeResponse, error) {
	// if block mode, skip mount
	if spec.VolumeCapability.GetBlock() != nil {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	iscsiDevPaths, err := ns.loginTarget(spec.VolumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	stagedOK := false
	defer func() {
		if !stagedOK {
			ns.logoutTarget(spec.VolumeId, spec.StagingTargetPath)
		}
	}()

	volumeMountPath := getVolumeMountPath(iscsiDevPaths)
	if volumeMountPath == "" {
		return nil, status.Error(codes.Internal, "Can't get volume mount path")
	}

	notMount, err := ns.Mounter.Interface.IsLikelyNotMountPoint(spec.StagingTargetPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if !notMount {
		stagedOK = true
		return &csi.NodeStageVolumeResponse{}, nil
	}

	fsType := spec.VolumeCapability.GetMount().GetFsType()
	options := append([]string{"rw"}, spec.VolumeCapability.GetMount().GetMountFlags()...)

	formatOptions := utils.StringToSlice(spec.FormatOptions)

	if err = ns.stageMountWithSessionRefresh(spec, volumeMountPath, fsType, options, formatOptions); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	stagedOK = true
	return &csi.NodeStageVolumeResponse{}, nil
}

// stageMountWithSessionRefresh mounts the iSCSI volume, and if the first attempt fails with a
// filesystem/mount error, refreshes the iSCSI session (logout + login, re-resolving the device)
// and retries once. This guards against a resolved by-path device that has no valid filesystem
// because a stale/dead session or a clone+delete cycle (e.g. the velero snapshot data mover)
// left a stale by-path device behind — observed as "wrong fs type, bad option, bad superblock on
// /dev/sdX". login() short-circuits when a session already exists, so kubelet's NodeStageVolume
// retries (every ~2m) would otherwise reuse the same stale device and never recover. Logging the
// blkid result distinguishes an empty device (mkfs is appropriate) from a stale/wrong device.
func (ns *nodeServer) stageMountWithSessionRefresh(spec *models.NodeStageVolumeSpec, volumeMountPath, fsType string, options, formatOptions []string) error {
	err := ns.Mounter.FormatAndMountSensitiveWithFormatOptions(volumeMountPath, spec.StagingTargetPath, fsType, options, nil, formatOptions)
	if err == nil {
		return nil
	}

	log.Warnf("NodeStageVolume: mount of %s at %s failed (%v); refreshing iSCSI session and retrying", volumeMountPath, spec.StagingTargetPath, err)
	ns.logDeviceFilesystem(volumeMountPath)

	k8sVolume := ns.dsmService.GetVolume(spec.VolumeId)
	if k8sVolume == nil || k8sVolume.Protocol != utils.ProtocolIscsi {
		return err
	}

	if lgErr := ns.Initiator.logout(k8sVolume.Target.Iqn, k8sVolume.DsmIp); lgErr != nil {
		log.Warnf("NodeStageVolume: session refresh logout for %s failed: %v", k8sVolume.Target.Iqn, lgErr)
	}

	loginTarget := ns.loginTarget
	if ns.loginTargetFunc != nil {
		loginTarget = ns.loginTargetFunc
	}
	paths, lgErr := loginTarget(spec.VolumeId)
	if lgErr != nil {
		return fmt.Errorf("refresh login for volume %s failed: %v", spec.VolumeId, lgErr)
	}
	if refreshed := getVolumeMountPath(paths); refreshed != "" {
		volumeMountPath = refreshed
		log.Infof("NodeStageVolume: refreshed device path for %s -> %s", spec.VolumeId, volumeMountPath)
	}

	return ns.Mounter.FormatAndMountSensitiveWithFormatOptions(volumeMountPath, spec.StagingTargetPath, fsType, options, nil, formatOptions)
}

// logDeviceFilesystem logs the detected filesystem (or absence) on a block device. Useful to tell
// an empty device (no superblock) apart from a stale/wrong device, and to confirm why a mount
// failed with "wrong fs type / bad superblock".
func (ns *nodeServer) logDeviceFilesystem(devPath string) {
	if ns.tools.executor == nil {
		return
	}
	out, err := ns.tools.executor.Command("blkid", "-p", "-s", "TYPE", "-s", "PTTYPE", devPath).CombinedOutput()
	if err != nil {
		// blkid exits non-zero when there is no usable filesystem — that is exactly the
		// "bad superblock" case, so surface it at info level for diagnosis.
		log.Infof("NodeStageVolume: blkid on %s -> no filesystem detected (%v): %s", devPath, err, strings.TrimSpace(string(out)))
		return
	}
	log.Infof("NodeStageVolume: blkid on %s -> %s", devPath, strings.TrimSpace(string(out)))
}

func (ns *nodeServer) nodeStageSMBVolume(ctx context.Context, spec *models.NodeStageVolumeSpec, secrets map[string]string) (*csi.NodeStageVolumeResponse, error) {
	if spec.VolumeCapability.GetBlock() != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("SMB protocol only allows 'mount' access type"))
	}

	if spec.Source == "" { //"//<host>/<shareName>"
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Missing 'source' field"))
	}

	if secrets == nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Missing secrets for node staging volume"))
	}

	username := strings.TrimSpace(secrets["username"])
	password := strings.TrimSpace(secrets["password"])
	domain := strings.TrimSpace(secrets["domain"])

	// set permission to access the share
	if err := ns.setSMBVolumePermission(spec.Source, username, utils.AuthTypeReadWrite); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to set permission, source: %s, err: %v", spec.Source, err))
	}

	// create mount point if not exists
	targetPath := spec.StagingTargetPath
	notMount, err := createTargetMountPath(ns.Mounter.Interface, targetPath, false)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		log.Infof("NodeStageVolume: %s is already mounted", targetPath)
		return &csi.NodeStageVolumeResponse{}, nil // already mount
	}

	fsType := "cifs"
	options := spec.VolumeCapability.GetMount().GetMountFlags()

	volumeMountGroup := spec.VolumeCapability.GetMount().GetVolumeMountGroup()
	gidPresent, err := checkGidPresentInMountFlags(volumeMountGroup, options)
	if err != nil {
		return nil, err
	}
	if !gidPresent && volumeMountGroup != "" {
		options = append(options, fmt.Sprintf("gid=%s", volumeMountGroup))
	}

	if domain != "" {
		options = append(options, fmt.Sprintf("%s=%s", "domain", domain))
	}
	var sensitiveOptions = []string{fmt.Sprintf("%s=%s,%s=%s", "username", username, "password", password)}
	if err := ns.mountSensitiveWithRetry(ctx, spec.Source, targetPath, fsType, options, sensitiveOptions); err != nil {
		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Volume[%s] failed to mount %q on %q. err: %v", spec.VolumeId, spec.Source, targetPath, err))
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) nodeStageNFSVolume(ctx context.Context, spec *models.NodeStageVolumeSpec) (*csi.NodeStageVolumeResponse, error) {
	if spec.VolumeCapability.GetBlock() != nil {
		return nil, status.Error(codes.InvalidArgument, "NFS protocol only allows 'mount' access type")
	}

	nodeIps, err := getNodeAddress(ctx, ns.Client)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to get node IPs for NFS privilege setting, err: %v", err))
	}

	if err := ns.setNFSVolumePrivilege(spec.Source, nodeIps, utils.AuthTypeReadWrite, spec.RootSquash); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to set NFS privilege rule, source: %s, err: %v", spec.Source, err))
	}

	// Synology creates CSI share roots with Windows/NFSv4 ACLs granting ONLY
	// group:administrators. Over NFSv4.1 sec=sys the server evaluates that ACL
	// for every request: a non-root pod uid (e.g. 1000950000) does not match a
	// group ACE (group membership of anonymous uids is not expanded), so writes
	// fail with EACCES even when the share POSIX mode is 0777. The durable,
	// server-side fix (equivalent to DSM UI "map all users to admin/guest") is
	// to ALSO grant a user RW ACE on the share for the user the client uids are
	// squashed to (admin for all_admin, guest for all_guest). Without this, the
	// SC must be manually worked around with an out-of-band `chmod` on the NAS.
	// Only the all_* squash modes reach non-root pods; root ("no mapping") and
	// admin/guest (root-only squash) are left with the historical behaviour.
	if spec.RootSquash == "all_admin" || spec.RootSquash == "all_guest" {
		if err := ns.setNFSVolumePermission(spec.Source, spec.RootSquash); err != nil {
			log.Printf("Failed to set NFS share permission (continuing; rule already saved). source: %s, err: %v", spec.Source, err)
		}
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeId, stagingTargetPath, volumeCapability :=
		req.GetVolumeId(), req.GetStagingTargetPath(), req.GetVolumeCapability()

	if volumeId == "" || stagingTargetPath == "" || volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument,
			"InvalidArgument: Please check volume ID, staging target path and volume capability.")
	}

	if volumeCapability.GetBlock() != nil && volumeCapability.GetMount() != nil {
		return nil, status.Error(codes.InvalidArgument, "Cannot mix block and mount capabilities")
	}

	unlock := ns.lockNodePaths(stagingTargetPath)
	defer unlock()

	spec := &models.NodeStageVolumeSpec{
		VolumeId:          volumeId,
		StagingTargetPath: stagingTargetPath,
		VolumeCapability:  volumeCapability,
		Dsm:               req.VolumeContext["dsm"],
		Source:            req.VolumeContext["source"], // filled by CreateVolume response
		FormatOptions:     req.VolumeContext["formatOptions"],
		RootSquash:        req.VolumeContext["rootSquash"],
	}

	switch req.VolumeContext["protocol"] {
	case utils.ProtocolSmb:
		return ns.nodeStageSMBVolume(ctx, spec, req.GetSecrets())
	case utils.ProtocolNfs:
		return ns.nodeStageNFSVolume(ctx, spec)
	default:
		return ns.nodeStageISCSIVolume(ctx, spec)
	}
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID, stagingTargetPath := req.GetVolumeId(), req.GetStagingTargetPath()

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	unlock := ns.lockNodePaths(stagingTargetPath)
	defer unlock()
	defer ns.logoutTarget(volumeID, stagingTargetPath)

	notMount, err := mount.IsNotMountPoint(ns.Mounter.Interface, stagingTargetPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		err = ns.Mounter.Interface.Unmount(stagingTargetPath)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeId, targetPath, stagingTargetPath := req.GetVolumeId(), req.GetTargetPath(), req.GetStagingTargetPath()

	if volumeId == "" || targetPath == "" || stagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument,
			"InvalidArgument: Please check volume ID, target path and staging target path.")
	}

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}

	unlock := ns.lockNodePaths(targetPath, stagingTargetPath)
	defer unlock()

	isBlock := req.GetVolumeCapability().GetBlock() != nil // raw block, only for iscsi protocol
	fsType := req.GetVolumeCapability().GetMount().GetFsType()
	options := []string{}
	if req.GetReadonly() {
		options = append(options, "ro")
	}

	// nfs
	if req.VolumeContext["protocol"] == utils.ProtocolNfs {
		if isBlock {
			return nil, status.Error(codes.InvalidArgument, "NFS protocol only allows 'mount' access type")
		}

		options = append(options, req.GetVolumeCapability().GetMount().GetMountFlags()...)

		var server, baseDir string             //NFSTODO: subDir
		var mountPermissionsUint uint64 = 0750 // default
		for k, v := range req.GetVolumeContext() {
			switch k {
			case "dsm":
				server = v
			case "baseDir":
				baseDir = v
			case "mountPermissions":
				if v != "" {
					var err error
					mountPermissionsUint, err = strconv.ParseUint(v, 8, 32)
					if err != nil {
						return nil, status.Errorf(codes.InvalidArgument, "invalid mountPermissions %s", v)
					}
				}
			}
		}

		if server == "" || baseDir == "" {
			return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Invalid inputs: server(dsm) and baseDir are required."))
		}
		source := fmt.Sprintf("%s:%s", server, baseDir)

		notMount, err := createTargetMountPathNFS(ns.Mounter.Interface, targetPath, mountPermissionsUint)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if !notMount {
			log.Infof("NodePublishVolume: %s is already mounted", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}

		log.Debugf("NodePublishVolume: volumeId(%v) source(%s) targetPath(%s) mountflags(%v)", volumeId, source, targetPath, options)
		err = ns.Mounter.Mount(source, targetPath, "nfs", options)
		if err != nil {
			if os.IsPermission(err) {
				return nil, status.Error(codes.PermissionDenied, err.Error())
			}
			if strings.Contains(err.Error(), "invalid argument") {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			return nil, status.Error(codes.Internal, err.Error())
		}

		if mountPermissionsUint > 0 {
			if err := chmodIfPermissionMismatch(targetPath, os.FileMode(mountPermissionsUint)); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}

		log.Debugf("NFS volume(%s) mount %s on %s succeeded", volumeId, source, targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// iscsi & smb
	notMount, err := createTargetMountPath(ns.Mounter.Interface, targetPath, isBlock)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !notMount {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	options = append(options, "bind")

	switch req.VolumeContext["protocol"] {
	case utils.ProtocolSmb:
		if err := ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, "", options); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	default:
		if !isBlock {
			if err := ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, fsType, options); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			return &csi.NodePublishVolumeResponse{}, nil
		}

		loginTarget := ns.loginTarget
		if ns.loginTargetFunc != nil {
			loginTarget = ns.loginTargetFunc
		}
		logoutTarget := ns.logoutTarget
		if ns.logoutTargetFunc != nil {
			logoutTarget = ns.logoutTargetFunc
		}

		iscsiDevPaths, err := loginTarget(volumeId)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		publishedOK := false
		defer func() {
			if !publishedOK {
				logoutTarget(volumeId, stagingTargetPath)
			}
		}()

		volumeMountPath := getVolumeMountPath(iscsiDevPaths)
		if volumeMountPath == "" {
			return nil, status.Error(codes.Internal, "Can't get volume mount path")
		}

		if isBlock {
			err = ns.Mounter.Interface.Mount(volumeMountPath, targetPath, "", options)
		} else {
			err = ns.Mounter.Interface.Mount(stagingTargetPath, targetPath, fsType, options)
		}
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		publishedOK = true
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" { // Not needed, but still a mandatory field
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	unlock := ns.lockNodePaths(targetPath)
	defer unlock()

	if _, err := os.Stat(targetPath); err != nil {
		if os.IsNotExist(err) {
			return &csi.NodeUnpublishVolumeResponse{}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	notMount, err := mount.IsNotMountPoint(ns.Mounter.Interface, targetPath)

	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if notMount {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err := ns.Mounter.Interface.Unmount(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := os.Remove(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to remove target path.")
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	log.Debugf("Using default NodeGetInfo, ns.Driver.nodeID = [%s]", ns.Driver.nodeID)

	return &csi.NodeGetInfoResponse{
		NodeId: ns.Driver.nodeID,
	}, nil
}

func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.Driver.nsCap,
	}, nil
}

func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeId, volumePath := req.GetVolumeId(), req.GetVolumePath()
	if volumeId == "" || volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "Invalid Argument")
	}

	k8sVolume := ns.dsmService.GetVolume(volumeId)
	if k8sVolume == nil {
		return nil, status.Error(codes.NotFound,
			fmt.Sprintf("Volume[%s] is not found", volumeId))
	}

	notMount, err := mount.IsNotMountPoint(ns.Mounter.Interface, volumePath)
	if err != nil || notMount {
		return nil, status.Error(codes.NotFound,
			fmt.Sprintf("Volume[%s] does not exist on the %s", volumeId, volumePath))
	}

	if k8sVolume.Protocol == utils.ProtocolSmb || k8sVolume.Protocol == utils.ProtocolNfs {
		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				&csi.VolumeUsage{
					Total: k8sVolume.SizeInBytes,
					Unit:  csi.VolumeUsage_BYTES,
				},
			},
		}, nil
	}

	// If we are dealing with a LUN use statfs
	statfs := &unix.Statfs_t{}
	err = unix.Statfs(volumePath, statfs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get fs info on path %s: %v", req.VolumePath, err)
	}

	// Available is blocks available * fragment size
	available := int64(statfs.Bavail) * int64(statfs.Bsize)

	// Capacity is total block count * fragment size
	capacity := int64(statfs.Blocks) * int64(statfs.Bsize)

	// Usage is block being used * fragment size (aka block size).
	usage := (int64(statfs.Blocks) - int64(statfs.Bfree)) * int64(statfs.Bsize)

	inodes := int64(statfs.Files)
	inodesFree := int64(statfs.Ffree)
	inodesUsed := inodes - inodesFree

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Available: available,
				Total:     capacity,
				Used:      usage,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
			},
		},
	}, nil
}

func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeId, volumePath := req.GetVolumeId(), req.GetVolumePath()
	if volumeId == "" || volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "InvalidArgument: Please check volume ID and volume path.")
	}

	sizeInByte, err := getSizeByCapacityRange(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}

	k8sVolume := ns.dsmService.GetVolume(volumeId)
	if k8sVolume == nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume[%s] is not found", volumeId))
	}

	if k8sVolume.Protocol == utils.ProtocolSmb || k8sVolume.Protocol == utils.ProtocolNfs {
		return &csi.NodeExpandVolumeResponse{
			CapacityBytes: sizeInByte}, nil
	}

	if len(k8sVolume.Target.MappedLuns) == 0 {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Volume[%s] has no mapped LUNs", volumeId))
	}

	if err := ns.Initiator.rescan(k8sVolume.Target.Iqn); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to rescan. err: %v", err))
	}

	// Assume target and lun 1-1 mapping
	mappingIndex := k8sVolume.Target.MappedLuns[0].MappingIndex
	volumeMountPath := ns.tools.getExistedVolumeMountPath(k8sVolume.Target.Iqn, mappingIndex)
	if volumeMountPath == "" {
		return nil, status.Error(codes.Internal, "Can't get volume mount path")
	}

	if strings.Contains(volumeMountPath, "/dev/mapper") && ns.tools.IsMultipathEnabled() {
		if err := ns.tools.multipath_resize(filepath.Base(volumeMountPath)); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to resize multipath device in %s. err: %v", volumeMountPath, err))
		}
	}

	isBlock := req.GetVolumeCapability() != nil && req.GetVolumeCapability().GetBlock() != nil
	if isBlock {
		return &csi.NodeExpandVolumeResponse{
			CapacityBytes: sizeInByte}, nil
	}

	ok, err := mount.NewResizeFs(ns.Mounter.Exec).Resize(volumeMountPath, volumePath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Error(codes.Internal, "Failed to expand volume filesystem")
	}
	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: sizeInByte}, nil
}
