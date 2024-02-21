// SPDX-License-Identifier: Apache-2.0
//
// Copyright (C) 2022 Renesas Electronics Corporation.
// Copyright (C) 2022 EPAM Systems, Inc.
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

package network_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/aostypes"
	"github.com/aoscloud/aos_common/api/cloudprotocol"
	"github.com/coreos/go-iptables/iptables"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	log "github.com/sirupsen/logrus"

	"github.com/aoscloud/aos_servicemanager/config"
	"github.com/aoscloud/aos_servicemanager/networkmanager"
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

type trafficData struct {
	lastUpdate   time.Time
	currentValue uint64
}

type testTrafficStorage struct {
	chains map[string]trafficData
}

/***********************************************************************************************************************
 * Vars
 **********************************************************************************************************************/

var tmpDir string

/***********************************************************************************************************************
 * Init
 **********************************************************************************************************************/

func init() {
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: false,
		TimestampFormat:  "2006-01-02 15:04:05.000",
		FullTimestamp:    true,
	})
	log.SetLevel(log.DebugLevel)
	log.SetOutput(os.Stdout)
}

/***********************************************************************************************************************
 * Main
 **********************************************************************************************************************/

func TestMain(m *testing.M) {
	var err error

	if tmpDir, err = ioutil.TempDir("", "aos_"); err != nil {
		log.Fatalf("Error creating tmp folder: %v", err)
	}

	firewall, err := iptables.New()
	if err != nil {
		log.Fatalf("Can't create iptables instance: %v", err)
	}

	exists, err := checkPolicyForwardAcceptAll(firewall)
	if err != nil {
		log.Fatalf("Can't check policy exists: %v", err)
	}

	if exists {
		if err := firewall.ChangePolicy("filter", "FORWARD", "DROP"); err != nil {
			log.Fatalf("Can't create iptables rule: %v", err)
		}
	}

	ret := m.Run()

	if err := os.RemoveAll(tmpDir); err != nil {
		log.Errorf("Can't remove tmp folder: %v", err)
	}

	if exists {
		if err := firewall.ChangePolicy("filter", "FORWARD", "ACCEPT"); err != nil {
			log.Errorf("Can't create iptables rule: %v", err)
		}
	}

	os.Exit(ret)
}

func TestAddNetwork(t *testing.T) {
	cases := []struct {
		countInstance int
		nameInstance  string
	}{
		{
			countInstance: 10,
			nameInstance:  "instance1",
		},
		{
			countInstance: 15,
			nameInstance:  "instance2",
		},
	}

	manager, err := networkmanager.New(&config.Config{WorkingDir: tmpDir}, nil)
	if err != nil {
		t.Fatalf("Can't create network manager: %v", err)
	}
	defer manager.Close()

	for numberNet, testCase := range cases {
		for numberIns := 1; numberIns <= testCase.countInstance; numberIns++ {
			t.Run(fmt.Sprintf("add_network%d_%s%d", numberNet, testCase.nameInstance, numberIns), func(t *testing.T) {
				if err := manager.AddInstanceToNetwork(
					fmt.Sprintf("%s%d", testCase.nameInstance, numberIns), fmt.Sprintf("network%d", numberNet),
					networkmanager.NetworkParams{}); err != nil {
					t.Errorf("Can't add instance to network: %v", err)
				}
			})
		}
	}

	for numberNet, testCase := range cases {
		for numberIns := 1; numberIns <= testCase.countInstance; numberIns++ {
			t.Run(fmt.Sprintf("remove_network%d_%s%d", numberNet, testCase.nameInstance, numberIns), func(t *testing.T) {
				if err := manager.RemoveInstanceFromNetwork(
					fmt.Sprintf("%s%d", testCase.nameInstance, numberIns),
					fmt.Sprintf("network%d", numberNet)); err != nil {
					t.Errorf("Can't add instance to network: %v", err)
				}
			})
		}
	}
}

func TestInternetAccess(t *testing.T) {
	manager, err := networkmanager.New(&config.Config{WorkingDir: tmpDir}, nil)
	if err != nil {
		t.Fatalf("Can't create network manager: %v", err)
	}
	defer manager.Close()

	containerPath := path.Join(tmpDir, "instance")

	if err = createOCIContainer(manager, containerPath,
		"instance", []string{"curl", "google.com", "--connect-timeout", "2"}, false); err != nil {
		t.Fatalf("Can't create container: %v", err)
	}

	if err = manager.AddInstanceToNetwork("instance", "network0", networkmanager.NetworkParams{
		ResolvConfFilePath: path.Join(containerPath, "etc", "resolv.conf"),
		DNSSevers:          []string{"1.1.1.1"},
	}); err != nil {
		t.Fatalf("Can't add instance to network: %v", err)
	}

	if err = runOCIContainer(containerPath, "instance"); err != nil {
		t.Errorf("Can't run container: %v", err)
	}

	if err = manager.RemoveInstanceFromNetwork("instance", "network0"); err != nil {
		t.Fatalf("Can't remove instance from network: %v", err)
	}
}

func TestPingDenied(t *testing.T) {
	manager, err := networkmanager.New(&config.Config{WorkingDir: tmpDir}, nil)
	if err != nil {
		t.Fatalf("Can't create network manager: %v", err)
	}
	defer manager.Close()

	containerPath := path.Join(tmpDir, "instancePing")

	if err = createOCIContainer(manager, containerPath,
		"instancePing", []string{"ping", "127.0.0.1", "-c3", "-w5"}, false); err != nil {
		t.Fatalf("Can't create container: %v", err)
	}

	if err = manager.AddInstanceToNetwork("instancePing", "network0", networkmanager.NetworkParams{
		ResolvConfFilePath: path.Join(containerPath, "etc", "resolv.conf"),
	}); err != nil {
		t.Fatalf("Can't add instance to network: %v", err)
	}

	if err = runOCIContainer(containerPath, "instancePing"); err == nil {
		t.Error("Should be an error, ping command denied")
	}

	if err = manager.RemoveInstanceFromNetwork("instancePing", "network0"); err != nil {
		t.Fatalf("Can't remove instance from network: %v", err)
	}
}

func TestInterServiceCommunication(t *testing.T) {
	manager, err := networkmanager.New(&config.Config{WorkingDir: tmpDir}, nil)
	if err != nil {
		t.Fatalf("Can't create network manager: %v", err)
	}
	defer manager.Close()

	httpListenerPort := "10001"
	container0Path := path.Join(tmpDir, "http_server")

	if err = createOCIContainerWitHTTPServer(
		manager, container0Path, "http_server", httpListenerPort); err != nil {
		t.Fatalf("Can't create container: %v", err)
	}

	if err = manager.AddInstanceToNetwork("http_server", "network0", networkmanager.NetworkParams{
		ResolvConfFilePath: path.Join(container0Path, "etc", "resolv.conf"),
		DNSSevers:          []string{"1.1.1.1"},
		ExposedPorts:       []string{httpListenerPort},
	}); err != nil {
		t.Fatalf("Can't add instance to network: %v", err)
	}

	defer func() {
		if err := manager.RemoveInstanceFromNetwork("http_server", "network0"); err != nil {
			t.Errorf("Can't remove instance from network: %v", err)
		}
	}()

	ipHTTPServer, err := manager.GetInstanceIP("http_server", "network0")
	if err != nil {
		t.Fatalf("Can't get ip address from instance: %v", err)
	}

	go func() {
		_ = runOCIContainer(container0Path, "http_server")
	}()

	t.Cleanup(func() {
		if err := killOCIContainer("http_server"); err != nil {
			t.Errorf("Can't kill container: %v", err)
		}
	})

	time.Sleep(1 * time.Second)

	cases := []struct {
		testName                   string
		spName                     string
		serviceName                string
		allowedConnection          []string
		expectedRunContainerStatus func(err error) bool
	}{
		{
			testName:    "Access HTTP server in the same subnet",
			spName:      "network0",
			serviceName: "instance_same_subnet",
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:          "Access HTTP server from different subnet with port access",
			spName:            "network1",
			serviceName:       "instance_different_subnet",
			allowedConnection: []string{"http_server" + "/" + httpListenerPort},
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:    "Access HTTP server from different subnet without port access",
			spName:      "network1",
			serviceName: "instance_different_subnet_not_allowed_port",
			expectedRunContainerStatus: func(err error) bool {
				return err != nil
			},
		},
	}

	for _, testCases := range cases {
		t.Run(testCases.testName, func(t *testing.T) {
			containerPath := path.Join(tmpDir, testCases.serviceName)

			if err := createOCIContainer(manager, containerPath, testCases.serviceName,
				[]string{
					"curl", strings.Join([]string{"http_server", httpListenerPort}, ":"),
					"--connect-timeout", "2",
				}, false); err != nil {
				t.Errorf("Can't create container: %v", err)
			}

			if err := manager.AddInstanceToNetwork(testCases.serviceName, testCases.spName, networkmanager.NetworkParams{
				Hosts:              []aostypes.Host{{IP: ipHTTPServer, Hostname: "http_server"}},
				HostsFilePath:      path.Join(containerPath, "etc", "hosts"),
				ResolvConfFilePath: path.Join(containerPath, "etc", "resolv.conf"),
				AllowedConnections: testCases.allowedConnection,
			}); err != nil {
				t.Errorf("Can't add instance to network: %v", err)
			}

			if err := runOCIContainer(containerPath, testCases.serviceName); !testCases.expectedRunContainerStatus(err) {
				t.Error("Unexpected result run container")
			}
		})
	}

	for _, testCases := range cases {
		if err = manager.RemoveInstanceFromNetwork(testCases.serviceName, testCases.spName); err != nil {
			t.Errorf("Can't remove instance from network: %v", err)
		}
	}
}

func TestNetworkDNS(t *testing.T) {
	manager, err := networkmanager.New(&config.Config{WorkingDir: tmpDir}, nil)
	if err != nil {
		t.Fatalf("Can't create network manager: %v", err)
	}
	defer manager.Close()

	casesInfinityRunningInstance := []struct {
		spName       string
		instanceName string
		params       networkmanager.NetworkParams
	}{
		{
			spName:       "network0",
			instanceName: "serverDNS1",
			params: networkmanager.NetworkParams{
				Hostname: "server1",
				Aliases:  []string{"DNS1"},
				InstanceIdent: cloudprotocol.InstanceIdent{
					Instance:  0,
					SubjectID: "subject1",
					ServiceID: "serverDNS1",
				},
			},
		},
		{
			spName:       "network1",
			instanceName: "server2",
			params: networkmanager.NetworkParams{
				Hostname: "serverDNS2",
				Aliases:  []string{"DNS2"},
				InstanceIdent: cloudprotocol.InstanceIdent{
					Instance:  1,
					SubjectID: "subject2",
					ServiceID: "serverDNS2",
				},
			},
		},
	}

	for _, testCase := range casesInfinityRunningInstance {
		containerPath := path.Join(tmpDir, testCase.instanceName)

		if err = createOCIContainer(
			manager, containerPath, testCase.instanceName, []string{"sleep", "infinity"}, false); err != nil {
			t.Errorf("Can't create container: %v", err)
		}

		if err = manager.AddInstanceToNetwork(testCase.instanceName, testCase.spName, testCase.params); err != nil {
			t.Errorf("Can't add instance to network: %v", err)
		}

		go func(instanceName string) {
			_ = runOCIContainer(containerPath, instanceName)
		}(testCase.instanceName)
	}

	time.Sleep(1 * time.Second)

	cases := []struct {
		testName                   string
		spName                     string
		pingHostName               string
		expectedRunContainerStatus func(err error) bool
	}{
		{
			testName:     "ping using hostname in subnet network0",
			spName:       "network0",
			pingHostName: "server1",
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:     "ping using hostname in subnet network1",
			spName:       "network1",
			pingHostName: "server2",
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:     "ping using alias in subnet network0",
			spName:       "network0",
			pingHostName: "DNS1",
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:     "ping using alias in subnet network1",
			spName:       "network1",
			pingHostName: "DNS2",
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:     "ping using instance path in subnet network0",
			spName:       "network0",
			pingHostName: "0.subject1.serverDNS1.network0",
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:     "ping using instance path in subnet network1",
			spName:       "network1",
			pingHostName: "1.subject2.serverDNS2.network1",
			expectedRunContainerStatus: func(err error) bool {
				return err == nil
			},
		},
		{
			testName:     "ping using hostname between different subnet",
			spName:       "network1",
			pingHostName: "server1",
			expectedRunContainerStatus: func(err error) bool {
				return err != nil
			},
		},
		{
			testName:     "ping using alias between different subnet",
			spName:       "network1",
			pingHostName: "DNS1",
			expectedRunContainerStatus: func(err error) bool {
				return err != nil
			},
		},
		{
			testName:     "ping using instance path between different subnet",
			spName:       "network1",
			pingHostName: "0.subject1.serverDNS1.network0",
			expectedRunContainerStatus: func(err error) bool {
				return err != nil
			},
		},
	}

	for i, testCase := range cases {
		t.Run(testCase.testName, func(t *testing.T) {
			instanceID := fmt.Sprintf("instance%d", i)
			containerPath := path.Join(tmpDir, instanceID)

			if err = createOCIContainer(
				manager, containerPath, instanceID,
				[]string{"ping", testCase.pingHostName, "-c1", "-w5"}, true); err != nil {
				t.Errorf("Can't create container: %v", err)
			}

			if err = manager.AddInstanceToNetwork(instanceID, testCase.spName, networkmanager.NetworkParams{
				ResolvConfFilePath: path.Join(containerPath, "etc", "resolv.conf"),
			}); err != nil {
				t.Errorf("Can't add instance to network: %v", err)
			}

			if err = runOCIContainer(containerPath, instanceID); !testCase.expectedRunContainerStatus(err) {
				t.Error("Unexpected result run container")
			}
		})
	}

	for i, testCase := range cases {
		if err = manager.RemoveInstanceFromNetwork(fmt.Sprintf("instance%d", i), testCase.spName); err != nil {
			t.Errorf("Can't remove instance from network: %v", err)
		}
	}

	for _, testCase := range casesInfinityRunningInstance {
		if err = manager.RemoveInstanceFromNetwork(testCase.instanceName, testCase.spName); err != nil {
			t.Errorf("Can't remove instance from network: %v", err)
		}

		if err = killOCIContainer(testCase.instanceName); err != nil {
			t.Errorf("Can't kill container: %v", err)
		}
	}
}

func TestBandwidth(t *testing.T) {
	manager, err := networkmanager.New(&config.Config{WorkingDir: tmpDir}, nil)
	if err != nil {
		t.Fatalf("Can't create network manager: %v", err)
	}
	defer manager.Close()

	container0Path := path.Join(tmpDir, "instance0")

	var (
		setULSpeed uint64 = 1000
		setDLSpeed uint64 = 4000
	)

	if err := createOCIContainer(manager, container0Path, "instance0", []string{"iperf", "-s"}, false); err != nil {
		t.Fatalf("Can't create container: %v", err)
	}

	if err := manager.AddInstanceToNetwork("instance0", "network0", networkmanager.NetworkParams{
		IngressKbit: setDLSpeed,
		EgressKbit:  setULSpeed,
	}); err != nil {
		t.Fatalf("Can't add instance to network: %v", err)
	}

	go func() {
		_ = runOCIContainer(container0Path, "instance0")
	}()

	time.Sleep(1 * time.Second)

	ip, err := manager.GetInstanceIP("instance0", "network0")
	if err != nil {
		t.Errorf("Can't get ip address from instance: %v", err)
	}

	output, err := exec.Command("iperf", "-c", ip, "-d", "-r", "-t5", "-yc").CombinedOutput()
	if err != nil {
		t.Errorf("iperf failed: %v", err)
	}

	ulSpeed := -1.0
	dlSpeed := -1.0

	for _, line := range strings.Split(string(output), "\n") {
		result := strings.Split(line, ",")
		if len(result) >= 9 {
			// 5001 - server listening TCP port
			if result[4] == "5001" {
				value, err := strconv.ParseFloat(result[8], 64)
				if err != nil {
					t.Errorf("Can't parse download speed: %v", err)
					continue
				}

				dlSpeed = value / 1000
			} else {
				value, err := strconv.ParseFloat(result[8], 64)
				if err != nil {
					t.Errorf("Can't parse upload speed: %v", err)
					continue
				}

				ulSpeed = value / 1000
			}
		}
	}

	if ulSpeed < 0.0 || dlSpeed < 0.0 {
		t.Error("Can't determine UL/DL speed")
	}

	// Max delta 5%
	delta := 1.05

	if ulSpeed > float64(setULSpeed)*delta || dlSpeed > float64(setDLSpeed)*delta {
		t.Errorf("Speed limit exceeds expected level: DL %0.2f, UL %0.2f", dlSpeed, ulSpeed)
	}

	if err = manager.RemoveInstanceFromNetwork("instance0", "network0"); err != nil {
		t.Errorf("Can't remove instance from network: %v", err)
	}

	if err = killOCIContainer("instance0"); err != nil {
		t.Errorf("Can't kill running container: %v", err)
	}
}

func TestSystemTraffic(t *testing.T) {
	manager, err := networkmanager.New(&config.Config{WorkingDir: tmpDir},
		&testTrafficStorage{chains: make(map[string]trafficData)})
	if err != nil {
		t.Fatalf("Can't create network manager: %v", err)
	}
	defer manager.Close()

	if output, err := exec.Command("curl", "google.com", "--connect-timeout", "2").CombinedOutput(); err != nil {
		t.Errorf("Can't execute curl command: msg: %s err: %v", output, err)
	}

	inputTraffic, outputTraffic, err := manager.GetSystemTraffic()
	if err != nil {
		t.Errorf("Can't get system traffic: %v", err)
	}

	if inputTraffic <= 0 || outputTraffic <= 0 {
		t.Errorf("Unexpected input and output traffic")
	}

	container1Path := path.Join(tmpDir, "instance1")

	if err := createOCIContainer(
		manager, container1Path, "instance1", []string{"ping", "1.1.1.1", "-c2", "-s100", "-w5"}, true); err != nil {
		t.Errorf("Can't create container: %v", err)
	}

	if err := manager.AddInstanceToNetwork("instance1", "network0", networkmanager.NetworkParams{
		UploadLimit:        500,
		DownloadLimit:      500,
		ResolvConfFilePath: path.Join(container1Path, "etc", "resolv.conf"),
	}); err != nil {
		t.Errorf("Can't add instance to network: %v", err)
	}

	// 100 - ping count bytes, 8 icmp header, 20 additional bytes
	packageBytes := 100 + 8 + 20

	if err := runOCIContainer(container1Path, "instance1"); err != nil {
		t.Errorf("Can't run container: %v", err)
	}

	if inputTraffic, outputTraffic, err = manager.GetInstanceTraffic("instance1"); err != nil {
		t.Errorf("Can't get system traffic: %v", err)
	}

	if inputTraffic != uint64(packageBytes*2) || outputTraffic != uint64(packageBytes*2) {
		t.Errorf("Unexpected input and output traffic")
	}

	if err := runOCIContainer(container1Path, "instance1"); err != nil {
		t.Errorf("Can't run container: %v", err)
	}

	if inputTraffic, outputTraffic, err = manager.GetInstanceTraffic("instance1"); err != nil {
		t.Errorf("Can't get system traffic: %v", err)
	}

	if inputTraffic != uint64(packageBytes*4) || outputTraffic != uint64(packageBytes*4) {
		t.Errorf("Unexpected input and output traffic")
	}

	if err := runOCIContainer(container1Path, "instance1"); err == nil {
		t.Errorf("Unexpected result run container")
	}

	if inputTraffic, outputTraffic, err = manager.GetInstanceTraffic("instance1"); err != nil {
		t.Errorf("Can't get system traffic: %v", err)
	}

	if inputTraffic != uint64(packageBytes*4) || outputTraffic != uint64(packageBytes*4) {
		t.Errorf("Unexpected input and output traffic")
	}

	if err = manager.RemoveInstanceFromNetwork("instance1", "network0"); err != nil {
		t.Errorf("Can't remove instance from network: %v", err)
	}
}

func createOCIContainer(
	networkmanager *networkmanager.NetworkManager, imagePath string, containerID string, args []string, allowPing bool,
) error {
	if err := os.RemoveAll(imagePath); err != nil {
		return aoserrors.Wrap(err)
	}

	if err := os.MkdirAll(path.Join(imagePath, "rootfs"), 0o755); err != nil {
		return aoserrors.Wrap(err)
	}

	out, err := exec.Command("runc", "spec", "-b", imagePath).CombinedOutput()
	if err != nil {
		return aoserrors.New(string(out))
	}

	if err := addHostResolvFiles(imagePath); err != nil {
		return err
	}

	specJSON, err := ioutil.ReadFile(path.Join(imagePath, "config.json"))
	if err != nil {
		return aoserrors.Wrap(err)
	}

	var spec runtimespec.Spec

	if err = json.Unmarshal(specJSON, &spec); err != nil {
		return aoserrors.Wrap(err)
	}

	spec.Process.Terminal = false

	spec.Process.Args = args

	for i, ns := range spec.Linux.Namespaces {
		if ns.Type == runtimespec.NetworkNamespace {
			spec.Linux.Namespaces[i].Path = networkmanager.GetNetnsPath(containerID)
		}
	}

	for _, mount := range []string{"/bin", "/sbin", "/lib", "/lib64", "/usr", "/etc/nsswitch.conf"} {
		spec.Mounts = append(spec.Mounts, runtimespec.Mount{
			Destination: mount,
			Type:        "bind", Source: mount, Options: []string{"bind", "ro"},
		})
	}

	for _, mount := range []string{"hosts", "resolv.conf"} {
		spec.Mounts = append(spec.Mounts, runtimespec.Mount{
			Destination: path.Join("/etc", mount),
			Type:        "bind", Source: path.Join(imagePath, "etc", mount), Options: []string{"bind", "ro"},
		})
	}

	if allowPing {
		spec.Process.Capabilities.Bounding = append(spec.Process.Capabilities.Bounding, "CAP_NET_RAW")
		spec.Process.Capabilities.Effective = append(spec.Process.Capabilities.Effective, "CAP_NET_RAW")
		spec.Process.Capabilities.Inheritable = append(spec.Process.Capabilities.Inheritable, "CAP_NET_RAW")
		spec.Process.Capabilities.Permitted = append(spec.Process.Capabilities.Permitted, "CAP_NET_RAW")
		spec.Process.Capabilities.Ambient = append(spec.Process.Capabilities.Ambient, "CAP_NET_RAW")
	}

	if specJSON, err = json.Marshal(&spec); err != nil {
		return aoserrors.Wrap(err)
	}

	if err = ioutil.WriteFile(path.Join(imagePath, "config.json"), specJSON, 0o600); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func createOCIContainerWitHTTPServer(
	networkmanager *networkmanager.NetworkManager, imagePath string, containerID string, port string,
) error {
	if err := createOCIContainer(networkmanager, imagePath,
		containerID, []string{"/usr/bin/python3", "/httpserver.py"}, false); err != nil {
		return err
	}

	httpServerContent := `#!/usr/bin/python

import threading
import time
from http.server import ThreadingHTTPServer, SimpleHTTPRequestHandler

class MyServer(threading.Thread):
	def run(self):
		self.server = ThreadingHTTPServer(('',` + port + `), SimpleHTTPRequestHandler)
		self.server.serve_forever()
	def stop(self):
		self.server.shutdown()

if __name__ == '__main__':
	s = MyServer()
	s.start()`

	if err := ioutil.WriteFile(
		path.Join(imagePath, "rootfs", "httpserver.py"), []byte(httpServerContent), 0o600); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func addHostResolvFiles(pathToContainer string) error {
	etcPath := path.Join(pathToContainer, "etc")
	if err := os.MkdirAll(etcPath, 0o755); err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err := os.Create(path.Join(etcPath, "hosts")); err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err := os.Create(path.Join(etcPath, "resolv.conf")); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func runOCIContainer(imagePath string, containerID string) error {
	output, err := exec.Command("runc", "run", "-b", imagePath, containerID).CombinedOutput()
	if err != nil {
		return aoserrors.Errorf("message: %s, err: %v", string(output), err)
	}

	return nil
}

func killOCIContainer(containerID string) error {
	output, err := exec.Command("runc", "delete", "-f", containerID).CombinedOutput()
	if err != nil {
		return aoserrors.Errorf("message: %s, err: %v", string(output), err)
	}

	return nil
}

func checkPolicyForwardAcceptAll(firewall *iptables.IPTables) (bool, error) {
	chains, err := firewall.List("filter", "FORWARD")
	if err != nil {
		return false, aoserrors.Wrap(err)
	}

	for _, chain := range chains {
		if strings.Compare(chain, "-P FORWARD ACCEPT") == 0 {
			return true, nil
		}
	}

	return false, nil
}

func (storage *testTrafficStorage) SetTrafficMonitorData(chain string, timestamp time.Time, value uint64) error {
	storage.chains[chain] = trafficData{lastUpdate: timestamp, currentValue: value}

	return nil
}

func (storage *testTrafficStorage) GetTrafficMonitorData(chain string) (timestamp time.Time, value uint64, err error) {
	data, ok := storage.chains[chain]
	if !ok {
		return timestamp, 0, networkmanager.ErrEntryNotExist
	}

	return data.lastUpdate, data.currentValue, nil
}

func (storage *testTrafficStorage) RemoveTrafficMonitorData(chain string) error {
	delete(storage.chains, chain)

	return nil
}
