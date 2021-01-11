// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2019 Renesas Inc.
// Copyright 2019 EPAM Systems Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package launcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/pkg/reexec"
	"github.com/jlaffaye/ftp"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runc/libcontainer/specconv"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/sha3"

	amqp "aos_servicemanager/amqphandler"
	"aos_servicemanager/config"
	"aos_servicemanager/fcrypt"
	"aos_servicemanager/monitoring"
	"aos_servicemanager/networkmanager"
	"aos_servicemanager/platform"
	"aos_servicemanager/resourcemanager"
)

/*******************************************************************************
 * Types
 ******************************************************************************/

// Generates test image with python script
type pythonImage struct {
	serviceID string
	version   int
}

// Generates test image with iperf server
type iperfImage struct {
}

// Generates test image with ftp server
type ftpImage struct {
	ftpDir       string
	storageLimit uint64
	stateLimit   uint64
	tmpLimit     uint64
	layersDigest []digest.Digest
}

// Test monitor info
type testMonitorInfo struct {
	serviceID string
	config    monitoring.ServiceMonitoringConfig
}

// Test monitor
type testMonitor struct {
	startChannel chan *testMonitorInfo
	stopChannel  chan string
}

type stateRequest struct {
	serviceID    string
	defaultState bool
}

// Test sender
type testSender struct {
	statusChannel       chan amqp.ServiceInfo
	stateRequestChannel chan stateRequest
}

type testServiceProvider struct {
	sync.Mutex
	services      map[string]*Service
	usersServices []*UsersService
}

type testLayerProvider struct {
}

type testDeviceManager struct {
	sync.Mutex
	isValid bool
}

type fakeFcrypt struct {
}

/*******************************************************************************
 * Vars
 ******************************************************************************/

var serviceProvider = testServiceProvider{services: make(map[string]*Service)}
var layerProviderForTest = testLayerProvider{}
var networkProvider *networkmanager.NetworkManager

var deviceManager = testDeviceManager{isValid: true}

var chains []amqp.CertificateChain
var certs []amqp.Certificate

var tmpDir string
var testDir string

/*******************************************************************************
 * Init
 ******************************************************************************/

func init() {
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: false,
		TimestampFormat:  "2006-01-02 15:04:05.000",
		FullTimestamp:    true})
	log.SetLevel(log.DebugLevel)
	log.SetOutput(os.Stdout)
}

/*******************************************************************************
 * Main
 ******************************************************************************/

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}

	if err := setup(); err != nil {
		log.Fatalf("Error setting up: %s", err)
	}

	ret := m.Run()

	if err := cleanup(); err != nil {
		log.Fatalf("Error cleaning up: %s", err)
	}

	os.Exit(ret)
}

/*******************************************************************************
 * Tests
 ******************************************************************************/

func TestInstallRemove(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	numInstallServices := 10
	numUninstallServices := 5

	// install services
	for i := 0; i < numInstallServices; i++ {
		launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("service%d", i)}, chains, certs)
	}
	// remove services
	for i := 0; i < numUninstallServices; i++ {
		launcher.UninstallService(fmt.Sprintf("service%d", i))
	}

	for i := 0; i < numInstallServices+numUninstallServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s", status.Error, status.ID)
		}
	}

	services, err := launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}
	if len(services) != numInstallServices-numUninstallServices {
		t.Errorf("Wrong service quantity")
	}
	for _, service := range services {
		if service.Status != "installed" {
			t.Errorf("Service %s error status: %s", service.ID, service.Status)
		}
	}

	time.Sleep(time.Second * 2)

	// remove remaining services
	for i := numUninstallServices; i < numInstallServices; i++ {
		launcher.UninstallService(fmt.Sprintf("service%d", i))
	}

	for i := 0; i < numInstallServices-numUninstallServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
		}
	}

	services, err = launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}
	if len(services) != 0 {
		t.Errorf("Wrong service quantity")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestRemoveAllServices(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	numInstallServices := 10

	// install services
	for i := 0; i < numInstallServices; i++ {
		launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("service%d", i)}, chains, certs)
	}

	for i := 0; i < numInstallServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s", status.Error, status.ID)
		}
	}

	services, err := launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}

	if len(services) != numInstallServices {
		t.Errorf("Wrong service quantity. Actual %d, Expected %d", len(services), numInstallServices)
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}

	services, err = launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}

	if len(services) != 0 {
		t.Errorf("Wrong service quantity. Actual: %d, Expected 0", len(services))
	}
}

func TestCheckServicesConsistency(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	numInstallServices := 10

	// install services
	for i := 0; i < numInstallServices; i++ {
		launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("service%d", i)}, chains, certs)
	}

	for i := 0; i < numInstallServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s", status.Error, status.ID)
		}
	}

	services, err := launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}

	if len(services) != numInstallServices {
		t.Errorf("Wrong service quantity. Actual %d, Expected %d", len(services), numInstallServices)
	}

	if err = launcher.CheckServicesConsistency(); err != nil {
		t.Error("Expected services to be consistent")
	}

	cmd := exec.Command("rm", "-rf", path.Join(testDir, "storage"))
	if res, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Can't remove services dir contents: %s %s", err, res)
	}

	if err = launcher.CheckServicesConsistency(); err == nil {
		t.Error("Expected services to be inconsistent")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestAutoStart(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	numServices := 5

	// install services
	for i := 0; i < numServices; i++ {
		launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("service%d", i)}, chains, certs)
	}

	for i := 0; i < numServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s", status.Error, status.ID)
		}
	}

	launcher.Close()

	time.Sleep(time.Second * 2)

	launcher, err = newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	time.Sleep(time.Second * 2)

	services, err := launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}
	if len(services) != numServices {
		t.Errorf("Wrong service quantity")
	}
	for _, service := range services {
		if service.Status != "installed" {
			t.Errorf("Service %s error status: %s", service.ID, service.Status)
		}
	}

	// remove services
	for i := 0; i < numServices; i++ {
		launcher.UninstallService(fmt.Sprintf("service%d", i))
	}

	for i := 0; i < numServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s, version: %d", status.Error, status.ID, status.AosVersion)
		}
	}

	services, err = launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}
	if len(services) != 0 {
		t.Errorf("Wrong service quantity")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestErrors(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	// test AosVersion mistmatch

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 5}}, chains, certs)
	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 4}}, chains, certs)
	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 6}}, chains, certs)

	for i := 0; i < 3; i++ {
		status := <-sender.statusChannel
		switch {
		case status.AosVersion == 5 && status.Error != "":
			t.Errorf("%s, service ID %s, AosVersion: %d", status.Error, status.ID, status.AosVersion)
		case status.AosVersion == 4 && status.Error == "":
			t.Errorf("Service %s AosVersion %d should not be installed", status.ID, status.AosVersion)
		case status.AosVersion == 6 && status.Error != "":
			t.Errorf("%s, service ID %s, AosVersion: %d", status.Error, status.ID, status.AosVersion)
		}
	}

	services, err := launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}
	if len(services) != 1 {
		t.Errorf("Wrong service quantity: %d", len(services))
	} else if services[0].AosVersion != 6 {
		t.Errorf("Wrong service version")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestUpdate(t *testing.T) {
	sender := newTestSender()
	imageDownloader := new(pythonImage)

	launcher, err := newTestLauncher(imageDownloader, sender, nil, networkProvider)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	serverAddr, err := net.ResolveUDPAddr("udp", ":10001")
	if err != nil {
		t.Fatalf("Can't create resolve UDP address: %s", err)
	}

	serverConn, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		t.Fatalf("Can't listen UDP: %s", err)
	}
	defer serverConn.Close()

	imageDownloader.version = 0
	imageDownloader.serviceID = "service0"
	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 0}}, chains, certs)

	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, version: %d", status.Error, status.ID, status.AosVersion)
	}

	if err := serverConn.SetReadDeadline(time.Now().Add(time.Second * 30)); err != nil {
		t.Fatalf("Can't set read deadline: %s", err)
	}

	buf := make([]byte, 1024)

	n, _, err := serverConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("Can't read from UDP: %s", err)
	} else {
		message := string(buf[:n])

		if message != "service0, version: 0" {
			t.Fatalf("Wrong service content: %s", message)
		}
	}

	imageDownloader.version = 1
	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 1}}, chains, certs)

	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, version: %d", status.Error, status.ID, status.AosVersion)
	}

	n, _, err = serverConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("Can't read from UDP: %s", err)
	} else {
		message := string(buf[:n])

		if message != "service0, version: 1" {
			t.Fatalf("Wrong service content: %s", message)
		}
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestDeviceManagementNotValidOnStartup(t *testing.T) {
	sender := newTestSender()

	// set fake resource system to invalid state (UT emulation)
	deviceManager.isValid = false

	// create launcher instance
	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	defer func() {
		deviceManager.isValid = true
		launcher.Close()
	}()

	// run stored service configuration. In case if service is invalid we do not start services,
	// but return no error.
	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Error executing SetUsers command")
	}
}

func TestDeviceManagementRequestDeviceFail(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	defer func() {
		deviceManager.isValid = true
		launcher.Close()
	}()

	// run stored service configuration only in case system resources are valid
	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("SM can start services when device resources are invalid")
	}

	// set fake resource system to invalid state (UT emulation)
	deviceManager.isValid = false

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 1}}, chains, certs)

	// wait while service will be installed and tried to run
	// it should be failed because service requests random device
	// according to aos service configuration that generates on mocked download operation
	var status amqp.ServiceInfo
	if status = <-sender.statusChannel; status.Error == "" {
		t.Fatalf("SM can remove service when device resource is not released")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestNetworkSpeed(t *testing.T) {
	t.Skip("Skip due to functionality not temporary implemented")

	sender := newTestSender()

	launcher, err := newTestLauncher(new(iperfImage), sender, nil, networkProvider)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	numServices := 2

	for i := 0; i < numServices; i++ {
		launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("service%d", i)}, chains, certs)
	}

	for i := 0; i < numServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
		}
	}

	for i := 0; i < numServices; i++ {
		serviceID := fmt.Sprintf("service%d", i)

		service, err := launcher.serviceProvider.GetService(serviceID)
		if err != nil {
			t.Errorf("Can't get service: %s", err)
			continue
		}

		addr, err := networkProvider.GetServiceIP(service.ID, service.ServiceProvider)
		if err != nil {
			t.Errorf("Can't get ip address: %s", err)
			continue
		}

		output, err := exec.Command("iperf", "-c"+addr, "-d", "-r", "-t2", "-yc").Output()
		if err != nil {
			t.Errorf("Iperf failed: %s", err)
			continue
		}

		ulSpeed := -1
		dlSpeed := -1

		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			result := strings.Split(line, ",")
			if len(result) >= 9 {
				if result[4] == "5001" {
					value, err := strconv.ParseInt(result[8], 10, 64)
					if err != nil {
						t.Errorf("Can't parse ul speed: %s", err)
						continue
					}
					ulSpeed = int(value) / 1000
				} else {
					value, err := strconv.ParseUint(result[8], 10, 64)
					if err != nil {
						t.Errorf("Can't parse ul speed: %s", err)
						continue
					}
					dlSpeed = int(value) / 1000
				}
			}
		}

		if ulSpeed == -1 || dlSpeed == -1 {
			t.Error("Can't determine ul/dl speed")
		}

		if ulSpeed > 4096*1.5 || dlSpeed > 8192*1.5 {
			t.Errorf("Speed limit exceeds: dl %d, ul %d", dlSpeed, ulSpeed)
		}
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestVisPermissions(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 0}}, chains, certs)

	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, version: %d", status.Error, status.ID, status.AosVersion)
	}

	service, ok := serviceProvider.services["service0"]
	if !ok {
		t.Fatalf("Service not found")
	}

	if service.Permissions != `{"*": "rw", "123": "rw"}` {
		t.Fatalf("Permissions mismatch")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestUsersServices(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	numUsers := 3
	numServices := 3

	for i := 0; i < numUsers; i++ {
		users := []string{fmt.Sprintf("user%d", i)}

		if err = launcher.SetUsers(users); err != nil {
			t.Fatalf("Can't set users: %s", err)
		}

		services, err := launcher.serviceProvider.GetUsersServices(users)
		if err != nil {
			t.Fatalf("Can't get users services: %s", err)
		}
		if len(services) != 0 {
			t.Fatalf("Wrong service quantity")
		}

		// install services
		for j := 0; j < numServices; j++ {
			launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("user%d_service%d", i, j)}, chains, certs)
		}
		for i := 0; i < numServices; i++ {
			if status := <-sender.statusChannel; status.Error != "" {
				t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
			}
		}

		time.Sleep(time.Second * 2)

		services, err = launcher.serviceProvider.GetServices()
		if err != nil {
			t.Fatalf("Can't get services: %s", err)
		}

		count := 0
		for _, service := range services {
			if service.State == stateRunning {
				_, err = launcher.serviceProvider.GetUsersService(users, service.ID)
				if err != nil && !strings.Contains(err.Error(), "not exist") {
					t.Errorf("Can't check users service: %s", err)
				}

				if err != nil {
					t.Errorf("Service doesn't belong to users: %s", service.ID)
				}

				count++
			}
		}

		if count != numServices {
			t.Fatalf("Wrong running services count")
		}
	}

	for i := 0; i < numUsers; i++ {
		users := []string{fmt.Sprintf("user%d", i)}

		if err = launcher.SetUsers(users); err != nil {
			t.Fatalf("Can't set users: %s", err)
		}

		time.Sleep(time.Second * 2)

		services, err := launcher.serviceProvider.GetServices()
		if err != nil {
			t.Fatalf("Can't get services: %s", err)
		}

		count := 0
		for _, service := range services {
			if service.State == stateRunning {
				_, err = launcher.serviceProvider.GetUsersService(users, service.ID)
				if err != nil && !strings.Contains(err.Error(), "not exist") {
					t.Errorf("Can't check users service: %s", err)
				}

				if err != nil {
					t.Errorf("Service doesn't belong to users: %s", service.ID)
				}

				count++
			}
		}

		if count != numServices {
			t.Fatalf("Wrong running services count")
		}
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestServiceTTL(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	numServices := 3

	if err = launcher.SetUsers([]string{"user0"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	// install services
	for i := 0; i < numServices; i++ {
		launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("service%d", i)}, chains, certs)
	}
	for i := 0; i < numServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
		}
	}

	services, err := launcher.serviceProvider.GetServices()
	if err != nil {
		t.Fatalf("Can't get services: %s", err)
	}

	for _, service := range services {
		if err = launcher.serviceProvider.SetServiceStartTime(service.ID, service.StartAt.Add(-time.Hour*24*30)); err != nil {
			t.Errorf("Can't set service start time: %s", err)
		}
	}

	if err = launcher.SetUsers([]string{"user1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	services, err = launcher.serviceProvider.GetServices()
	if err != nil {
		t.Fatalf("Can't get services: %s", err)
	}

	if len(services) != 0 {
		t.Fatal("Wrong service quantity")
	}

	if len(serviceProvider.usersServices) != 0 {
		t.Fatalf("Wrong users quantity: %d", len(serviceProvider.usersServices))
	}
}

func TestServiceMonitoring(t *testing.T) {
	sender := newTestSender()

	monitor, err := newTestMonitor()
	if err != nil {
		t.Fatalf("Can't create monitor: %s", err)
	}

	launcher, err := newTestLauncher(new(pythonImage), sender, monitor, networkProvider)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"user0"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	serviceAlerts := amqp.ServiceAlertRules{
		RAM: &config.AlertRule{
			MinTimeout:   config.Duration{Duration: 30 * time.Second},
			MinThreshold: 0,
			MaxThreshold: 80},
		CPU: &config.AlertRule{
			MinTimeout:   config.Duration{Duration: 1 * time.Minute},
			MinThreshold: 0,
			MaxThreshold: 20},
		UsedDisk: &config.AlertRule{
			MinTimeout:   config.Duration{Duration: 5 * time.Minute},
			MinThreshold: 0,
			MaxThreshold: 20}}

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "Service1", AlertRules: &serviceAlerts}, chains, certs)
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	select {
	case info := <-monitor.startChannel:
		if info.serviceID != "Service1" {
			t.Fatalf("Wrong service ID: %s", info.serviceID)
		}

		if !reflect.DeepEqual(info.config.ServiceRules, &serviceAlerts) {
			t.Fatalf("Wrong service alert rules")
		}

	case <-time.After(1000 * time.Millisecond):
		t.Errorf("Waiting for service monitor timeout")
	}

	launcher.UninstallService("Service1")
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	select {
	case serviceID := <-monitor.stopChannel:
		if serviceID != "Service1" {
			t.Fatalf("Wrong service ID: %s", serviceID)
		}

	case <-time.After(2000 * time.Millisecond):
		t.Errorf("Waiting for service monitor timeout")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestServiceStorage(t *testing.T) {
	sender := newTestSender()

	// Set limit for 2 files + some buffer
	launcher, err := newTestLauncher(&ftpImage{"/home/service/storage", 8192*2 + 8192*20, 0, 0, nil}, sender, nil, networkProvider)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 0}}, chains, certs)
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	// Wait ftp server ready
	time.Sleep(2 * time.Second)

	ftp, err := launcher.connectToFtp("service0")
	if err != nil {
		t.Fatalf("Can't connect to ftp: %s", err)
	}
	defer ftp.Quit()

	service, err := launcher.serviceProvider.GetService("service0")
	if err != nil {
		t.Errorf("Can't get service: %s", err)
	}

	testData := make([]byte, 8192)

	if err := ftp.Stor("test1.dat", bytes.NewReader(testData)); err != nil {
		t.Errorf("Can't write file: %s", err)
	}

	diskUsage, err := platform.GetUserFSQuotaUsage(launcher.config.StorageDir, service.UID, service.GID)
	if err != nil {
		t.Errorf("Can't get disk usage: %s", err)
	}

	// file size + storage folders and workdir
	if diskUsage != (8192 + 4096 + 8192*5) {
		t.Errorf("Wrong disk usage value: %d", diskUsage)
	}

	if err := ftp.Stor("test2.dat", bytes.NewReader(testData)); err != nil {
		t.Errorf("Can't write file: %s", err)
	}

	diskUsage, err = platform.GetUserFSQuotaUsage(launcher.config.StorageDir, service.UID, service.GID)
	if err != nil {
		t.Errorf("Can't get disk usage: %s", err)
	}

	// 2 files size + storage folders and workdir
	if diskUsage != (8192*2 + 4096 + 8192*5) {
		t.Errorf("Wrong disk usage value: %d", diskUsage)
	}

	bigTestData := make([]byte, 8192*20)
	if err := ftp.Stor("test3.dat", bytes.NewReader(bigTestData)); err == nil {
		t.Errorf("Unexpected nil error")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestServiceState(t *testing.T) {
	sender := newTestSender()

	launcher, err := newTestLauncher(&ftpImage{"/", 1024 * 24, 256, 0, nil}, sender, nil, networkProvider)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 0}}, chains, certs)
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	// Wait ftp server ready
	time.Sleep(2 * time.Second)

	ftp, err := launcher.connectToFtp("service0")
	if err != nil {
		t.Fatalf("Can't connect to ftp: %s", err)
	}
	defer ftp.Quit()

	// Check new state accept

	stateData := ""

	if err := ftp.Stor("state.dat", bytes.NewReader([]byte(stateData))); err != nil {
		t.Errorf("Can't write file: %s", err)
	}

	time.Sleep(500 * time.Millisecond)

	stateData = "Hello"

	if err := ftp.Stor("state.dat", bytes.NewReader([]byte(stateData))); err != nil {
		t.Errorf("Can't write file: %s", err)
	}

	select {
	case newState := <-launcher.NewStateChannel:
		if newState.State != stateData {
			t.Errorf("Wrong state: %s", newState.State)
		}

		launcher.StateAcceptance(amqp.StateAcceptance{Result: "accepted"}, newState.CorrelationID)

	case <-time.After(2 * time.Second):
		t.Error("No new state event")
	}

	// Check new state reject

	stateData = "Hello again"

	if err := ftp.Stor("state.dat", bytes.NewReader([]byte(stateData))); err != nil {
		t.Errorf("Can't write file: %s", err)
	}

	select {
	case newState := <-launcher.NewStateChannel:
		if newState.State != stateData {
			t.Errorf("Wrong state: %s", newState.State)
		}

		launcher.StateAcceptance(amqp.StateAcceptance{Result: "rejected", Reason: "just because"}, newState.CorrelationID)

	case <-time.After(2 * time.Second):
		t.Error("No new state event")
	}

	select {
	case <-sender.stateRequestChannel:
		stateData = "Hello"
		calcSum := sha3.Sum224([]byte(stateData))

		launcher.UpdateState(amqp.UpdateState{
			ServiceID: "service0",
			State:     stateData,
			Checksum:  hex.EncodeToString(calcSum[:])})

		time.Sleep(1 * time.Second)

	case <-time.After(2 * time.Second):
		t.Error("No state request event")
	}

	// Wait ftp server ready
	time.Sleep(5 * time.Second)

	if ftp, err = launcher.connectToFtp("service0"); err != nil {
		t.Fatalf("Can't connect to ftp: %s", err)
	}
	defer ftp.Quit()

	response, err := ftp.Retr("state.dat")
	if err != nil {
		t.Errorf("Can't retrieve state file: %s", err)
	} else {
		serviceState, err := ioutil.ReadAll(response)
		if err != nil {
			t.Errorf("Can't retrieve state file: %s", err)
		}

		if string(serviceState) != stateData {
			t.Errorf("Wrong state: %s", serviceState)
		}

		response.Close()
	}

	// Check state after update

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 1}}, chains, certs)
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	// Wait ftp server ready
	time.Sleep(5 * time.Second)

	if ftp, err = launcher.connectToFtp("service0"); err != nil {
		t.Fatalf("Can't connect to ftp: %s", err)
	}
	defer ftp.Quit()

	if response, err = ftp.Retr("state.dat"); err != nil {
		t.Errorf("Can't retrieve state file: %s", err)
	} else {
		serviceState, err := ioutil.ReadAll(response)
		if err != nil {
			t.Errorf("Can't retrieve state file: %s", err)
		}

		if string(serviceState) != stateData {
			t.Errorf("Wrong state: %s", serviceState)
		}

		response.Close()
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

func TestTmpDir(t *testing.T) {
	sender := newTestSender()

	// Test no tmp limit

	launcher, err := newTestLauncher(&ftpImage{"/tmp", 0, 0, 0, nil}, sender, nil, networkProvider)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 0}}, chains, certs)
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	// Wait ftp server ready
	time.Sleep(2 * time.Second)

	ftp, err := launcher.connectToFtp("service0")
	if err == nil {
		t.Error("Unexpected nil error")
	}

	launcher.Close()

	// Test tmp limit

	if launcher, err = newTestLauncher(&ftpImage{"/tmp", 0, 0, 8192, nil}, sender, nil, networkProvider); err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service1",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 0}}, chains, certs)
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	// Wait ftp server ready
	time.Sleep(2 * time.Second)

	ftp, err = launcher.connectToFtp("service1")
	if err != nil {
		t.Fatalf("Can't connect to ftp: %s", err)
	}

	testData := make([]byte, 8192)

	if err := ftp.Stor("test1.dat", bytes.NewReader(testData)); err != nil {
		t.Errorf("Can't write file: %s", err)
	}

	if err := ftp.Stor("test2.dat", bytes.NewReader(testData)); err == nil {
		t.Error("Unexpected nil error")
	}

	ftp.Quit()

	// Test tmp works after restarts

	launcher.Close()

	if launcher, err = newTestLauncher(&ftpImage{"/tmp", 0, 0, 0, nil}, sender, nil, networkProvider); err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	// Wait ftp server ready
	time.Sleep(2 * time.Second)

	ftp, err = launcher.connectToFtp("service1")
	if err != nil {
		t.Fatalf("Can't connect to ftp: %s", err)
	}

	if err := ftp.Stor("test3.dat", bytes.NewReader(testData)); err != nil {
		t.Errorf("Can't write file: %s", err)
	}

	ftp.Quit()

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}

	launcher.Close()
}

func TestSpec(t *testing.T) {
	if err := generateConfig(testDir); err != nil {
		t.Fatalf("Can't generate service spec: %s", err)
	}

	spec, err := loadServiceSpec(path.Join(testDir, ocConfigFile))
	if err != nil {
		t.Fatalf("Can't load service spec: %s", err)
	}
	defer func() {
		if err := spec.save(); err != nil {
			t.Fatalf("Can't save service spec: %s", err)
		}
	}()

	aosConf := generateAosSrvConfig()
	randomDevice := Device{Name: "random", Permissions: "rwm"}
	aosConf.Devices = []Device{randomDevice}

	if err != nil {
		t.Fatalf("Can't load aos service config: %s", err)
	}

	// add device and group from resource manager

	for _, device := range aosConf.Devices {
		// request device resources
		deviceResource, err := deviceManager.RequestDeviceResourceByName(device.Name)
		if err != nil {
			t.Fatalf("Can't request device resource: %s", err)
		}

		for _, hostDevice := range deviceResource.HostDevices {
			// use absolute path from host devices and permissions from aos configuration
			if err = spec.addHostDevice(Device{hostDevice, device.Permissions}); err != nil {
				t.Fatalf("Can't add host device: %s", err)
			}
		}

		for _, group := range deviceResource.Groups {
			if err = spec.addAdditionalGroup(group); err != nil {
				t.Fatalf("Can't add host group: %s", err)
			}
		}
	}

	found := false

	var device runtimespec.LinuxDevice

	for _, device = range spec.ocSpec.Linux.Devices {
		if strings.Contains(device.Path, path.Join("/dev/", randomDevice.Name)) {
			found = true

			break
		}
	}

	if !found {
		t.Fatal("Device not found")
	}

	found = false

	for _, resource := range spec.ocSpec.Linux.Resources.Devices {
		if resource.Major == nil || resource.Minor == nil {
			continue
		}

		if *resource.Major == device.Major && *resource.Minor == device.Minor {
			found = true

			if !resource.Allow {
				t.Error("Resource is not allowed")
			}
		}
	}

	if !found {
		t.Fatal("Resource not found")
	}

	// add group

	groupName := "audio"

	group, err := user.LookupGroup(groupName)
	if err != nil {
		t.Fatalf("Can't lookup group: %s", err)
	}

	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		t.Fatalf("Can't parse GID: %s", err)
	}

	if err = spec.addAdditionalGroup(groupName); err != nil {
		t.Fatalf("Can't add group: %s", err)
	}

	found = false

	for _, serviceGID := range spec.ocSpec.Process.User.AdditionalGids {
		if uint32(gid) == serviceGID {
			found = true

			break
		}
	}

	if !found {
		t.Error("Group not found")
	}
}

func TestSpecFromImageConfig(t *testing.T) {
	configFilePath := path.Join(testDir, "config.json")
	_, err := generateSpecFromImageConfig("no_file", configFilePath)
	if err == nil {
		t.Errorf("Should be error no such file or director")
	}

	imgConfig, err := generateImageConfig()
	if err != nil {
		log.Fatalf("Error creating OCI Image config %s", err)
	}

	imgConfig.OS = "Windows"
	configFile, err := saveImageConfig(testDir, imgConfig)
	if err != nil {
		log.Fatalf("Error save OCI Image config %s", err)
	}

	_, err = generateSpecFromImageConfig(configFile, configFilePath)
	if err == nil {
		t.Errorf("Should be error unsupported OS in image config")
	}

	imgConfig.OS = "linux"
	configFile, err = saveImageConfig(testDir, imgConfig)
	if err != nil {
		log.Fatalf("Error save OCI Image config %s", err)
	}

	runtimeSpec, err := generateSpecFromImageConfig(configFile, configFilePath)
	if err != nil {
		t.Errorf("Error generating OCI runtime spec %s", err)
	}

	originalCmd := []string{"/bin/my-app-binary", "--foreground", "--config", "/etc/my-app.d/default.cfg"}
	if false == reflect.DeepEqual(runtimeSpec.ocSpec.Process.Args, originalCmd) {
		t.Errorf("Error crating args from config")
	}

	origEnv := []string{"PATH=/usr/local/sbin", "TERM", "FOO=oci_is_a", "BAR=well_written_spec", "MY_VAR"}
	if false == reflect.DeepEqual(runtimeSpec.ocSpec.Process.Env, origEnv) {
		t.Errorf("Error crating env from config")
	}
}

func TestValidateUnpackedImage(t *testing.T) {
	fakeImageFolder := path.Join(testDir, "fakeImage")
	if err := os.MkdirAll(fakeImageFolder, 0755); err != nil {
		log.Fatalf("Can't create fakeImage Folder %s", err)
	}

	if err := generateFakeImage(fakeImageFolder); err != nil {
		log.Fatalf("Can't generate fakeImage %s", err)
	}

	if err := validateUnpackedImage(fakeImageFolder); err != nil {
		t.Errorf("Error validateUnpackedImage %s", err)
	}
}

func TestServiceWithLayers(t *testing.T) {
	layerDir := path.Join(testDir, "layerStorage", "layer1")
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		t.Fatalf("Can't create layer dir: %s", err)
	}

	file, err := os.Create(path.Join(layerDir, "someFile.txt"))
	if err != nil {
		t.Fatalf("Can't create layer file: %s", err)
	}
	defer file.Close()

	testString := "This is test layer file"
	_, err = file.Write([]byte(testString))
	if err != nil {
		t.Fatalf("Can't write layer file: %s", err)
	}

	sender := newTestSender()

	digests := []digest.Digest{digest.NewDigestFromBytes(digest.SHA256, []byte(testString))}

	launcher, err := newTestLauncher(&ftpImage{"/layer1", 0, 0, 0, digests}, sender, nil, networkProvider)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	launcher.InstallService(amqp.ServiceInfoFromCloud{ID: "service0",
		VersionFromCloud: amqp.VersionFromCloud{AosVersion: 0}}, chains, certs)
	if status := <-sender.statusChannel; status.Error != "" {
		t.Errorf("%s, service ID %s, aosVersion: %d", status.Error, status.ID, status.AosVersion)
	}

	// Wait ftp server ready
	time.Sleep(3 * time.Second)

	ftp, err := launcher.connectToFtp("service0")
	if err != nil {
		t.Error("Can't connect to server")
		return
	}

	resp, err := ftp.Retr("someFile.txt")
	if err != nil {
		t.Error("No files")
	}
	defer resp.Close()

	ftp.Quit()

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}

	launcher.Close()
}

func TestSetServiceResources(t *testing.T) {
	launcher, err := newTestLauncher(new(pythonImage), nil, nil, nil)
	if err != nil {
		t.Fatalf("Can't create test launcher: %s", err)
	}

	spec := &serviceSpec{runtimeFileName: "fileName", ocSpec: *specconv.Example()}

	spec.ocSpec.Process.Env = []string{"ENV1", "ENV2=HELLO"}
	spec.ocSpec.Mounts = []runtimespec.Mount{runtimespec.Mount{Destination: "/orig1",
		Source: "/orig2"},
	}
	spec.ocSpec.Process.User.AdditionalGids = []uint32{1000}

	if err := launcher.setServiceResources(spec, []string{"dbus", "wifi"}); err != nil {
		t.Error("Can't setServiceResources: ", err)
	}

	etalonEnv := []string{"ENV1", "ENV2=HELLO", "BUS_SYSTEM_BUS_ADDRESS=unix:path=/var/run/dbus/system_bus_socket"}
	if false == reflect.DeepEqual(etalonEnv, spec.ocSpec.Process.Env) {
		t.Error("incorrect env")
	}

	etalonMounts := []runtimespec.Mount{
		runtimespec.Mount{
			Destination: "/orig1",
			Source:      "/orig2"},
		runtimespec.Mount{
			Destination: "/destination",
			Source:      "/source",
			Type:        "bind",
		},
	}
	if false == reflect.DeepEqual(etalonMounts, spec.ocSpec.Mounts) {
		t.Error("incorrect env")
	}

	etalonGids := []uint32{1000, 2}
	if false == reflect.DeepEqual(etalonGids, spec.ocSpec.Process.User.AdditionalGids) {
		t.Error("incorrect env")
	}
}

func TestNotStartIfInvalidResource(t *testing.T) {
	sender := newTestSender()

	// set fake resource system to valid state (UT emulation)
	deviceManager.isValid = true

	launcher, err := newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	numServices := 5

	// install services
	for i := 0; i < numServices; i++ {
		launcher.InstallService(amqp.ServiceInfoFromCloud{ID: fmt.Sprintf("service%d", i)}, chains, certs)
	}

	for i := 0; i < numServices; i++ {
		status := <-sender.statusChannel
		if status.Error != "" {
			t.Errorf("%s, service ID %s", status.Error, status.ID)
		}
		if serviceProvider.services[status.ID].State != stateRunning {
			t.Errorf("Service %s state is invalid : %s", status.ID, serviceProvider.services[status.ID].State)
		}

	}

	launcher.Close()

	// set fake resource system to valid state (UT emulation)
	deviceManager.isValid = false

	defer func() {
		deviceManager.isValid = true
	}()

	time.Sleep(time.Second * 2)

	launcher, err = newTestLauncher(new(pythonImage), sender, nil, nil)
	if err != nil {
		t.Fatalf("Can't create launcher: %s", err)
	}
	defer launcher.Close()

	if err = launcher.SetUsers([]string{"User1"}); err != nil {
		t.Fatalf("Can't set users: %s", err)
	}

	time.Sleep(time.Second * 2)

	services, err := launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}
	if len(services) != numServices {
		t.Errorf("Resource config is invalid, services shouldn't start")
	}

	for _, service := range services {
		if service.Status != "installed" {
			t.Errorf("Service %s error status: %s", service.ID, service.Status)
		}
		if serviceProvider.services[service.ID].State == stateRunning {
			t.Errorf("Service %s should be stopped", service.ID)
		}
	}

	// remove services
	for i := 0; i < numServices; i++ {
		launcher.UninstallService(fmt.Sprintf("service%d", i))
	}

	for i := 0; i < numServices; i++ {
		if status := <-sender.statusChannel; status.Error != "" {
			t.Errorf("%s, service ID %s, version: %d", status.Error, status.ID, status.AosVersion)
		}
	}

	services, err = launcher.GetServicesInfo()
	if err != nil {
		t.Errorf("Can't get services info: %s", err)
	}
	if len(services) != 0 {
		t.Errorf("Wrong service quantity")
	}

	if err := launcher.RemoveAllServices(); err != nil {
		t.Errorf("Can't cleanup all services: %s", err)
	}
}

/*******************************************************************************
 * Interfaces
 ******************************************************************************/

func newTestLauncher(
	downloader downloader, sender Sender,
	monitor ServiceMonitor, network NetworkProvider) (launcher *Launcher, err error) {
	launcher, err = New(&config.Config{WorkingDir: testDir, StorageDir: path.Join(testDir, "storage"),
		DefaultServiceTTL: 30}, downloader,
		sender, &serviceProvider, &layerProviderForTest, monitor, network, &deviceManager)
	if err != nil {
		return nil, err
	}

	return launcher, err
}

func (downloader pythonImage) DownloadAndDecrypt(packageInfo amqp.DecryptDataStruct,
	chains []amqp.CertificateChain, certs []amqp.Certificate, decryptDir string) (outputFile string, err error) {
	imageDir, err := ioutil.TempDir("", "aos_")
	if err != nil {
		log.Error("Can't create image dir : ", err)
		return outputFile, err
	}
	defer os.RemoveAll(imageDir)

	// create dir
	if err := os.MkdirAll(path.Join(imageDir, "rootfs", "home"), 0755); err != nil {
		return outputFile, err
	}

	if err := generatePythonContent(imageDir); err != nil {
		return outputFile, err
	}

	fsDigest, err := generateFsLayer(imageDir, path.Join(imageDir, "rootfs"))
	if err != nil {
		return outputFile, err
	}

	aosSrvConfig := generateAosSrvConfig()
	aosSrvConfig.Quotas.VisPermissions = `{"*": "rw", "123": "rw"}`
	aosSrvConfig.Devices = []Device{{Name: "random", Permissions: "rwm"}}

	data, err := json.Marshal(aosSrvConfig)
	if err != nil {
		return outputFile, err
	}

	aosSrvConfigDigest, err := generateAndSaveDigest(path.Join(imageDir, "blobs"), data)
	if err != nil {
		return outputFile, err
	}

	ociImgSpec := imagespec.Image{}
	ociImgSpec.OS = "Linux"
	ociImgSpec.Config.Env = append(ociImgSpec.Config.Env, "PYTHONDONTWRITEBYTECODE=1")
	ociImgSpec.Config.Cmd = []string{"python3", "/home/service.py", downloader.serviceID,
		fmt.Sprintf("%d", downloader.version)}

	dataImgSpec, err := json.Marshal(ociImgSpec)
	if err != nil {
		return outputFile, err
	}

	imgSpecDigestDigest, err := generateAndSaveDigest(path.Join(imageDir, "blobs"), dataImgSpec)
	if err != nil {
		return outputFile, err
	}

	if err := genarateImageManfest(imageDir, &imgSpecDigestDigest, &aosSrvConfigDigest, &fsDigest, nil); err != nil {
		return outputFile, err
	}

	imageFile, err := ioutil.TempFile("", "aos_")
	if err != nil {
		return outputFile, err
	}
	outputFile = imageFile.Name()
	imageFile.Close()

	if err = packImage(imageDir, outputFile); err != nil {
		return outputFile, err
	}

	return outputFile, nil
}

func (downloader iperfImage) DownloadAndDecrypt(packageInfo amqp.DecryptDataStruct,
	chains []amqp.CertificateChain, certs []amqp.Certificate, decryptDir string) (outputFile string, err error) {
	imageDir, err := ioutil.TempDir("", "aos_")
	if err != nil {
		log.Error("Can't create image dir : ", err)
		return outputFile, err
	}
	defer os.RemoveAll(imageDir)

	// create dir
	if err := os.MkdirAll(path.Join(imageDir, "rootfs", "home"), 0755); err != nil {
		return outputFile, err
	}

	fsDigest, err := generateFsLayer(imageDir, path.Join(imageDir, "rootfs"))
	if err != nil {
		return outputFile, err
	}

	var uploadSpeed uint64 = 4096 * 1024
	var downloadSpeed uint64 = 8192 * 1024

	aosSrvConfig := generateAosSrvConfig()
	aosSrvConfig.Quotas.UploadSpeed = &uploadSpeed
	aosSrvConfig.Quotas.DownloadSpeed = &downloadSpeed
	aosSrvConfig.Devices = []Device{{Name: "random", Permissions: "rwm"}}

	data, err := json.Marshal(aosSrvConfig)
	if err != nil {
		return outputFile, err
	}

	aosSrvConfigDigest, err := generateAndSaveDigest(path.Join(imageDir, "blobs"), data)
	if err != nil {
		return outputFile, err
	}

	ociImgSpec := imagespec.Image{}
	ociImgSpec.OS = "Linux"
	ociImgSpec.Config.Cmd = []string{"iperf", "-s"}

	dataImgSpec, err := json.Marshal(ociImgSpec)
	if err != nil {
		return outputFile, err
	}

	imgSpecDigestDigest, err := generateAndSaveDigest(path.Join(imageDir, "blobs"), dataImgSpec)
	if err != nil {
		return outputFile, err
	}

	if err := genarateImageManfest(imageDir, &imgSpecDigestDigest, &aosSrvConfigDigest, &fsDigest, nil); err != nil {
		return outputFile, err
	}

	imageFile, err := ioutil.TempFile("", "aos_")
	if err != nil {
		return outputFile, err
	}
	outputFile = imageFile.Name()
	imageFile.Close()

	if err = packImage(imageDir, outputFile); err != nil {
		return outputFile, err
	}

	return outputFile, nil
}

func (downloader ftpImage) DownloadAndDecrypt(packageInfo amqp.DecryptDataStruct,
	chains []amqp.CertificateChain, certs []amqp.Certificate, decryptDir string) (outputFile string, err error) {
	imageDir, err := ioutil.TempDir("", "aos_")
	if err != nil {
		log.Error("Can't create image dir : ", err)
		return outputFile, err
	}
	defer os.RemoveAll(imageDir)

	// create dir
	if err := os.MkdirAll(path.Join(imageDir, "rootfs", "home"), 0755); err != nil {
		return outputFile, err
	}

	if err := generateFtpContent(imageDir, downloader.ftpDir); err != nil {
		return outputFile, err
	}

	fsDigest, err := generateFsLayer(imageDir, path.Join(imageDir, "rootfs"))
	if err != nil {
		return outputFile, err
	}

	aosSrvConfig := generateAosSrvConfig()

	if downloader.storageLimit > 0 {
		aosSrvConfig.Quotas.StorageLimit = &downloader.storageLimit
	}

	if downloader.stateLimit > 0 {
		aosSrvConfig.Quotas.StateLimit = &downloader.stateLimit
	}

	if downloader.tmpLimit > 0 {
		aosSrvConfig.Quotas.TmpLimit = &downloader.tmpLimit
	}

	data, err := json.Marshal(aosSrvConfig)
	if err != nil {
		return outputFile, err
	}

	aosSrvConfigDigest, err := generateAndSaveDigest(path.Join(imageDir, "blobs"), data)
	if err != nil {
		return outputFile, err
	}

	ociImgSpec := imagespec.Image{}
	ociImgSpec.OS = "Linux"
	ociImgSpec.Config.Env = append(ociImgSpec.Config.Env, "PYTHONDONTWRITEBYTECODE=1")
	ociImgSpec.Config.Cmd = []string{"python3", "/home/service.py"}

	dataImgSpec, err := json.Marshal(ociImgSpec)
	if err != nil {
		return outputFile, err
	}

	imgSpecDigestDigest, err := generateAndSaveDigest(path.Join(imageDir, "blobs"), dataImgSpec)
	if err != nil {
		return outputFile, err
	}

	if err := genarateImageManfest(imageDir, &imgSpecDigestDigest, &aosSrvConfigDigest,
		&fsDigest, downloader.layersDigest); err != nil {
		return outputFile, err
	}

	imageFile, err := ioutil.TempFile("", "aos_")
	if err != nil {
		return outputFile, err
	}
	outputFile = imageFile.Name()
	imageFile.Close()

	if err = packImage(imageDir, outputFile); err != nil {
		return outputFile, err
	}

	return outputFile, nil
}

func (crypt fakeFcrypt) ImportSessionKey(keyInfo fcrypt.CryptoSessionKeyInfo) (fcrypt.SymmetricContextInterface, error) {
	return nil, nil
}

func (crypt fakeFcrypt) CreateSignContext() (fcrypt.SignContextInterface, error) {
	return nil, nil
}

func newTestMonitor() (monitor *testMonitor, err error) {
	monitor = &testMonitor{}

	monitor.startChannel = make(chan *testMonitorInfo, 100)
	monitor.stopChannel = make(chan string, 100)

	return monitor, nil
}

func (monitor *testMonitor) StartMonitorService(serviceID string, monitorConfig monitoring.ServiceMonitoringConfig) (err error) {

	monitor.startChannel <- &testMonitorInfo{serviceID, monitorConfig}

	return nil
}

func (monitor *testMonitor) StopMonitorService(serviceID string) (err error) {
	monitor.stopChannel <- serviceID

	return nil
}

func newTestSender() (sender *testSender) {
	sender = &testSender{}

	sender.statusChannel = make(chan amqp.ServiceInfo, maxExecutedActions)
	sender.stateRequestChannel = make(chan stateRequest, 32)

	return sender
}

func (sender *testSender) SendServiceStatus(serviceStatus amqp.ServiceInfo) (err error) {
	sender.statusChannel <- serviceStatus

	return nil
}

func (sender *testSender) SendStateRequest(serviceID string, defaultState bool) (err error) {
	sender.stateRequestChannel <- stateRequest{serviceID, defaultState}

	return nil
}

func (serviceProvider *testServiceProvider) AddService(service Service) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	if _, ok := serviceProvider.services[service.ID]; ok {
		return fmt.Errorf("service %s already exists", service.ID)
	}

	serviceProvider.services[service.ID] = &service

	return nil
}

func (serviceProvider *testServiceProvider) UpdateService(service Service) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	if _, ok := serviceProvider.services[service.ID]; !ok {
		return fmt.Errorf("service %s does not exist", service.ID)
	}

	serviceProvider.services[service.ID] = &service

	return nil
}

func (serviceProvider *testServiceProvider) RemoveService(serviceID string) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	if _, ok := serviceProvider.services[serviceID]; !ok {
		return fmt.Errorf("service %s does not exist", serviceID)
	}

	delete(serviceProvider.services, serviceID)

	return nil
}

func (serviceProvider *testServiceProvider) GetService(serviceID string) (service Service, err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	servicePtr, ok := serviceProvider.services[serviceID]
	if !ok {
		return service, fmt.Errorf("service %s does not exist", serviceID)
	}

	return *servicePtr, nil
}

func (serviceProvider *testServiceProvider) GetServices() (services []Service, err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, servicePtr := range serviceProvider.services {
		services = append(services, *servicePtr)
	}

	return services, nil
}

func (serviceProvider *testServiceProvider) GetServiceProviderServices(spID string) (services []Service, err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, servicePtr := range serviceProvider.services {
		if servicePtr.ServiceProvider == spID {
			services = append(services, *servicePtr)
		}
	}

	return services, nil
}

func (serviceProvider *testServiceProvider) GetServiceByUnitName(unitName string) (service Service, err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, servicePtr := range serviceProvider.services {
		if service.UnitName == unitName {
			return *servicePtr, nil
		}
	}

	return service, fmt.Errorf("service with unit %s does not exist", unitName)
}

func (serviceProvider *testServiceProvider) SetServiceStatus(serviceID string, status ServiceStatus) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	if _, ok := serviceProvider.services[serviceID]; !ok {
		return fmt.Errorf("service %s does not exist", serviceID)
	}

	serviceProvider.services[serviceID].Status = status

	return nil
}

func (serviceProvider *testServiceProvider) SetServiceState(serviceID string, state ServiceState) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	if _, ok := serviceProvider.services[serviceID]; !ok {
		return fmt.Errorf("service %s does not exist", serviceID)
	}

	serviceProvider.services[serviceID].State = state

	return nil
}

func (serviceProvider *testServiceProvider) SetServiceStartTime(serviceID string, time time.Time) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	if _, ok := serviceProvider.services[serviceID]; !ok {
		return fmt.Errorf("service %s does not exist", serviceID)
	}

	serviceProvider.services[serviceID].StartAt = time

	return nil
}

func (serviceProvider *testServiceProvider) AddServiceToUsers(users []string, serviceID string) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, usersServicePtr := range serviceProvider.usersServices {
		if reflect.DeepEqual(usersServicePtr.Users, users) && usersServicePtr.ServiceID == serviceID {
			return fmt.Errorf("service %s already in users", serviceID)
		}
	}

	serviceProvider.usersServices = append(serviceProvider.usersServices, &UsersService{Users: users, ServiceID: serviceID})

	return nil
}

func (serviceProvider *testServiceProvider) RemoveServiceFromUsers(users []string, serviceID string) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	i := 0

	for _, usersServicePtr := range serviceProvider.usersServices {
		if !reflect.DeepEqual(usersServicePtr.Users, users) || usersServicePtr.ServiceID != serviceID {
			serviceProvider.usersServices[i] = usersServicePtr
			i++
		}
	}

	serviceProvider.usersServices = serviceProvider.usersServices[:i]

	return nil
}

func (serviceProvider *testServiceProvider) GetUsersServices(users []string) (services []Service, err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, usersService := range serviceProvider.usersServices {
		if reflect.DeepEqual(usersService.Users, users) {
			service, ok := serviceProvider.services[usersService.ServiceID]
			if !ok {
				return nil, fmt.Errorf("service %s does not exist", usersService.ServiceID)
			}

			services = append(services, *service)
		}
	}

	return services, nil
}

func (serviceProvider *testServiceProvider) RemoveServiceFromAllUsers(serviceID string) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	i := 0

	for _, usersService := range serviceProvider.usersServices {
		if usersService.ServiceID != serviceID {
			serviceProvider.usersServices[i] = usersService
			i++
		}
	}

	serviceProvider.usersServices = serviceProvider.usersServices[:i]

	return nil
}

func (serviceProvider *testServiceProvider) GetUsersService(users []string, serviceID string) (userService UsersService, err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, usersServicePtr := range serviceProvider.usersServices {
		if reflect.DeepEqual(usersServicePtr.Users, users) && usersServicePtr.ServiceID == serviceID {
			return *usersServicePtr, nil
		}
	}

	return userService, fmt.Errorf("service %s does not exist in users", serviceID)
}

func (serviceProvider *testServiceProvider) GetUsersServicesByServiceID(serviceID string) (userServices []UsersService, err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, usersServicePtr := range serviceProvider.usersServices {
		userServices = append(userServices, *usersServicePtr)
	}

	return userServices, nil
}

func (serviceProvider *testServiceProvider) SetUsersStorageFolder(users []string, serviceID string, storageFolder string) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, usersServicePtr := range serviceProvider.usersServices {
		if reflect.DeepEqual(usersServicePtr.Users, users) && usersServicePtr.ServiceID == serviceID {
			usersServicePtr.StorageFolder = storageFolder

			return nil
		}
	}

	return fmt.Errorf("service %s does not exist in users", serviceID)
}

func (serviceProvider *testServiceProvider) SetUsersStateChecksum(users []string, serviceID string, checksum []byte) (err error) {
	serviceProvider.Lock()
	defer serviceProvider.Unlock()

	for _, usersServicePtr := range serviceProvider.usersServices {
		if reflect.DeepEqual(usersServicePtr.Users, users) && usersServicePtr.ServiceID == serviceID {
			usersServicePtr.StateChecksum = checksum

			return nil
		}
	}

	return fmt.Errorf("service %s does not exist in users", serviceID)
}

func (layerProvider *testLayerProvider) GetLayerPathByDigest(layerDigest string) (layerPath string, err error) {
	return path.Join(testDir, "layerStorage"), nil
}

func (layerProvider *testLayerProvider) DeleteUnneededLayers() (err error) {
	return nil
}

func (deviceManager *testDeviceManager) GetBoardConfigError() (err error) {
	deviceManager.Lock()
	defer deviceManager.Unlock()

	if deviceManager.isValid == false {
		return errors.New("this device isn't presented on System")
	}

	return nil
}

func (deviceManager *testDeviceManager) RequestDeviceResourceByName(
	name string) (deviceResource resourcemanager.DeviceResource, err error) {
	deviceManager.Lock()
	defer deviceManager.Unlock()

	if deviceManager.isValid == false {
		return resourcemanager.DeviceResource{}, errors.New("device resources are not valid")
	}

	return resourcemanager.DeviceResource{Name: "random", Groups: []string{"root"},
		HostDevices: []string{"/dev/random"}}, nil
}

func (deviceManager *testDeviceManager) RequestDevice(device string, serviceID string) (err error) {
	deviceManager.Lock()
	defer deviceManager.Unlock()

	if deviceManager.isValid == false {
		return errors.New("device resources are not valid")
	}

	return nil
}

func (deviceManager *testDeviceManager) ReleaseDevice(device string, serviceID string) (err error) {
	deviceManager.Lock()
	defer deviceManager.Unlock()

	return nil
}
func (deviceManager *testDeviceManager) RequestBoardResourceByName(name string) (boardResource resourcemanager.BoardResource,
	err error) {
	switch name {
	case "dbus":
		boardResource := resourcemanager.BoardResource{
			Env: []string{"BUS_SYSTEM_BUS_ADDRESS=unix:path=/var/run/dbus/system_bus_socket"},
			Mounts: []resourcemanager.FileSystemMount{resourcemanager.FileSystemMount{
				Destination: "/destination",
				Source:      "/source",
			}},
		}
		return boardResource, nil

	case "wifi":
		boardResource := resourcemanager.BoardResource{
			Env: []string{"BUS_SYSTEM_BUS_ADDRESS=unix:path=/var/run/dbus/system_bus_socket"},
			Mounts: []resourcemanager.FileSystemMount{resourcemanager.FileSystemMount{
				Destination: "/destination",
				Source:      "/source"},
			},
			Groups: []string{"bin"},
		}
		return boardResource, nil

	default:
		return boardResource, errors.New("Resource doesn't exist")
	}
}

/*******************************************************************************
 * Private
 ******************************************************************************/

func setup() (err error) {
	if tmpDir, err = ioutil.TempDir("", "aos_"); err != nil {
		return err
	}

	testDir = path.Join(tmpDir, "testDir")

	if err := createTestPartition(testDir, "ext4", 16); err != nil {
		return err
	}

	if err = os.MkdirAll(path.Join(testDir, "storage"), 0755); err != nil {
		return err
	}

	if networkProvider, err = networkmanager.New(&config.Config{WorkingDir: testDir}); err != nil {
		return err
	}

	return nil
}

func cleanup() (err error) {
	launcher, err := newTestLauncher(new(pythonImage), nil, nil, nil)
	if err != nil {
		log.Errorf("Can't create test launcher: %s", err)
	}

	if launcher != nil {
		if err := launcher.RemoveAllServices(); err != nil {
			log.Errorf("Can't remove all services: %s", err)
		}

		launcher.Close()
	}

	if err := networkProvider.DeleteAllNetworks(); err != nil {
		log.Errorf("Can't delete all networks: %s", err)
	}

	if err := networkProvider.Close(); err != nil {
		log.Errorf("Can't close network provider: %s", err)
	}

	if err := deleteTestPartition(testDir); err != nil {
		log.Errorf("Can't remove storage partition: %s", err)
	}

	if err := os.RemoveAll(tmpDir); err != nil {
		log.Errorf("Can't remove tmp folder: %s", err)
	}

	return nil
}

func createTestPartition(mountPoint string, fsType string, size uint64) (err error) {
	defer func() {
		if err != nil {
			deleteTestPartition(mountPoint)
		}
	}()

	var output []byte
	imagePath := path.Join(tmpDir, "storage.img")

	if output, err = exec.Command("dd", "if=/dev/zero", "of="+imagePath, "bs=1M",
		"count="+strconv.FormatUint(size, 10)).CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%s)", err, (string(output)))
	}

	if output, err = exec.Command("mkfs."+fsType, "-b", "4096", imagePath).CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%s)", err, (string(output)))
	}

	if err = os.MkdirAll(mountPoint, 0755); err != nil {
		return err
	}

	if output, err = exec.Command("mount", "-o,usrjquota=aquota.user,jqfmt=vfsv0", imagePath, mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%s)", err, (string(output)))
	}

	if output, err = exec.Command("quotacheck", "-favum").CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%s)", err, (string(output)))
	}

	if output, err = exec.Command("quotaon", "-avu").CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%s)", err, (string(output)))
	}

	return nil
}

func deleteTestPartition(mountPoint string) (err error) {
	var output []byte

	if output, err = exec.Command("umount", mountPoint).CombinedOutput(); err != nil {
		return fmt.Errorf("%s (%s)", err, (string(output)))
	}

	return nil
}

func generatePythonContent(imagePath string) (err error) {
	serviceContent := `#!/usr/bin/python

import time
import socket
import sys
import netifaces

i = 0
serviceName = sys.argv[1]
serviceVersion = sys.argv[2]

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM) 
message = serviceName + ", version: " + serviceVersion

sock.sendto(str.encode(message), (netifaces.gateways()['default'][netifaces.AF_INET][0], 10001))
sock.close()

print(">>>> Start", serviceName, "version", serviceVersion)
while True:
	print(">>>> aos", serviceName, "version", serviceVersion, "count", i)
	i = i + 1
	time.sleep(5)`

	if err := ioutil.WriteFile(path.Join(imagePath, "rootfs", "home", "service.py"), []byte(serviceContent), 0644); err != nil {
		return err
	}

	return nil
}

func generateFtpContent(imagePath string, ftpDir string) (err error) {
	serviceContent := `#!/usr/bin/python

from pyftpdlib.authorizers import DummyAuthorizer
from pyftpdlib.handlers import FTPHandler
from pyftpdlib.servers import FTPServer
from pathlib import Path
import os

Path("%s").mkdir(parents=True, exist_ok=True)

authorizer = DummyAuthorizer()
authorizer.add_anonymous("%s", perm="elradfmw")

handler = FTPHandler
handler.authorizer = authorizer

server = FTPServer(("", 21), handler)
server.serve_forever()`

	if err := ioutil.WriteFile(
		path.Join(imagePath, "rootfs", "home", "service.py"),
		[]byte(fmt.Sprintf(serviceContent, ftpDir, ftpDir)), 0644); err != nil {
		return err
	}

	return nil
}

func generateConfig(imagePath string) (err error) {
	// remove json
	if err := os.Remove(path.Join(imagePath, "config.json")); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}

	// generate config spec
	out, err := exec.Command("runc", "spec", "-b", imagePath).CombinedOutput()
	if err != nil {
		return errors.New(string(out))
	}

	return nil
}

func generateAndSaveDigest(folder string, data []byte) (retDigest digest.Digest, err error) {
	fullPath := path.Join(folder, "sha256")
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		return retDigest, err
	}

	h := sha256.New()
	h.Write(data)
	retDigest = digest.NewDigest("sha256", h)

	file, err := os.Create(path.Join(fullPath, retDigest.Hex()))
	if err != nil {
		return retDigest, err
	}
	defer file.Close()

	_, err = file.Write(data)
	if err != nil {
		return retDigest, err
	}

	return retDigest, nil
}

func generateFakeImage(folderPath string) (err error) {
	blobsDir := path.Join(folderPath, "blobs")
	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		return err
	}

	configString := "This is fake image config"
	configDigest, err := generateAndSaveDigest(blobsDir, []byte(configString))
	if err != nil {
		return err
	}

	aosConfigString := "AOS Serice config fake file"
	aosConfigDigest, err := generateAndSaveDigest(blobsDir, []byte(aosConfigString))
	if err != nil {
		return err
	}

	layerString := "Fake Serice rootfs"
	layerDigest, err := generateAndSaveDigest(blobsDir, []byte(layerString))
	if err != nil {
		return err
	}

	if err := genarateImageManfest(folderPath, &configDigest, &aosConfigDigest, &layerDigest, nil); err != nil {
		return err
	}

	return nil
}

func genarateImageManfest(folderPath string, imgConfig, aosSrvConfig, rootfsLayer *digest.Digest,
	srvLayers []digest.Digest) (err error) {
	var manifest serviceManifest
	manifest.SchemaVersion = 2

	manifest.Config = imagespec.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json",
		Digest: *imgConfig,
	}

	if aosSrvConfig != nil {
		manifest.AosService = &imagespec.Descriptor{MediaType: "application/vnd.aos.service.config.v1+json",
			Digest: *aosSrvConfig,
		}
	}

	layerDescriptor := imagespec.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest: *rootfsLayer,
	}

	manifest.Layers = append(manifest.Layers, layerDescriptor)

	for _, layerDigest := range srvLayers {
		layerDescriptor := imagespec.Descriptor{MediaType: "application/vnd.aos.image.layer.v1.tar",
			Digest: layerDigest,
		}

		manifest.Layers = append(manifest.Layers, layerDescriptor)
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	jsonFile, err := os.Create(path.Join(folderPath, "manifest.json"))
	if err != nil {
		return err
	}

	if _, err := jsonFile.Write(data); err != nil {
		return err
	}

	return nil
}

func (launcher *Launcher) connectToFtp(serviceID string) (ftpConnection *ftp.ServerConn, err error) {
	service, err := launcher.serviceProvider.GetService(serviceID)
	if err != nil {
		return nil, err
	}

	ip, err := networkProvider.GetServiceIP(service.ID, service.ServiceProvider)
	if err != nil {
		return nil, err
	}

	ftpConnection, err = ftp.DialTimeout(ip+":21", 5*time.Second)
	if err != nil {
		return nil, err
	}

	if err = ftpConnection.Login("anonymous", "anonymous"); err != nil {
		ftpConnection.Quit()
		return nil, err
	}

	return ftpConnection, nil
}

func generateImageConfig() (config *imagespec.Image, err error) {
	configStr := `{
		"created": "2015-10-31T22:22:56.015925234Z",
		"author": "Alyssa P. Hacker <alyspdev@example.com>",
		"architecture": "amd64",
		"os": "Linux",
		"config": {
			"ExposedPorts": {
				"8080/tcp": {}
			},
			"Env": [
				"PATH=/usr/local/sbin",
				"FOO=oci_is_a",
				"BAR=well_written_spec",
				"MY_VAR",
				"TERM"
			],
			"Entrypoint": [
				"/bin/my-app-binary"
			],
			"Cmd": [
				"--foreground",
				"--config",
				"/etc/my-app.d/default.cfg"
			],
			"Volumes": {
				"/var/job-result-data": {},
				"/var/log/my-app-logs": {}
			},
			"WorkingDir": "/home/alice",
			"Labels": {
				"com.example.project.git.url": "https://example.com/project.git",
				"com.example.project.git.commit": "45a939b2999782a3f005621a8d0f29aa387e1d6b"
			}
		},
		"rootfs": {
		  "diff_ids": [
			"sha256:c6f988f4874bb0add23a778f753c65efe992244e148a1d2ec2a8b664fb66bbd1",
			"sha256:5f70bf18a086007016e948b04aed3b82103a36bea41755b6cddfaf10ace3c6ef"
		  ],
		  "type": "layers"
		},
		"history": [
		  {
			"created": "2015-10-31T22:22:54.690851953Z",
			"created_by": "/bin/sh -c #(nop) ADD file:a3bc1e842b69636f9df5256c49c5374fb4eef1e281fe3f282c65fb853ee171c5 in /"
		  },
		  {
			"created": "2015-10-31T22:22:55.613815829Z",
			"created_by": "/bin/sh -c #(nop) CMD [\"sh\"]",
			"empty_layer": true
		  }
		]
	}
	`
	var imageConfig imagespec.Image
	if err = json.Unmarshal([]byte(configStr), &imageConfig); err != nil {
		return nil, err
	}

	return &imageConfig, nil
}

func saveImageConfig(folderPath string, config *imagespec.Image) (filePath string, err error) {
	filePath = path.Join(folderPath, "imageConfig.json")

	if err := os.Remove(filePath); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
	}

	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}

	jsonFile, err := os.Create(filePath)
	if err != nil {
		return "", err
	}

	if _, err := jsonFile.Write(data); err != nil {
		return "", err
	}

	return filePath, err
}

func generateFsLayer(imgFolder, rootfs string) (digest digest.Digest, err error) {
	blobsDir := path.Join(imgFolder, "blobs")
	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		return digest, err
	}

	tarFile := path.Join(blobsDir, "_temp.tar.gz")

	if output, err := exec.Command("tar", "-C", rootfs, "-czf", tarFile, "./").CombinedOutput(); err != nil {
		return digest, fmt.Errorf("error: %s, code: %s", string(output), err)
	}
	defer os.Remove(tarFile)

	file, err := os.Open(tarFile)
	if err != nil {
		return digest, err
	}
	defer file.Close()

	byteValue, err := ioutil.ReadAll(file)
	if err != nil {
		return digest, err
	}

	digest, err = generateAndSaveDigest(blobsDir, byteValue)
	if err != nil {
		return digest, err
	}

	os.RemoveAll(rootfs)

	return digest, nil
}

func generateAosSrvConfig() (cfg aosServiceConfig) {
	cfg.Author = "Test Author"
	cfg.Created = time.Now()

	var nofileLimit uint64 = 1024

	cfg.Quotas.NoFileLimit = &nofileLimit

	return cfg
}
