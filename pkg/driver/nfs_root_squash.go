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
// Accepted values:
//
//   - "none": no root squash. NFS requests from uid 0 keep their identity
//     server-side. This is required for kubelet's fsGroup chown to take effect
//     so non-root pods can read/write a share regardless of its ownership.
//   - "root": map remote root to the admin account (the historical driver
//     default). With root squash active, kubelet's root chown is squashed to an
//     anonymous identity server-side and cannot fix share ownership, which
//     leaves non-root pods vulnerable to EACCES.
//   - "admin": map remote root to the DSM admin account.
//   - "all": map all NFS users to an anonymous/guest identity.
//
// Any other value (empty string included) falls back to defaultRootSquash.
func normalizeRootSquash(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "none", "root", "admin", "all":
		return v
	default:
		return defaultRootSquash
	}
}
