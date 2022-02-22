// SPDX-License-Identifier: Apache-2.0
//
// Copyright (C) 2021 Renesas Electronics Corporation.
// Copyright (C) 2021 EPAM Systems, Inc.
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

package config_test

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"path"
	"reflect"
	"testing"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"

	"github.com/aoscloud/aos_servicemanager/config"
)

/*******************************************************************************
 * Private
 ******************************************************************************/

func createConfigFile() (err error) {
	configContent := `{
	"CACert" : "CACert",	
	"smServerUrl": "smserver",
	"workingDir" : "workingDir",
	"certStorage": "sm",
	"storageDir" : "/var/aos/storage",
	"layersDir": "/var/aos/srvlib",
	"boardConfigFile" : "/var/aos/aos_board.cfg",
	"iamServer" : "localhost:8089",
	"iamPublicServer" : "localhost:8090",
	"defaultServiceTTLDays" : 30,
	"serviceHealthCheckTimeout": "10s",
	"monitoring": {
		"sendPeriod": "00:05:00",
		"pollPeriod": "00:00:01",		
		"ram": {
			"minTimeout": "00:00:10",
			"minThreshold": 10,
			"maxThreshold": 150
		},
		"outTraffic": {
			"minTimeout": "00:00:20",
			"minThreshold": 10,
			"maxThreshold": 150
		}
	},
	"logging": {
		"maxPartSize": 1024,
		"maxPartCount": 10
	},
	"alerts": {		
		"filter": ["(test)", "(regexp)"],
		"serviceAlertPriority": 7,
		"systemAlertPriority": 5
	},
	"hostBinds": ["dir0", "dir1", "dir2"],
	"hosts": [{
			"ip": "127.0.0.1",
			"hostName" : "wwwivi"
		},
		{
			"ip": "127.0.0.1",
			"hostName" : "wwwaosum"
		}
	],
	"migration": {
		"migrationPath" : "/usr/share/aos_servicemnager/migration",
		"mergedMigrationPath" : "/var/aos/servicemanager/mergedMigration"
	}
}`

	if err := ioutil.WriteFile(path.Join("tmp", "aos_servicemanager.cfg"), []byte(configContent), 0644); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func setup() (err error) {
	if err := os.MkdirAll("tmp", 0o755); err != nil {
		return aoserrors.Wrap(err)
	}

	if err = createConfigFile(); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func cleanup() (err error) {
	if err := os.RemoveAll("tmp"); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

/*******************************************************************************
 * Main
 ******************************************************************************/

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		log.Fatalf("Error creating service images: %s", err)
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

func TestGetCrypt(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.CACert != "CACert" {
		t.Errorf("Wrong CACert value: %s", config.CACert)
	}
}

func TestSMServerURL(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.SMServerURL != "smserver" {
		t.Errorf("Wrong smServer value: %s", config.SMServerURL)
	}
}

func TestGetWorkingDir(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.WorkingDir != "workingDir" {
		t.Errorf("Wrong workingDir value: %s", config.WorkingDir)
	}
}

func TestGetStorageDirAsWorkingDir(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.StorageDir != "/var/aos/storage" {
		t.Errorf("Wrong storageDir value: %s", config.StorageDir)
	}
}

func TestGetBoardConfigFile(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.BoardConfigFile != "/var/aos/aos_board.cfg" {
		t.Errorf("Wrong storageDir value: %s", config.BoardConfigFile)
	}
}

func TestGetLayersDir(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.LayersDir != "/var/aos/srvlib" {
		t.Errorf("Wrong storageDir value: %s", config.LayersDir)
	}
}

func TestGetIAMServerURL(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.IAMServerURL != "localhost:8089" {
		t.Errorf("Wrong IAM server value: %s", config.IAMServerURL)
	}
}

func TestGetIAMPublicServerURL(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.IAMPublicServerURL != "localhost:8090" {
		t.Errorf("Wrong IAM public server value: %s", config.IAMPublicServerURL)
	}
}

func TestGetDefaultServiceTTL(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.DefaultServiceTTLDays != 30 {
		t.Errorf("Wrong default service TTL value: %d", config.DefaultServiceTTLDays)
	}
}

func TestDurationMarshal(t *testing.T) {
	d := config.Duration{Duration: 32 * time.Second}

	result, err := json.Marshal(d)
	if err != nil {
		t.Errorf("Can't marshal: %s", err)
	}

	if string(result) != `"00:00:32"` {
		t.Errorf("Wrong value: %s", result)
	}
}

func TestGetMonitoringConfig(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.Monitoring.SendPeriod.Duration != 5*time.Minute {
		t.Errorf("Wrong send period value: %s", config.Monitoring.SendPeriod)
	}

	if config.Monitoring.PollPeriod.Duration != 1*time.Second {
		t.Errorf("Wrong poll period value: %s", config.Monitoring.PollPeriod)
	}

	if config.Monitoring.RAM.MinTimeout.Duration != 10*time.Second {
		t.Errorf("Wrong value: %s", config.Monitoring.RAM.MinTimeout)
	}

	if config.Monitoring.OutTraffic.MinTimeout.Duration != 20*time.Second {
		t.Errorf("Wrong value: %s", config.Monitoring.RAM.MinTimeout)
	}
}

func TestGetLoggingConfig(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.Logging.MaxPartSize != 1024 {
		t.Errorf("Wrong max part size: %d", config.Logging.MaxPartSize)
	}

	if config.Logging.MaxPartCount != 10 {
		t.Errorf("Wrong max part count: %d", config.Logging.MaxPartCount)
	}
}

func TestGetAlertsConfig(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	filter := []string{"(test)", "(regexp)"}

	if !reflect.DeepEqual(config.Alerts.Filter, filter) {
		t.Errorf("Wrong filter value: %v", config.Alerts.Filter)
	}

	if config.Alerts.ServiceAlertPriority != 7 {
		t.Errorf("Wrong service alert priority: %d", config.Alerts.ServiceAlertPriority)
	}

	if config.Alerts.SystemAlertPriority != 5 {
		t.Errorf("Wrong system alert priority: %d", config.Alerts.SystemAlertPriority)
	}
}

func TestHostBinds(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	dirs := []string{"dir0", "dir1", "dir2"}

	if !reflect.DeepEqual(config.HostBinds, dirs) {
		t.Errorf("Wrong hostDirs value: %v", config.HostBinds)
	}
}

func TestHosts(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if len(config.Hosts) != 2 {
		t.Errorf("Wrong count of hosts entry: 2!= %d", len(config.Hosts))
	}

	if config.Hosts[0].IP != "127.0.0.1" {
		t.Errorf("Incorrect ip")
	}

	if config.Hosts[1].Hostname != "wwwaosum" {
		t.Errorf("Incorrect hostname")
	}
}

func TestDatabaseMigration(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.Migration.MigrationPath != "/usr/share/aos_servicemnager/migration" {
		t.Errorf("Wrong migrationPath /usr/share/aos_servicemanager/migration != %s", config.Migration.MigrationPath)
	}

	if config.Migration.MergedMigrationPath != "/var/aos/servicemanager/mergedMigration" {
		t.Errorf("Wrong migrationPath /var/aos/servicemanager/mergedMigration != %s", config.Migration.MergedMigrationPath)
	}
}

func TestCertStorage(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.CertStorage != "sm" {
		t.Errorf("Wrong CertStorage value: %s", config.CertStorage)
	}
}

func TestServiceHealthCheckTimeout(t *testing.T) {
	config, err := config.New("tmp/aos_servicemanager.cfg")
	if err != nil {
		t.Fatalf("Error opening config file: %s", err)
	}

	if config.ServiceHealthCheckTimeout.Duration != 10*time.Second {
		t.Errorf("Wrong ServiceHealthCheckTimeout value: %s", config.ServiceHealthCheckTimeout.String())
	}
}
