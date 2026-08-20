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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/SynologyOpenSource/synology-csi/pkg/models"
)

func TestNormalizeRootSquash(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty falls back to default", value: "", want: "root"},
		{name: "root is accepted unchanged", value: "root", want: "root"},
		{name: "none disables root squash", value: "none", want: "none"},
		{name: "admin maps root to admin", value: "admin", want: "admin"},
		{name: "all maps everyone to guest", value: "all", want: "all"},
		{name: "None is case insensitive", value: "None", want: "none"},
		{name: "ROOT is case insensitive", value: "ROOT", want: "root"},
		{name: "surrounding whitespace is trimmed", value: "  none  ", want: "none"},
		{name: "unknown value falls back to default", value: "true", want: "root"},
		{name: "garbage value falls back to default", value: "everyone", want: "root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeRootSquash(tt.value); got != tt.want {
				t.Errorf("normalizeRootSquash(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestCreateVolume_RootSquashVolumeContext(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{
			name:   "rootSquash none is propagated normalized",
			params: map[string]string{"protocol": "nfs", "location": "/volume1", "rootSquash": "none"},
			want:   "none",
		},
		{
			name:   "rootSquash absent defaults to root",
			params: map[string]string{"protocol": "nfs", "location": "/volume1"},
			want:   "root",
		},
		{
			name:   "invalid rootSquash falls back to root",
			params: map[string]string{"protocol": "nfs", "location": "/volume1", "rootSquash": "broken"},
			want:   "root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDsmService{
				createVolumeFunc: func(spec *models.CreateK8sVolumeSpec) (*models.K8sVolumeRespSpec, error) {
					return &models.K8sVolumeRespSpec{
						VolumeId:    "test-volume-id",
						SizeInBytes: spec.Size,
						Protocol:    spec.Protocol,
						BaseDir:     "/volume1/nfsshare",
					}, nil
				},
			}
			cs := newTestControllerServer(mock)

			resp, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name: "test-volume",
				CapacityRange: &csi.CapacityRange{
					RequiredBytes: 1 * 1024 * 1024 * 1024, // 1GB
				},
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessMode: &csi.VolumeCapability_AccessMode{
							Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
						},
						AccessType: &csi.VolumeCapability_Mount{
							Mount: &csi.VolumeCapability_MountVolume{},
						},
					},
				},
				Parameters: tt.params,
			})
			if err != nil {
				t.Fatalf("CreateVolume failed: %v", err)
			}

			if resp == nil || resp.Volume == nil || resp.Volume.VolumeContext == nil {
				t.Fatal("CreateVolume returned no volume context")
			}
			if got := resp.Volume.VolumeContext["rootSquash"]; got != tt.want {
				t.Errorf("VolumeContext[rootSquash] = %q, want %q", got, tt.want)
			}
		})
	}
}
