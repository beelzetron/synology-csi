package sanitytest

import (
	"fmt"
	"os"
	"testing"

	sanity "github.com/kubernetes-csi/csi-test/v5/pkg/sanity"

	"github.com/SynologyOpenSource/synology-csi/pkg/driver"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/service"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils/hostexec"
)

const (
	ConfigPath      = "./../../config/client-info.yml"
	SecretsFilePath = "./sanity-test-secret-file.yaml"
)

func TestSanity(t *testing.T) {
	if testing.Short() {
		t.Skip("CSI sanity requires a live DSM and config/client-info.yml; run without -short or use make test-sanity")
	}

	nodeID := "CSINode"

	endpointFile, err := os.CreateTemp("", "csi-gcs.*.sock")
	if err != nil {
		t.Fatal(err)
	}
	endpointFile.Close()
	defer os.Remove(endpointFile.Name())

	stagingPath, err := os.MkdirTemp("", "csi-gcs-staging")
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(stagingPath)

	targetPath, err := os.MkdirTemp("", "csi-gcs-target")
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(targetPath)

	info, err := common.LoadConfig(ConfigPath)
	if err != nil {
		t.Fatal(fmt.Sprintf("Failed to read config: %v", err))
	}

	dsmService := service.NewDsmService()

	for _, client := range info.Clients {
		if err := dsmService.AddDsm(client); err != nil {
			t.Fatalf("add DSM %s: %v", client.Host, err)
		}
	}

	if dsmService.GetDsmsCount() == 0 {
		t.Fatal("No available DSM.")
	}

	cmdExecutor, err := hostexec.New(nil, "")
	if err != nil {
		t.Fatal(fmt.Sprintf("Failed to create command executor: %v\n", err))
	}
	tools := driver.NewTools(cmdExecutor)

	endpoint := "unix://" + endpointFile.Name()
	drv, err := driver.NewControllerAndNodeDriver(nodeID, endpoint, dsmService, tools, "")
	if err != nil {
		t.Fatal(fmt.Sprintf("Failed to create driver: %v\n", err))
	}
	grpcSrv := drv.Activate()
	t.Cleanup(func() { dsmService.RemoveAllDsms() })
	t.Cleanup(func() {
		if grpcSrv != nil {
			grpcSrv.Stop()
			grpcSrv.Wait()
		}
	})

	// Set configuration options as needed
	testConfig := sanity.NewTestConfig()
	testConfig.TargetPath = targetPath
	testConfig.StagingPath = stagingPath
	testConfig.Address = endpoint
	testConfig.SecretsFile = SecretsFilePath

	// Set Input parameters for test
	testConfig.TestVolumeParameters = map[string]string{
		"protocol":              "iscsi",
		"allowMultipleSessions": "true",
	}

	// testConfig.TestVolumeAccessType = "block" // raw block

	// Run test
	sanity.Test(t, testConfig)
}
