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

import "strings"

// defaultRootSquash preserves the historical hardcoded behaviour
// (RootSquash: "root" written to the DSM NFS share-privilege rule) whenever a
// StorageClass does not set the rootSquash parameter. Existing volumes and
// behaviour are therefore unchanged.
const defaultRootSquash = "root"

// normalizeRootSquash validates and normalizes the rootSquash StorageClass
// parameter that is written to the "root_squash" field of the DSM
// SYNO.Core.FileServ.NFS.SharePrivilege.save rule.
//
// The value space is EXACTLY the DSM vocabulary (verified on DSM 7.x against
// SYNO.Core.FileServ.NFS.SharePrivilege; any other literal is rejected by the
// API with error 2301 and the NodeStage NFS mount fails):
//
//   - "root":      UI "No mapping" -> exports no_root_squash (uid 0 keeps its
//     identity; non-root uids pass through untouched). Historical driver default.
//   - "admin":     UI "Map root to admin" -> root_squash (anonuid 1024).
//   - "guest":     UI "Map root to guest" -> root_squash (anonuid 1025).
//   - "all_admin": UI "Map all users to admin" -> all_squash (anonuid 1024).
//     EVERY NFS client uid (including non-root pod uids) is mapped to admin.
//   - "all_guest": UI "Map all users to guest" -> all_squash (anonuid 1025).
//
// Note "none"/"no_root_squash"/"root_squash"/"all_squash" are NOT accepted by
// the DSM API (they are the export keywords, not the API literals).
//
// Any other value (empty string included) falls back to defaultRootSquash.
func normalizeRootSquash(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "root", "admin", "guest", "all_admin", "all_guest":
		return v
	default:
		return defaultRootSquash
	}
}
