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

package umclient_test

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"gitpct.epam.com/nunc-ota/aos_common/umprotocol"
	"gitpct.epam.com/nunc-ota/aos_common/wsserver"

	amqp "aos_servicemanager/amqphandler"
	"aos_servicemanager/config"
	"aos_servicemanager/database"
	"aos_servicemanager/fcrypt"
	"aos_servicemanager/image"
	"aos_servicemanager/umclient"
)

/*******************************************************************************
 * Consts
 ******************************************************************************/

const serverURL = "wss://localhost:8089"

const (
	imageFile = "This is image file"
)

/*******************************************************************************
 * Types
 ******************************************************************************/

type operationStatus struct {
	status       string
	err          string
	imageVersion uint64
}

// Test sender
type testSender struct {
	upgradeStatusChannel chan operationStatus
	revertStatusChannel  chan operationStatus
}

type messageProcessor struct {
}

/*******************************************************************************
 * Vars
 ******************************************************************************/

var (
	db     *database.Database
	sender *testSender
	server *wsserver.Server
	client *umclient.Client
)

var (
	imageVersion     uint64
	operationVersion uint64
)

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
	if err := os.MkdirAll("tmp/fileServer", 0755); err != nil {
		log.Fatalf("Can't crate file server dir: %s", err)
	}

	go func() {
		log.Fatal(http.ListenAndServe(":8080", http.FileServer(http.Dir("tmp/fileServer"))))
	}()

	url, err := url.Parse(serverURL)
	if err != nil {
		log.Fatalf("Can't parse url: %s", err)
	}

	server, err = wsserver.New("TestServer", url.Host,
		"../vendor/gitpct.epam.com/nunc-ota/aos_common/wsserver/data/crt.pem",
		"../vendor/gitpct.epam.com/nunc-ota/aos_common/wsserver/data/key.pem", newMessageProcessor)
	if err != nil {
		log.Fatalf("Can't create ws server: %s", err)
	}
	defer server.Close()

	sender = newTestSender()

	db, err = database.New("tmp/servicemanager.db")
	if err != nil {
		log.Fatalf("Can't create db: %s", err)
	}

	rootCert := []byte(`
-----BEGIN CERTIFICATE-----
MIIEAjCCAuqgAwIBAgIJAPwk2NFfSDPjMA0GCSqGSIb3DQEBCwUAMIGNMRcwFQYD
VQQDDA5GdXNpb24gUm9vdCBDQTEpMCcGCSqGSIb3DQEJARYadm9sb2R5bXlyX2Jh
YmNodWtAZXBhbS5jb20xDTALBgNVBAoMBEVQQU0xHDAaBgNVBAsME05vdnVzIE9y
ZG8gU2VjbG9ydW0xDTALBgNVBAcMBEt5aXYxCzAJBgNVBAYTAlVBMB4XDTE4MDQx
MDExMzMwMFoXDTI2MDYyNzExMzMwMFowgY0xFzAVBgNVBAMMDkZ1c2lvbiBSb290
IENBMSkwJwYJKoZIhvcNAQkBFhp2b2xvZHlteXJfYmFiY2h1a0BlcGFtLmNvbTEN
MAsGA1UECgwERVBBTTEcMBoGA1UECwwTTm92dXMgT3JkbyBTZWNsb3J1bTENMAsG
A1UEBwwES3lpdjELMAkGA1UEBhMCVUEwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAw
ggEKAoIBAQC+K2ow2HO7+SUVfOq5tTtmHj4LQijHJ803mLk9pkPef+Glmeyp9HXe
jDlQC04MeovMBeNTaq0wibf7qas9niXbeXRVzheZIFziMXqRuwLqc0KXdDxIDPTb
TW3K0HE6M/eAtTfn9+Z/LnkWt4zMXasc02hvufsmIVEuNbc1VhrsJJg5uk88ldPM
LSF7nff9eYZTHYgCyBkt9aL+fwoXO6eSDSAhjopX3lhdidkM+ni7EOhlN7STmgDM
WKh9nMjXD5f28PGhtW/dZvn4SzasRE5MeaExIlBmhkWEUgVCyP7LvuQGRUPK+NYz
FE2CLRuirLCWy1HIt9lLziPjlZ4361mNAgMBAAGjYzBhMB0GA1UdDgQWBBR0Shhz
OuM95BhD0mWxC1j+KrE6UjAMBgNVHRMEBTADAQH/MAsGA1UdDwQEAwIBBjAlBgNV
HREEHjAcgRp2b2xvZHlteXJfYmFiY2h1a0BlcGFtLmNvbTANBgkqhkiG9w0BAQsF
AAOCAQEAl8bv1HTYe3l4Y+g0TVZR7bYL5BNsnGgqy0qS5fu991khXWf+Zwa2MLVn
YakMnLkjvdHqUpWMJ/S82o2zWGmmuxca56ehjxCiP/nkm4M74yXz2R8cu52WxYnF
yMvgawzQ6c1yhvZiv/gEE7KdbYRVKLHPgBzfyup21i5ngSlTcMRRS7oOBmoye4qc
6adq6HtY6X/OnZ9I5xoRN1GcvaLUgUE6igTiVa1pF8kedWhHY7wzTXBxzSvIZkCU
VHEOzvaGk9miP6nBrDfNv7mIkgEKARrjjSpmJasIEU+mNtzeOIEiMtW1EMRc457o
0PdFI3jseyLVPVhEzUkuC7mwjb7CeQ==
-----END CERTIFICATE-----
`)

	if err := ioutil.WriteFile("tmp/rootCert.pem", rootCert, 0644); err != nil {
		log.Fatalf("Can't create root cert: %s", err)
	}

	crypt, err := fcrypt.CreateContext(config.Crypt{CACert: "tmp/rootCert.pem"})
	if err != nil {
		log.Fatalf("Can't create crypto context: %s", err)
	}

	client, err = umclient.New(&config.Config{UpgradeDir: "tmp/upgrade"}, crypt, sender, db)
	if err != nil {
		log.Fatalf("Error creating UM client: %s", err)
	}

	go func() {
		<-client.ErrorChannel
	}()

	ret := m.Run()

	if err = client.Close(); err != nil {
		log.Fatalf("Error closing UM: %s", err)
	}

	if err := os.RemoveAll("tmp"); err != nil {
		log.Fatalf("Error removing tmp dir: %s", err)
	}

	os.Exit(ret)
}

/*******************************************************************************
 * Tests
 ******************************************************************************/

func TestGetSystemVersion(t *testing.T) {
	imageVersion = 4

	if err := client.Connect(serverURL); err != nil {
		log.Fatalf("Error connecting to UM server: %s", err)
	}
	defer client.Disconnect()

	version, err := client.GetSystemVersion()
	if err != nil {
		t.Fatalf("Can't get system version: %s", err)
	}

	if version != imageVersion {
		t.Errorf("Wrong image version: %d", version)
	}

	client.Disconnect()
}

func TestSystemUpgrade(t *testing.T) {
	imageVersion = 3

	if err := client.Connect(serverURL); err != nil {
		log.Fatalf("Error connecting to UM server: %s", err)
	}
	defer client.Disconnect()

	metadata := amqp.UpgradeMetadata{
		Data: []amqp.UpgradeFileInfo{createUpgradeFile("target1", "imagefile", []byte(imageFile))}}

	client.SystemUpgrade(4, metadata)

	// wait for upgrade status
	select {
	case status := <-sender.upgradeStatusChannel:
		if status.err != "" {
			t.Errorf("Upgrade failed: %s", status.err)
		}

	case <-time.After(1 * time.Second):
		t.Error("Waiting for upgrade status timeout")
	}
}

func TestRevertUpgrade(t *testing.T) {
	imageVersion = 4

	if err := client.Connect(serverURL); err != nil {
		log.Fatalf("Error connecting to UM server: %s", err)
	}
	defer client.Disconnect()

	client.SystemRevert(3)

	// wait for revert status
	select {
	case <-sender.revertStatusChannel:

	case <-time.After(1 * time.Second):
		t.Error("Waiting for revert status timeout")
	}
}

/*******************************************************************************
 * Private
 ******************************************************************************/

func newTestSender() (sender *testSender) {
	sender = &testSender{}

	sender.revertStatusChannel = make(chan operationStatus, 1)
	sender.upgradeStatusChannel = make(chan operationStatus, 1)

	return sender
}

func (sender *testSender) SendSystemRevertStatus(revertStatus, revertError string, imageVersion uint64) (err error) {
	sender.revertStatusChannel <- operationStatus{revertStatus, revertError, imageVersion}

	return nil
}

func (sender *testSender) SendSystemUpgradeStatus(upgradeStatus, upgradeError string, imageVersion uint64) (err error) {
	sender.upgradeStatusChannel <- operationStatus{upgradeStatus, upgradeError, imageVersion}

	return nil
}

func newMessageProcessor(sendMessage wsserver.SendMessage) (processor wsserver.MessageProcessor, err error) {
	return &messageProcessor{}, nil
}

func (processor *messageProcessor) ProcessMessage(messageType int, messageIn []byte) (messageOut []byte, err error) {
	var message umprotocol.Message
	var response interface{}

	log.Debug(string(messageIn))

	if err = json.Unmarshal(messageIn, &message); err != nil {
		return nil, err
	}

	switch message.Header.MessageType {
	case umprotocol.StatusRequestType:
		response = umprotocol.StatusRsp{
			Operation:        umprotocol.UpgradeOperation,
			Status:           umprotocol.SuccessStatus,
			RequestedVersion: operationVersion,
			CurrentVersion:   imageVersion}

	case umprotocol.UpgradeRequestType:
		var upgradeReq umprotocol.UpgradeReq

		if err = json.Unmarshal(message.Data, &upgradeReq); err != nil {
			return nil, err
		}

		operationVersion = upgradeReq.ImageVersion
		imageVersion = upgradeReq.ImageVersion

		status := umprotocol.SuccessStatus
		errStr := ""

		fileName := path.Join("tmp/upgrade/", upgradeReq.ImageInfo.Path)

		if err = image.CheckFileInfo(fileName, image.FileInfo{
			Sha256: upgradeReq.ImageInfo.Sha256,
			Sha512: upgradeReq.ImageInfo.Sha512,
			Size:   upgradeReq.ImageInfo.Size}); err != nil {
			status = umprotocol.FailedStatus
			errStr = err.Error()
			break
		}

		data, err := ioutil.ReadFile(fileName)
		if err != nil {
			status = umprotocol.FailedStatus
			errStr = err.Error()
			break
		}

		if imageFile != string(data) {
			status = umprotocol.FailedStatus
			errStr = "image file content mismatch"
			break
		}

		response = umprotocol.StatusRsp{
			Operation:        umprotocol.UpgradeOperation,
			Status:           status,
			Error:            errStr,
			RequestedVersion: operationVersion,
			CurrentVersion:   imageVersion}

	case umprotocol.RevertRequestType:
		var revertReq umprotocol.UpgradeReq

		if err = json.Unmarshal(message.Data, &revertReq); err != nil {
			return nil, err
		}

		operationVersion = revertReq.ImageVersion
		imageVersion = revertReq.ImageVersion

		response = umprotocol.StatusRsp{
			Operation:        umprotocol.RevertOperation,
			Status:           umprotocol.SuccessStatus,
			RequestedVersion: operationVersion,
			CurrentVersion:   imageVersion}

	default:
		return nil, fmt.Errorf("unsupported message type: %s", message.Header.MessageType)
	}

	message.Header.MessageType = umprotocol.StatusResponseType

	if message.Data, err = json.Marshal(response); err != nil {
		return nil, err
	}

	if messageOut, err = json.Marshal(message); err != nil {
		return nil, err
	}

	return messageOut, nil
}

func createUpgradeFile(target, fileName string, data []byte) (fileInfo amqp.UpgradeFileInfo) {
	filePath := path.Join("tmp/fileServer", fileName)

	if err := ioutil.WriteFile(filePath, data, 0644); err != nil {
		log.Fatalf("Can't write image file: %s", err)
	}

	imageFileInfo, err := image.CreateFileInfo(filePath)
	if err != nil {
		log.Fatalf("Can't create file info: %s", err)
	}

	fileInfo.Target = target
	fileInfo.URLs = []string{"http://localhost:8080/" + fileName}
	fileInfo.Sha256 = imageFileInfo.Sha256
	fileInfo.Sha512 = imageFileInfo.Sha512
	fileInfo.Size = imageFileInfo.Size

	return fileInfo
}
