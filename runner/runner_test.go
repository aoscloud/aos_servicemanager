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

package runner_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"testing"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/api/cloudprotocol"
	"github.com/coreos/go-systemd/v22/dbus"
	log "github.com/sirupsen/logrus"

	"github.com/aoscloud/aos_servicemanager/runner"
)

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

const serviceTemplate = `[Unit]
Description=AOS Service
StartLimitIntervalSec=2
StartLimitBurst=1

[Service]
Type=simple
Restart=always
ExecStart=/bin/sh %s/%%i/service.sh %%i

[Install]
WantedBy=multi-user.target
`

const serviceContent = `#!/bin/bash
echo "Hello from: $1"
sleep 10
exit 1
`
const serviceFileName = "service.sh"

const waitStatusTimeout = 20 * time.Second

/***********************************************************************************************************************
 * Vars
 **********************************************************************************************************************/

var tmpDir string

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

var systemd *dbus.Conn

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
***********************************************************************************************************************/

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		log.Fatalf("Error setting up: %v", err)
	}

	ret := m.Run()

	if err := cleanup(); err != nil {
		log.Errorf("Can't cleaning up: %v", err)
	}

	os.Exit(ret)
}

/***********************************************************************************************************************
* Tests
***********************************************************************************************************************/

func TestStartStopService(t *testing.T) {
	runnerInstance, err := runner.New()
	if err != nil {
		t.Fatalf("Can't create runner: %s", err)
	}
	defer runnerInstance.Close()

	serviceDir := path.Join(tmpDir, "id1")

	if err = os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatalf("Can't create service dir: %s", err)
	}

	if err = ioutil.WriteFile(path.Join(serviceDir, serviceFileName), []byte(serviceContent), 0o600); err != nil {
		t.Fatalf("Can't create service binary: %s", err)
	}

	status := runnerInstance.StartInstance("id1", serviceDir, runner.RunParameters{StartInterval: 2 * time.Second})
	if status.Err != nil {
		t.Errorf("Can't start service: %s", status.Err)
	}

	if status.State != cloudprotocol.InstanceStateActive {
		t.Error("Service is not active")
	}

	// test no service binary
	status = runnerInstance.StartInstance("someID", serviceDir, runner.RunParameters{StartInterval: 2 * time.Second})
	if status.Err == nil {
		t.Error("Should be error can't start service instance")
	}

	if status.State != cloudprotocol.InstanceStateFailed {
		t.Error("State should be failed")
	}

	// wait for service failed

	failStatus, err := waitForStatus(runnerInstance.InstanceStatusChannel())
	if err != nil {
		t.Fatalf("Wait for status error: %v", err)
	}

	if len(failStatus) != 1 {
		t.Error("Count of updated statuses should be 1")
	}

	if failStatus[0].InstanceID != "id1" {
		t.Error("Incorrect instance id in status")
	}

	if failStatus[0].State != cloudprotocol.InstanceStateFailed {
		t.Errorf("Incorrect service state: %s", failStatus[0].State)
	}

	// wait for service active

	activeStatus, err := waitForStatus(runnerInstance.InstanceStatusChannel())
	if err != nil {
		t.Fatalf("Wait for status error: %v", err)
	}

	if activeStatus[0].InstanceID != "id1" {
		t.Error("Incorrect instance id in status")
	}

	if activeStatus[0].State != cloudprotocol.InstanceStateActive {
		t.Errorf("Incorrect service state: %s", activeStatus[0].State)
	}

	// stop instance
	if err := runnerInstance.StopInstance("id1"); err != nil {
		t.Errorf("Can't stop service: %s", err)
	}

	// test service not loaded
	if err := runnerInstance.StopInstance("someID"); err != nil {
		t.Errorf("Can't stop service: %s", err)
	}
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func setup() (err error) {
	tmpDir, err = ioutil.TempDir("", "aos_")
	if err != nil {
		return aoserrors.Wrap(err)
	}

	serviceFile := path.Join(tmpDir, "aos-service@.service")

	if err = ioutil.WriteFile(serviceFile, []byte(fmt.Sprintf(serviceTemplate, tmpDir)), 0o600); err != nil {
		return aoserrors.Wrap(err)
	}

	systemd, err = dbus.NewSystemConnectionContext(context.Background())
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err = systemd.LinkUnitFilesContext(context.Background(), []string{serviceFile}, true, true); err != nil {
		return aoserrors.Wrap(err)
	}

	if err = systemd.ReloadContext(context.Background()); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func cleanup() (err error) {
	if _, disableErr := systemd.DisableUnitFilesContext(
		context.Background(), []string{"aos-service@.service"}, true); disableErr != nil && err == nil {
		err = aoserrors.Wrap(disableErr)
	}

	systemd.Close()

	if removeErr := os.RemoveAll(tmpDir); removeErr != nil && err == nil {
		err = aoserrors.Wrap(removeErr)
	}

	return err
}

func waitForStatus(channel <-chan []runner.InstanceStatus) ([]runner.InstanceStatus, error) {
	select {
	case status := <-channel:
		return status, nil

	case <-time.After(waitStatusTimeout):
		return nil, aoserrors.New("wait timeout")
	}
}
