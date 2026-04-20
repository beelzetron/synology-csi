/*
Copyright 2021 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/moby/sys/mountinfo"
	"k8s.io/mount-utils"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

const iscsiTeardownSubdir = "iscsi-teardown"

// iscsiTeardownRecord is persisted after a successful iSCSI login so NodeUnstageVolume can
// tear down sessions even when DSM no longer lists the volume (e.g. DeleteVolume raced ahead).
type iscsiTeardownRecord struct {
	VolumeID      string `json:"volume_id"`
	TargetIQN     string `json:"target_iqn"`
	DsmIP         string `json:"dsm_ip"`
	MappingIndex  int    `json:"mapping_index"`
	UsedMultipath bool   `json:"used_multipath"`
}

func csiPluginDataDirFromEndpoint(endpoint string) string {
	proto, addr, err := ParseEndpoint(endpoint)
	if err != nil || proto != "unix" {
		return ""
	}
	if !strings.HasPrefix(addr, "/") {
		addr = "/" + addr
	}
	return filepath.Dir(addr)
}

func (ns *nodeServer) iscsiTeardownDir() string {
	base := ns.Driver.csiPluginDataDir
	if base == "" {
		return ""
	}
	return filepath.Join(base, iscsiTeardownSubdir)
}

func (ns *nodeServer) iscsiTeardownStatePath(volumeID string) string {
	dir := ns.iscsiTeardownDir()
	if dir == "" {
		return ""
	}
	safe := strings.ReplaceAll(volumeID, string(os.PathSeparator), "_")
	safe = strings.ReplaceAll(safe, "..", "_")
	return filepath.Join(dir, safe+".json")
}

func (ns *nodeServer) hostAbsPath(p string) string {
	if ns.Driver.hostRoot == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return filepath.Join(ns.Driver.hostRoot, p)
	}
	return filepath.Join(ns.Driver.hostRoot, p)
}

func (ns *nodeServer) saveISCITeardownRecord(volumeID string, rec *iscsiTeardownRecord) {
	path := ns.iscsiTeardownStatePath(volumeID)
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		log.Warnf("iscsi teardown: could not create state dir %q: %v", dir, err)
		return
	}
	rec.VolumeID = volumeID
	data, err := json.Marshal(rec)
	if err != nil {
		log.Warnf("iscsi teardown: marshal state for volume %s: %v", volumeID, err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Warnf("iscsi teardown: write temp state %q: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Warnf("iscsi teardown: rename state to %q: %v", path, err)
		_ = os.Remove(tmp)
	}
}

func (ns *nodeServer) loadISCITeardownRecord(volumeID string) (*iscsiTeardownRecord, error) {
	path := ns.iscsiTeardownStatePath(volumeID)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec iscsiTeardownRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (ns *nodeServer) removeISCITeardownRecord(volumeID string) {
	path := ns.iscsiTeardownStatePath(volumeID)
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Debugf("iscsi teardown: remove state %q: %v", path, err)
	}
}

func (ns *nodeServer) persistISCITeardownAfterLogin(volumeID string, iscsiDevPaths []string) {
	k8sVolume := ns.dsmService.GetVolume(volumeID)
	if k8sVolume == nil || k8sVolume.Protocol != utils.ProtocolIscsi {
		return
	}
	if len(k8sVolume.Target.MappedLuns) < 1 {
		return
	}
	volumeMountPath := getVolumeMountPath(iscsiDevPaths)
	usedMP := strings.Contains(volumeMountPath, "/dev/mapper") && ns.tools.IsMultipathEnabled()
	rec := &iscsiTeardownRecord{
		TargetIQN:     k8sVolume.Target.Iqn,
		DsmIP:         k8sVolume.DsmIp,
		MappingIndex:  k8sVolume.Target.MappedLuns[0].MappingIndex,
		UsedMultipath: usedMP,
	}
	ns.saveISCITeardownRecord(volumeID, rec)
}

// mountSourcePath returns the mounted device (Source) for an exact mount point.
func mountSourcePath(_ mount.Interface, mountPoint string) (string, error) {
	entries, err := mountinfo.GetMounts(nil)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Mountpoint == mountPoint {
			if e.Source != "" {
				return e.Source, nil
			}
			if e.Root != "" {
				return e.Root, nil
			}
			return "", fmt.Errorf("empty mount source for %q", mountPoint)
		}
	}
	return "", fmt.Errorf("no mountinfo entry for %q", mountPoint)
}

func scsiLUNFromSysfsDevicePath(resolved string) int {
	parts := strings.Split(resolved, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "block" && i > 0 {
			if lun := lunFromHBTL(parts[i-1]); lun >= 0 {
				return lun
			}
		}
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if lun := lunFromHBTL(parts[i]); lun >= 0 {
			return lun
		}
	}
	return 0
}

func lunFromHBTL(s string) int {
	segs := strings.Split(s, ":")
	if len(segs) != 4 {
		return -1
	}
	n, err := strconv.Atoi(segs[3])
	if err != nil {
		return -1
	}
	return n
}

// iqnFromMountPoint walks sysfs from the block device backing mountPoint to read the iSCSI target IQN.
func (ns *nodeServer) iqnFromMountPoint(mountPoint string) (iqn string, lun int, err error) {
	src, err := mountSourcePath(ns.Mounter.Interface, mountPoint)
	if err != nil {
		return "", 0, err
	}
	return ns.iqnFromBlockDevPath(src)
}

func (ns *nodeServer) iqnFromBlockDevPath(devPath string) (iqn string, lun int, err error) {
	devPath = filepath.Clean(devPath)
	real, err := filepath.EvalSymlinks(ns.hostAbsPath(devPath))
	if err != nil {
		return "", 0, fmt.Errorf("eval device symlink %q: %w", devPath, err)
	}
	base := filepath.Base(real)
	if strings.HasPrefix(base, "dm-") {
		slavesDir := ns.hostAbsPath(filepath.Join("/sys/block", base, "slaves"))
		ents, err := os.ReadDir(slavesDir)
		if err != nil || len(ents) == 0 {
			return "", 0, fmt.Errorf("multipath device %s: no slaves under %s: %w", base, slavesDir, err)
		}
		base = ents[0].Name()
	}
	deviceLink := ns.hostAbsPath(filepath.Join("/sys/block", base, "device"))
	resolved, err := filepath.EvalSymlinks(deviceLink)
	if err != nil {
		return "", 0, fmt.Errorf("eval %s: %w", deviceLink, err)
	}
	lun = scsiLUNFromSysfsDevicePath(resolved)
	if !strings.Contains(resolved, "session") {
		return "", lun, fmt.Errorf("device %s does not appear to be iSCSI (no session in sysfs path)", devPath)
	}
	var sessionToken string
	for _, p := range strings.Split(resolved, string(filepath.Separator)) {
		if strings.HasPrefix(p, "session") && len(p) > len("session") {
			sessionToken = p
			break
		}
	}
	if sessionToken == "" {
		return "", lun, fmt.Errorf("session token not found in sysfs path %s", resolved)
	}
	targetFile := ns.hostAbsPath(filepath.Join("/sys/class/iscsi_session", sessionToken, "targetname"))
	b, err := os.ReadFile(targetFile)
	if err != nil {
		return "", lun, fmt.Errorf("read %s: %w", targetFile, err)
	}
	iqn = strings.TrimSpace(string(b))
	if iqn == "" {
		return "", lun, errors.New("empty IQN from sysfs")
	}
	return iqn, lun, nil
}

// logoutTarget tears down iSCSI sessions for volumeID using DSM metadata, persisted node state,
// or sysfs derived from stagingTargetPath (filesystem volumes).
func (ns *nodeServer) logoutTarget(volumeID string, stagingTargetPath string) {
	var iqn string
	var dsmIP string
	var mappingIndex int
	var usedMultipath bool
	var fromPersisted bool

	if rec, err := ns.loadISCITeardownRecord(volumeID); err != nil {
		log.Warnf("iscsi teardown: invalid state file for volume %s: %v", volumeID, err)
	} else if rec != nil {
		iqn, dsmIP, mappingIndex, usedMultipath = rec.TargetIQN, rec.DsmIP, rec.MappingIndex, rec.UsedMultipath
		fromPersisted = true
		log.Infof("iscsi teardown: using persisted metadata for volume %s (IQN %s)", volumeID, iqn)
	}

	k8sVolume := ns.dsmService.GetVolume(volumeID)
	if iqn == "" && k8sVolume != nil && k8sVolume.Protocol == utils.ProtocolIscsi {
		if len(k8sVolume.Target.MappedLuns) < 1 {
			log.Warnf("iscsi teardown: volume %s has no mapped LUNs in DSM response", volumeID)
			return
		}
		iqn = k8sVolume.Target.Iqn
		dsmIP = k8sVolume.DsmIp
		mappingIndex = k8sVolume.Target.MappedLuns[0].MappingIndex
		volumeMountPath := ns.tools.getExistedVolumeMountPath(iqn, mappingIndex)
		usedMultipath = strings.Contains(volumeMountPath, "/dev/mapper") && ns.tools.IsMultipathEnabled()
	}

	if iqn == "" && stagingTargetPath != "" {
		if iq, lun, err := ns.iqnFromMountPoint(stagingTargetPath); err == nil && iq != "" {
			iqn = iq
			mappingIndex = lun
			volumeMountPath := ns.tools.getExistedVolumeMountPath(iqn, mappingIndex)
			usedMultipath = strings.Contains(volumeMountPath, "/dev/mapper") && ns.tools.IsMultipathEnabled()
			log.Infof("iscsi teardown: derived IQN from staging mount for volume %s (IQN %s lunIndex %d)", volumeID, iqn, mappingIndex)
		} else if err != nil {
			log.Debugf("iscsi teardown: sysfs from staging %q for volume %s: %v", stagingTargetPath, volumeID, err)
		}
	}

	if iqn == "" {
		if k8sVolume == nil && !fromPersisted {
			log.Debugf("iscsi teardown: skip volume %s (not iSCSI or no teardown needed); staging=%q", volumeID, stagingTargetPath)
		}
		return
	}

	if usedMultipath {
		volumeMountPath := ns.tools.getExistedVolumeMountPath(iqn, mappingIndex)
		if volumeMountPath != "" && strings.Contains(volumeMountPath, "/dev/mapper") && ns.tools.IsMultipathEnabled() {
			if err := ns.tools.multipath_flush(volumeMountPath); err != nil {
				log.Errorf("Failed to remove multipath device in path %s. err: %v", volumeMountPath, err)
			}
		}
	}

	if err := ns.Initiator.logout(iqn, dsmIP); err != nil {
		log.Warnf("iscsi teardown: iscsiadm logout failed for volume %s IQN %s: %v", volumeID, iqn, err)
		return
	}
	ns.removeISCITeardownRecord(volumeID)
}
