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

package servicemanager_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/image"
	"github.com/aoscloud/aos_common/spaceallocator"
	"github.com/aoscloud/aos_common/utils/fs"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"

	"github.com/aoscloud/aos_servicemanager/config"
	"github.com/aoscloud/aos_servicemanager/servicemanager"
)

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
 * Consts
 **********************************************************************************************************************/

const (
	kilobyte           = uint64(1 << 10)
	megabyte           = uint64(1 << 20)
	errorAddServiceID  = "errorAddServiceID"
	errorGetServicID   = "errorGetServicID"
	blobsFolder        = "blobs"
	defaultServiceSize = int64(2 * kilobyte)
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

type testServiceStorage struct {
	cachedServiceError     bool
	getAllError            bool
	getServiceVersionError bool
	Services               []servicemanager.ServiceInfo
}

type testLayerProvider struct {
	digests []string
}

type testAllocator struct {
	sync.Mutex

	totalSize     uint64
	allocatedSize uint64
	partLimit     uint
	remover       spaceallocator.ItemRemover
	outdatedItems []testOutdatedItem
}

type testSpace struct {
	allocator *testAllocator
	size      uint64
}

type testOutdatedItem struct {
	id   string
	size uint64
}

/***********************************************************************************************************************
 * Vars
 **********************************************************************************************************************/

var (
	tmpDir           string
	serviceAllocator = &testAllocator{}
)

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
		log.Fatalf("Error setting up: %s", err)
	}

	ret := m.Run()

	cleanup()

	os.Exit(ret)
}

/***********************************************************************************************************************
* Tests
***********************************************************************************************************************/

func TestInstallService(t *testing.T) {
	serviceStorage := &testServiceStorage{
		Services: []servicemanager.ServiceInfo{
			{ServiceID: "id1", AosVersion: 1, GID: 5000},
			{ServiceID: "id2", AosVersion: 1, GID: 5001},
		},
	}

	config := &config.Config{
		ServicesDir: filepath.Join(tmpDir, "servicemanager", "services"),
		LayersDir:   filepath.Join(tmpDir, "layers"),
		DownloadDir: filepath.Join(tmpDir, "downloads"),
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %s", err)
	}

	// install services
	serviceID := "testService0"

	serviceURL, fileInfo, _, err := prepareService("Service content", defaultServiceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't install service: %s", err)
	}

	// update service
	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 2},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't update service: %s", err)
	}

	os.RemoveAll(serviceURL)

	// test install from remote domain
	fileServerDir, err := ioutil.TempDir("", "sm_fileserver")
	if err != nil {
		t.Fatalf("Error create temporary dir: %s", err)
	}

	defer os.RemoveAll(fileServerDir)

	serviceID = "testIDRemoteService"

	if serviceURL, fileInfo, _, err = prepareService("SomeContnet", defaultServiceSize); err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	urlVal, err := url.Parse(serviceURL)
	if err != nil {
		t.Fatalf("Can't parse url: %s", err)
	}

	_ = os.Rename(urlVal.Path, filepath.Join(fileServerDir, "downloadImage"))

	server := &http.Server{
		Addr:              ":9000",
		Handler:           http.FileServer(http.Dir(fileServerDir)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("Can't serve http server: %s", err)
		}
	}()

	time.Sleep(1 * time.Second)

	defer server.Close()

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		"http://:9000/downloadImage", fileInfo); err != nil {
		t.Errorf("Can't install service from remote domain: %s", err)
	}

	os.RemoveAll(filepath.Join(fileServerDir, "downloadImage"))

	// test version missmatch
	serviceID = "testID1"

	if serviceURL, fileInfo, _, err = prepareService("SomeContnet", defaultServiceSize); err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	serviceStorage.Services = append(serviceStorage.Services, servicemanager.ServiceInfo{
		AosVersion: 1,
		ServiceID:  serviceID,
	})

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		serviceURL, fileInfo); err == nil {
		t.Error("Should be error version mismatch")
	} else if !errors.Is(err, servicemanager.ErrVersionMismatch) {
		t.Errorf("Should be error version mismatch, but have: %s", err)
	}

	os.RemoveAll(serviceURL)

	// check incorrect check sum
	serviceID = "testID2"

	if serviceURL, fileInfo, _, err = prepareService("SomeContnet", defaultServiceSize); err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	fileInfo.Sha256 = []byte{0}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		serviceURL, fileInfo); err == nil {
		t.Error("Should be error")
	}

	// untar error
	notTarFile := filepath.Join(tmpDir, "notTar")

	if err := ioutil.WriteFile(notTarFile, []byte("testContent"), 0o600); err != nil {
		t.Errorf("Can't create file: %s", err)
	}

	if fileInfo, err = image.CreateFileInfo(context.Background(), notTarFile); err != nil {
		t.Errorf("Can't create file info: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		"file://"+notTarFile, fileInfo); err == nil {
		t.Error("Should be error can't untar")
	}

	// check download error
	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		"url://notexist", fileInfo); err == nil {
		t.Error("Should be error")
	}

	// test incorrect url
	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		"\n", fileInfo); err == nil {
		t.Error("Should be error")
	}

	os.RemoveAll(serviceURL)

	// test get service error
	if serviceURL, fileInfo, _, err = prepareService("SomeContent", defaultServiceSize); err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: errorGetServicID, AosVersion: 1},
		serviceURL, fileInfo); err == nil {
		t.Error("Should be error: can't install service")
	}

	// test add service error
	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: errorAddServiceID, AosVersion: 1},
		serviceURL, fileInfo); err == nil {
		t.Error("Should be error: can't install service")
	}

	os.RemoveAll(serviceURL)
}

func TestImageParts(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir: filepath.Join(tmpDir, "servicemanager", "services"),
		LayersDir:   filepath.Join(tmpDir, "layers"),
		DownloadDir: filepath.Join(tmpDir, "downloads"),
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %s", err)
	}

	// install services
	serviceID := "testService0"

	serviceURL, fileInfo, _, err := prepareService("Service content", defaultServiceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't install service: %s", err)
	}

	services, err := sm.GetAllServicesStatus()
	if err != nil {
		t.Errorf("Can't get all services: %s", err)
	}

	if !reflect.DeepEqual(services, serviceStorage.Services) {
		t.Error("Incorrect services")
	}

	serviceInfo, err := sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Errorf("Can't get service info: %v", err)
	}

	imageParts, err := sm.GetImageParts(serviceInfo)
	if err != nil {
		t.Errorf("Can't get image parts: %s", err)
	}

	if imageParts.ImageConfigPath == "" {
		t.Error("Image config path should not be empty")
	}

	if imageParts.ServiceConfigPath == "" {
		t.Error("Service config path should not be empty")
	}

	if imageParts.ServiceFSPath == "" {
		t.Error("Service fs path should not be empty")
	}

	if len(imageParts.LayersDigest) != 1 {
		t.Error("Count of layers should be 1")
	}
}

func TestApplyService(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir: filepath.Join(tmpDir, "servicemanager", "services"),
		LayersDir:   filepath.Join(tmpDir, "layers"),
		DownloadDir: filepath.Join(tmpDir, "downloads"),
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %s", err)
	}

	// install services
	serviceID := "testServiceApplyID"

	serviceURL, fileInfo, _, err := prepareService("Service content", defaultServiceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't install service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 2},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't update service: %s", err)
	}

	serviceInfo, err := sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Errorf("Can't get service info: %s", err)
	}

	if err := sm.ApplyService(serviceInfo); err != nil {
		t.Errorf("Can't apply service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 4},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't install service: %s", err)
	}

	serviceInfo, err = sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Errorf("Can't get service info: %s", err)
	}

	serviceStorage.Services = []servicemanager.ServiceInfo{}

	if err := sm.ApplyService(serviceInfo); err == nil {
		t.Error("Should be error: service not exist")
	}
}

func TestRevertService(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir: filepath.Join(tmpDir, "servicemanager", "services"),
		LayersDir:   filepath.Join(tmpDir, "layers"),
		DownloadDir: filepath.Join(tmpDir, "downloads"),
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %s", err)
	}

	serviceID := "testRevertID"

	serviceURL, fileInfo, _, err := prepareService("Service content", defaultServiceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't install service: %s", err)
	}

	serviceInfo, err := sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Errorf("Can't get service info: %s", err)
	}

	if err = sm.RevertService(serviceInfo); err != nil {
		t.Errorf("Can't revert service: %s", err)
	}

	if err = sm.RevertService(serviceInfo); err == nil {
		t.Error("Should be error: service does not exist")
	}
}

func TestValidateService(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir: filepath.Join(tmpDir, "servicemanager", "services"),
		LayersDir:   filepath.Join(tmpDir, "layers"),
		DownloadDir: filepath.Join(tmpDir, "downloads"),
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %s", err)
	}

	serviceID := "testServiceValidate"

	serviceURL, fileInfo, _, err := prepareService("Service content", defaultServiceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %s", err)
	}

	if err = sm.InstallService(servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1},
		serviceURL, fileInfo); err != nil {
		t.Errorf("Can't install service: %s", err)
	}

	serviceInfo, err := sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Errorf("Can't get service info: %s", err)
	}

	if err = sm.ValidateService(serviceInfo); err != nil {
		t.Errorf("Error service validation: %s", err)
	}
}

func TestRemoveService(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir:    filepath.Join(tmpDir, "servicemanager", "services"),
		LayersDir:      filepath.Join(tmpDir, "layers"),
		DownloadDir:    filepath.Join(tmpDir, "downloads"),
		ServiceTTLDays: 0,
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	servicemanager.RemoveCachedServicesPeriod = 1 * time.Second

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %v", err)
	}

	serviceID := "testRemoveService"

	serviceURL, fileInfo, _, err := prepareService("Service content", defaultServiceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	serviceInfo := servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1}
	if err = sm.InstallService(serviceInfo, serviceURL, fileInfo); err != nil {
		t.Errorf("Can't install service: %v", err)
	}

	if err := sm.ApplyService(serviceInfo); err != nil {
		t.Fatalf("Can't apply service: %v", err)
	}

	if err = sm.RemoveService(serviceID); err != nil {
		t.Fatalf("Can't remove service: %v", err)
	}

	serviceInfo, err = sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Fatalf("Can't get service info: %v", err)
	}

	if !serviceInfo.Cached {
		t.Error("Service should be cached")
	}

	time.Sleep(2 * time.Second)

	if _, err = sm.GetServiceInfo(serviceID); err == nil {
		t.Error("Should be error service doesn't exist")
	}
}

func TestRestoreService(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir:    filepath.Join(tmpDir, "servicemanager", "services"),
		LayersDir:      filepath.Join(tmpDir, "layers"),
		DownloadDir:    filepath.Join(tmpDir, "downloads"),
		ServiceTTLDays: 2,
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %v", err)
	}

	serviceID := "testRestoreService"

	serviceURL, fileInfo, layerDigest, err := prepareService("Service content", defaultServiceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	layerProvider.digests = layerDigest

	serviceInfo := servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1}

	if err = sm.InstallService(serviceInfo, serviceURL, fileInfo); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}

	if err = sm.RestoreService(serviceInfo.ServiceID); err != nil {
		t.Fatalf("Can't restore service: %v", err)
	}

	if err := sm.UseService(serviceInfo.ServiceID, serviceInfo.AosVersion); err != nil {
		t.Fatalf("Can't set use service: %v", err)
	}

	service, err := sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Fatalf("Can't get service info: %v", err)
	}

	if service.Cached {
		t.Error("Service should not be cached")
	}

	serviceStorage.cachedServiceError = true

	if err = sm.RemoveService(serviceID); err == nil {
		t.Fatal("Should be error to set cached service")
	}

	serviceStorage.cachedServiceError = false

	if err = sm.RemoveService(serviceID); err != nil {
		t.Fatalf("Can't remove service: %v", err)
	}

	if err = sm.InstallService(serviceInfo, serviceURL, fileInfo); err == nil {
		t.Fatal("Should be error service cashed")
	}

	service, err = sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Fatalf("Can't get service info: %v", err)
	}

	if !service.Cached {
		t.Error("Service should be cached")
	}

	serviceStorage.cachedServiceError = true

	if err = sm.RestoreService(serviceInfo.ServiceID); err == nil {
		t.Fatal("Should be error to set cached service")
	}

	serviceStorage.cachedServiceError = false

	if err = sm.RestoreService(serviceInfo.ServiceID); err != nil {
		t.Fatalf("Can't remove service: %v", err)
	}

	service, err = sm.GetServiceInfo(serviceID)
	if err != nil {
		t.Fatalf("Can't get service info: %v", err)
	}

	if service.Cached {
		t.Error("Service should not be cached")
	}
}

func TestAllocateMemoryInstallService(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir:    filepath.Join(tmpDir, "servicemanager", "services"),
		DownloadDir:    filepath.Join(tmpDir, "downloads"),
		ServiceTTLDays: 2,
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{
		totalSize: 1 * megabyte,
	}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %v", err)
	}

	serviceSize := int64(512 * kilobyte)

	serviceURL, fileInfo, layersDigest, err := prepareService("Service content", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	layerProvider.digests = layersDigest

	serviceID := "memoryService"
	serviceInfo := servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1}

	if err = sm.InstallService(serviceInfo, serviceURL, fileInfo); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}

	serviceSize = int64(512 * kilobyte)

	serviceURL1, fileInfo1, _, err := prepareService("Service content1", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	serviceID1 := "memoryService2"
	serviceInfo1 := servicemanager.ServiceInfo{ServiceID: serviceID1, AosVersion: 1}

	if err = sm.InstallService(serviceInfo1, serviceURL1, fileInfo1); err == nil {
		t.Fatalf("Should be error install service")
	}

	if err = sm.UseService(serviceInfo.ServiceID, serviceInfo.AosVersion); err != nil {
		t.Fatalf("Can't set use service: %v", err)
	}

	if err = sm.RemoveService(serviceID); err != nil {
		t.Fatalf("Can't remove service: %v", err)
	}

	if serviceInfo, err = sm.GetServiceInfo(serviceID); err != nil {
		t.Fatalf("Can't get service info: %v", err)
	}

	if !serviceInfo.Cached {
		t.Error("Service should be cached")
	}

	if err = sm.InstallService(serviceInfo1, serviceURL1, fileInfo1); err != nil {
		t.Fatalf("Should be error install service")
	}

	if serviceInfo, err = sm.GetServiceInfo(serviceID); err == nil {
		t.Fatal("Service should not be exist")
	}
}

func TestCachedServiceOnStart(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir:    filepath.Join(tmpDir, "servicemanager", "services"),
		DownloadDir:    filepath.Join(tmpDir, "downloads"),
		ServiceTTLDays: 2,
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{
		totalSize: 1 * megabyte,
	}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %v", err)
	}

	serviceSize := int64(512 * kilobyte)

	serviceURL1, fileInfo1, layersDigest1, err := prepareService("Service content", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	layerProvider.digests = layersDigest1

	serviceID1 := "cachedStart1"
	serviceInfo1 := servicemanager.ServiceInfo{ServiceID: serviceID1, AosVersion: 1}

	if err = sm.InstallService(serviceInfo1, serviceURL1, fileInfo1); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}

	if err = sm.UseService(serviceID1, 1); err != nil {
		t.Fatalf("Can't set use service: %v", err)
	}

	serviceSize = int64(256 * kilobyte)

	serviceURL2, fileInfo2, _, err := prepareService("Service content2", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	serviceID2 := "cachedStart2"
	serviceInfo2 := servicemanager.ServiceInfo{ServiceID: serviceID2, AosVersion: 1}

	if err = sm.InstallService(serviceInfo2, serviceURL2, fileInfo2); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}

	serviceSize = int64(300 * kilobyte)

	serviceURL3, fileInfo3, _, err := prepareService("Service content3", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	serviceID3 := "cachedStart3"
	serviceInfo3 := servicemanager.ServiceInfo{ServiceID: serviceID3, AosVersion: 1}

	if err = sm.InstallService(serviceInfo3, serviceURL3, fileInfo3); err == nil {
		t.Fatalf("Should be install error")
	}

	if err = sm.RemoveService(serviceID1); err != nil {
		t.Fatalf("Can't remove service: %v", err)
	}

	sm.Close()

	sm1, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %v", err)
	}

	serviceInfo, err := sm1.GetServiceInfo(serviceID1)
	if err != nil {
		t.Fatalf("Can't get service info: %v", err)
	}

	if !serviceInfo.Cached {
		t.Fatal("Service should be cached")
	}

	if err = sm1.InstallService(serviceInfo3, serviceURL3, fileInfo3); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}
}

func TestFailCreateAllocator(t *testing.T) {
	serviceAllocator = &testAllocator{
		partLimit: 100,
	}

	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir:       filepath.Join(tmpDir, "servicemanager", "services"),
		DownloadDir:       filepath.Join(tmpDir, "downloads"),
		ServicesPartLimit: 110,
	}

	layerProvider := &testLayerProvider{}

	if _, err := servicemanager.New(config, serviceStorage, layerProvider); err == nil {
		t.Fatal("Should be error creating allocator")
	}
}

func TestRemoveServiceVersionOnInstall(t *testing.T) {
	serviceStorage := &testServiceStorage{}

	config := &config.Config{
		ServicesDir:    filepath.Join(tmpDir, "servicemanager", "services"),
		DownloadDir:    filepath.Join(tmpDir, "downloads"),
		ServiceTTLDays: 2,
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %v", err)
	}

	serviceSize := int64(200 * kilobyte)

	serviceURL1, fileInfo1, layersDigest, err := prepareService("Service content", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	layerProvider.digests = layersDigest

	serviceID1 := "cachedStart1"
	serviceInfo1 := servicemanager.ServiceInfo{ServiceID: serviceID1, AosVersion: 1}

	if err = sm.InstallService(serviceInfo1, serviceURL1, fileInfo1); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}

	if err = sm.UseService(serviceID1, 1); err != nil {
		t.Fatalf("Can't set use service: %v", err)
	}

	if err = sm.RemoveService(serviceID1); err != nil {
		t.Fatalf("Can't remove service: %v", err)
	}

	serviceSize = int64(212 * kilobyte)

	serviceURL2, fileInfo2, _, err := prepareService("Service content", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	serviceInfo1 = servicemanager.ServiceInfo{ServiceID: serviceID1, AosVersion: 2}

	if err = sm.InstallService(serviceInfo1, serviceURL2, fileInfo2); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}

	services, err := serviceStorage.GetAllServiceVersions(serviceID1)
	if err != nil {
		t.Fatalf("Can't get services version: %v", err)
	}

	if len(services) != 2 {
		t.Error("Unexpected services size")
	}

	serviceSize = int64(212 * kilobyte)

	serviceURL3, fileInfo3, _, err := prepareService("Service content", serviceSize)
	if err != nil {
		t.Fatalf("Can't prepare test service: %v", err)
	}

	serviceInfo1 = servicemanager.ServiceInfo{ServiceID: serviceID1, AosVersion: 3}

	if err = sm.InstallService(serviceInfo1, serviceURL3, fileInfo3); err != nil {
		t.Fatalf("Can't install service: %v", err)
	}

	services, err = serviceStorage.GetAllServiceVersions(serviceID1)
	if err != nil {
		t.Fatalf("Can't get services version: %v", err)
	}

	if len(services) != 2 {
		t.Error("Unexpected services size")
	}
}

func TestFailedServiceStorage(t *testing.T) {
	serviceStorage := &testServiceStorage{
		getAllError: true,
	}

	config := &config.Config{
		ServicesDir:    filepath.Join(tmpDir, "servicemanager", "services"),
		DownloadDir:    filepath.Join(tmpDir, "downloads"),
		ServiceTTLDays: 2,
	}

	layerProvider := &testLayerProvider{}

	serviceAllocator = &testAllocator{}

	if _, err := servicemanager.New(config, serviceStorage, layerProvider); err == nil {
		t.Fatal("Should be error: creating servicemanager")
	}

	serviceStorage = &testServiceStorage{
		getServiceVersionError: true,
	}

	serviceID := "serviceFail"
	serviceInfo := servicemanager.ServiceInfo{ServiceID: serviceID, AosVersion: 1, Cached: true}
	serviceStorage.Services = append(serviceStorage.Services, serviceInfo)

	if _, err := servicemanager.New(config, serviceStorage, layerProvider); err == nil {
		t.Fatal("Should be error: creating servicemanager")
	}

	serviceStorage = &testServiceStorage{}

	serviceAllocator = &testAllocator{}

	sm, err := servicemanager.New(config, serviceStorage, layerProvider)
	if err != nil {
		t.Fatalf("Can't create SM: %v", err)
	}

	if err := sm.ApplyService(serviceInfo); err == nil {
		t.Fatal("Should be error: apply service")
	}

	if err := sm.RevertService(servicemanager.ServiceInfo{ServiceID: "unknown", Cached: true}); err == nil {
		t.Fatal("Should be error: revert service")
	}

	if err := sm.RemoveService("unknown"); err == nil {
		t.Fatal("Should be error: remove service")
	}

	if err := sm.UseService("unknown", 1); err == nil {
		t.Fatal("Should be error: set use service")
	}
}

/***********************************************************************************************************************
* Interfaces
***********************************************************************************************************************/

func newSpaceAllocator(
	path string, partLimit uint, remover spaceallocator.ItemRemover,
) (spaceallocator.Allocator, error) {
	if serviceAllocator.partLimit < partLimit {
		return nil, aoserrors.New("can't create space allocator")
	}

	if remover != nil {
		serviceAllocator.remover = remover
	}

	return serviceAllocator, nil
}

func (allocator *testAllocator) AllocateSpace(size uint64) (spaceallocator.Space, error) {
	allocator.Lock()
	defer allocator.Unlock()

	if allocator.totalSize != 0 && allocator.allocatedSize+size > allocator.totalSize {
		for allocator.allocatedSize+size > allocator.totalSize {
			if len(allocator.outdatedItems) == 0 {
				return nil, spaceallocator.ErrNoSpace
			}

			if err := allocator.remover(allocator.outdatedItems[0].id); err != nil {
				return nil, err
			}

			if allocator.outdatedItems[0].size < allocator.allocatedSize {
				allocator.allocatedSize -= allocator.outdatedItems[0].size
			} else {
				allocator.allocatedSize = 0
			}

			allocator.outdatedItems = allocator.outdatedItems[1:]
		}
	}

	allocator.allocatedSize += size

	return &testSpace{allocator: allocator, size: size}, nil
}

func (allocator *testAllocator) FreeSpace(size uint64) {
	allocator.Lock()
	defer allocator.Unlock()

	if size > allocator.allocatedSize {
		allocator.allocatedSize = 0
	} else {
		allocator.allocatedSize -= size
	}
}

func (allocator *testAllocator) AddOutdatedItem(id string, size uint64, timestamp time.Time) error {
	allocator.outdatedItems = append(allocator.outdatedItems, testOutdatedItem{id: id, size: size})

	return nil
}

func (allocator *testAllocator) RestoreOutdatedItem(id string) {
	for i, item := range allocator.outdatedItems {
		if item.id == id {
			allocator.outdatedItems = append(allocator.outdatedItems[:i], allocator.outdatedItems[i+1:]...)

			break
		}
	}
}

func (allocator *testAllocator) Close() error {
	return nil
}

func (space *testSpace) Accept() error {
	return nil
}

func (space *testSpace) Release() error {
	space.allocator.FreeSpace(space.size)

	return nil
}

func (storage *testServiceStorage) GetService(serviceID string) (service servicemanager.ServiceInfo, err error) {
	if serviceID == errorGetServicID {
		return service, aoserrors.New("can't get service")
	}

	for _, storeService := range storage.Services {
		if storeService.ServiceID == serviceID && service.AosVersion < storeService.AosVersion {
			service = storeService
		}
	}

	if service.ServiceID != serviceID {
		return service, servicemanager.ErrNotExist
	}

	return service, nil
}

func (storage *testServiceStorage) GetLatestVersionServices() (services []servicemanager.ServiceInfo, err error) {
	if storage.getAllError {
		return nil, aoserrors.New("can't get services")
	}

StoreServiceLoop:
	for _, storeService := range storage.Services {
		var serviceExists bool

		for i, service := range services {
			if service.ServiceID == storeService.ServiceID {
				if service.AosVersion < storeService.AosVersion {
					services[i] = storeService

					continue StoreServiceLoop
				}

				serviceExists = true
			}
		}

		if !serviceExists {
			services = append(services, storeService)
		}
	}

	return services, nil
}

func (storage *testServiceStorage) GetCachedServices() (services []servicemanager.ServiceInfo, err error) {
	for _, service := range storage.Services {
		if service.Cached {
			services = append(services, service)
		}
	}

	return services, nil
}

func (storage *testServiceStorage) AddService(service servicemanager.ServiceInfo) (err error) {
	if service.ServiceID == errorAddServiceID {
		return aoserrors.New("can't add service")
	}

	storage.Services = append(storage.Services, service)

	return err
}

func (storage *testServiceStorage) GetAllServiceVersions(id string) (result []servicemanager.ServiceInfo, err error) {
	if storage.getServiceVersionError {
		return nil, servicemanager.ErrNotExist
	}

	for _, outService := range storage.Services {
		if outService.ServiceID == id {
			result = append(result, outService)
		}
	}

	if len(result) == 0 {
		return nil, servicemanager.ErrNotExist
	}

	return result, nil
}

func (storage *testServiceStorage) RemoveService(service servicemanager.ServiceInfo) error {
	for i, outService := range storage.Services {
		if outService.ServiceID == service.ServiceID && outService.AosVersion == service.AosVersion {
			storage.Services = append(storage.Services[:i], storage.Services[i+1:]...)

			return nil
		}
	}

	return servicemanager.ErrNotExist
}

func (storage *testServiceStorage) ActivateService(serviceID string, aosVersion uint64) error {
	for i, outService := range storage.Services {
		if outService.ServiceID == serviceID && outService.AosVersion == aosVersion {
			storage.Services[i].IsActive = true

			return nil
		}
	}

	return servicemanager.ErrNotExist
}

func (storage *testServiceStorage) SetServiceTimestamp(serviceID string, aosVersion uint64, timestamp time.Time) error {
	for i, serviceInfo := range storage.Services {
		if serviceInfo.ServiceID == serviceID {
			storage.Services[i].Timestamp = timestamp
			storage.Services[i].AosVersion = aosVersion

			return nil
		}
	}

	return servicemanager.ErrNotExist
}

func (storage *testServiceStorage) SetServiceCached(serviceID string, cached bool) error {
	if storage.cachedServiceError {
		return aoserrors.New("can't get cached service")
	}

	for i, serviceInfo := range storage.Services {
		if serviceInfo.ServiceID == serviceID {
			storage.Services[i].Cached = cached

			return nil
		}
	}

	return servicemanager.ErrNotExist
}

func (layerProvider *testLayerProvider) UseLayer(digest string) error {
	for _, dgst := range layerProvider.digests {
		if dgst == digest {
			return nil
		}
	}

	return aoserrors.New("can't find digest")
}

/***********************************************************************************************************************
* Private
***********************************************************************************************************************/

func setup() (err error) {
	if tmpDir, err = ioutil.TempDir("", "aos_"); err != nil {
		return aoserrors.Wrap(err)
	}

	servicemanager.NewSpaceAllocator = newSpaceAllocator

	return nil
}

func cleanup() {
	os.RemoveAll(tmpDir)
}

func prepareService(
	testContent string, servicelayerSize int64,
) (outputURL string, fileInfo image.FileInfo, layersDigest []string, err error) {
	imageDir, err := ioutil.TempDir("", "aos_")
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	defer os.RemoveAll(imageDir)

	// create dir
	if err := os.MkdirAll(filepath.Join(imageDir, "rootfs", "home"), 0o755); err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	file, err := os.Create(filepath.Join(imageDir, "rootfs", "home", "service.py"))
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	if err := file.Truncate(servicelayerSize); err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	rootFsPath := filepath.Join(imageDir, "rootfs")

	serviceSize, err := fs.GetDirSize(rootFsPath)
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	fsDigest, err := generateFsLayer(imageDir, rootFsPath)
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	aosSrvConfigDigest, err := generateAndSaveDigest(filepath.Join(imageDir, blobsFolder), []byte("{}"))
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	imgSpecDigestDigest, err := generateAndSaveDigest(filepath.Join(imageDir, blobsFolder), []byte("{}"))
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	imgAosLayerDigest, err := generateAndSaveDigest(imageDir, []byte("{}"))
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	layersDigest = append(layersDigest, string(imgAosLayerDigest))

	if err := genarateImageManfest(
		imageDir, &imgSpecDigestDigest, &aosSrvConfigDigest, &fsDigest,
		serviceSize, []digest.Digest{imgAosLayerDigest}); err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	imageFile, err := ioutil.TempFile("", "aos_")
	if err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	outputURL = imageFile.Name()
	imageFile.Close()

	if err = packImage(imageDir, outputURL); err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	if fileInfo, err = image.CreateFileInfo(context.Background(), outputURL); err != nil {
		return outputURL, fileInfo, layersDigest, aoserrors.Wrap(err)
	}

	return "file://" + outputURL, fileInfo, layersDigest, nil
}

func generateFsLayer(imgFolder, rootfs string) (digest digest.Digest, err error) {
	blobsDir := filepath.Join(imgFolder, blobsFolder)
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		return digest, aoserrors.Wrap(err)
	}

	tarFile := filepath.Join(blobsDir, "_temp.tar.gz")

	if output, err := exec.Command("tar", "-C", rootfs, "-czf", tarFile, "./").CombinedOutput(); err != nil {
		return digest, aoserrors.Errorf("error: %s, code: %s", string(output), err)
	}
	defer os.Remove(tarFile)

	file, err := os.Open(tarFile)
	if err != nil {
		return digest, aoserrors.Wrap(err)
	}
	defer file.Close()

	byteValue, err := ioutil.ReadAll(file)
	if err != nil {
		return digest, aoserrors.Wrap(err)
	}

	digest, err = generateAndSaveDigest(blobsDir, byteValue)
	if err != nil {
		return digest, aoserrors.Wrap(err)
	}

	os.RemoveAll(rootfs)

	return digest, nil
}

func generateAndSaveDigest(folder string, data []byte) (retDigest digest.Digest, err error) {
	fullPath := filepath.Join(folder, "sha256")
	if err := os.MkdirAll(fullPath, 0o755); err != nil {
		return retDigest, aoserrors.Wrap(err)
	}

	h := sha256.New()
	h.Write(data)
	retDigest = digest.NewDigest("sha256", h)

	file, err := os.Create(filepath.Join(fullPath, retDigest.Hex()))
	if err != nil {
		return retDigest, aoserrors.Wrap(err)
	}
	defer file.Close()

	_, err = file.Write(data)
	if err != nil {
		return retDigest, aoserrors.Wrap(err)
	}

	return retDigest, nil
}

func genarateImageManfest(folderPath string, imgConfig, aosSrvConfig, rootfsLayer *digest.Digest,
	rootfsLayerSize int64, srvLayers []digest.Digest,
) (err error) {
	type serviceManifest struct {
		imagespec.Manifest
		AosService *imagespec.Descriptor `json:"aosService,omitempty"`
	}

	var manifest serviceManifest
	manifest.SchemaVersion = 2

	manifest.Config = imagespec.Descriptor{
		MediaType: "application/vnd.oci.image.config.v1+json",
		Digest:    *imgConfig,
	}

	if aosSrvConfig != nil {
		manifest.AosService = &imagespec.Descriptor{
			MediaType: "application/vnd.aos.service.config.v1+json",
			Digest:    *aosSrvConfig,
		}
	}

	layerDescriptor := imagespec.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    *rootfsLayer,
		Size:      rootfsLayerSize,
	}

	manifest.Layers = append(manifest.Layers, layerDescriptor)

	for _, layerDigest := range srvLayers {
		layerDescriptor := imagespec.Descriptor{
			MediaType: "application/vnd.aos.image.layer.v1.tar",
			Digest:    layerDigest,
		}

		manifest.Layers = append(manifest.Layers, layerDescriptor)
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	jsonFile, err := os.Create(filepath.Join(folderPath, "manifest.json"))
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err := jsonFile.Write(data); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func packImage(source, name string) (err error) {
	log.WithFields(log.Fields{"source": source, "name": name}).Debug("Pack image")

	if output, err := exec.Command("tar", "-C", source, "-cf", name, "./").CombinedOutput(); err != nil {
		return aoserrors.Errorf("tar error: %s, code: %s", string(output), err)
	}

	return nil
}
