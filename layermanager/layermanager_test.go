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

package layermanager_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/image"
	"github.com/aoscloud/aos_common/spaceallocator"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"

	"github.com/aoscloud/aos_servicemanager/config"
	"github.com/aoscloud/aos_servicemanager/layermanager"
)

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

const (
	kilobyte = uint64(1 << 10)
	megabyte = uint64(1 << 20)
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

type testLayerStorage struct {
	layers       []layermanager.LayerInfo
	addLayerFail bool
	getLayerFail bool
}

type testAllocator struct {
	sync.Mutex

	totalSize     uint64
	allocatedSize uint64
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
	tmpDir         string
	layersDir      string
	layerAllocator = &testAllocator{}
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
 **********************************************************************************************************************/

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
 **********************************************************************************************************************/

func TestInstallRemoveLayer(t *testing.T) {
	testLayerStorage := &testLayerStorage{}

	layerAllocator = &testAllocator{}

	defer func() {
		if err := os.RemoveAll(layersDir); err != nil {
			t.Errorf("Can't remove layers dir: %v", err)
		}
	}()

	layermanager.RemoveCachedLayersPeriod = 1 * time.Second

	layerManager, err := layermanager.New(
		&config.Config{
			LayersDir:    layersDir,
			ExtractDir:   filepath.Join(tmpDir, "extract"),
			DownloadDir:  filepath.Join(tmpDir, "download"),
			LayerTTLDays: 0,
		}, testLayerStorage)
	if err != nil {
		t.Fatalf("Can't create layer manager: %s", err)
	}
	defer layerManager.Close()

	sizeLayerContent := int64(2 * kilobyte)

	layerFile, digest, fileInfo, err := createLayer(filepath.Join(tmpDir, "layerdir1"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	layer := layermanager.LayerInfo{LayerID: "LayerId1", Digest: digest, AosVersion: 1}

	if err = layerManager.InstallLayer(layer, layerFile, fileInfo); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}

	if err = layerManager.InstallLayer(layer, layerFile, fileInfo); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}

	list, err := layerManager.GetLayersInfo()
	if err != nil {
		t.Fatalf("Can't get layer list: %v", err)
	}

	if len(list) != 1 {
		t.Error("Count of layers should be 1")
	}

	layerInfo, err := layerManager.GetLayerInfoByDigest(digest)
	if err != nil {
		t.Fatalf("Can't get layer info: %v", err)
	}

	if layerInfo.Digest != digest {
		t.Errorf("Unexpected layer digest: %v", layerInfo.Digest)
	}

	if layerInfo.Path != filepath.Join(tmpDir, "layers", strings.Replace(digest, ":", "/", 1)) {
		t.Errorf("Unexpected layer Path: %v", layerInfo.Path)
	}

	sizeLayerContent = int64(3 * kilobyte)

	layerFile1, digest1, fileInfo1, err := createLayer(filepath.Join(tmpDir, "layerdir2"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	layer1 := layermanager.LayerInfo{LayerID: "LayerId2", Digest: digest1, AosVersion: 1}

	if err = layerManager.InstallLayer(layer1, layerFile1, fileInfo1); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}

	if list, err = layerManager.GetLayersInfo(); err != nil {
		t.Fatalf("Can't get layer list: %v", err)
	}

	if len(list) != 2 {
		t.Error("Count of layers should be 2")
	}

	if err = layerManager.RemoveLayer(digest); err != nil {
		t.Fatalf("Can't remove layer: %v", err)
	}

	layerInfo, err = layerManager.GetLayerInfoByDigest(digest)
	if err != nil {
		t.Fatalf("Can't get layer info: %v", err)
	}

	if !layerInfo.Cached {
		t.Error("Layer should be cached")
	}

	time.Sleep(2 * time.Second)

	if _, err = layerManager.GetLayerInfoByDigest(digest); err == nil {
		t.Fatal("Layer should not be exist")
	}

	testLayerStorage.addLayerFail = true
	sizeLayerContent = int64(2 * kilobyte)

	layerFile2, digest2, fileInfo2, err := createLayer(filepath.Join(tmpDir, "layerdir3"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	layer2 := layermanager.LayerInfo{LayerID: "LayerId3", Digest: digest2, AosVersion: 1}

	if err = layerManager.InstallLayer(layer2, layerFile2, fileInfo2); err == nil {
		t.Fatal("Layer should not be installed")
	}

	testLayerStorage.getLayerFail = true

	if _, err = layerManager.GetLayersInfo(); err == nil {
		t.Fatal("Should be error: can't get layers list")
	}
}

func TestRestoreLayer(t *testing.T) {
	testLayerStorage := &testLayerStorage{}

	layerAllocator = &testAllocator{}

	defer func() {
		if err := os.RemoveAll(layersDir); err != nil {
			t.Errorf("Can't remove layers dir: %v", err)
		}
	}()

	layerManager, err := layermanager.New(
		&config.Config{
			LayersDir:    layersDir,
			ExtractDir:   filepath.Join(tmpDir, "extract"),
			DownloadDir:  filepath.Join(tmpDir, "download"),
			LayerTTLDays: 2,
		}, testLayerStorage)
	if err != nil {
		t.Fatalf("Can't create layer manager: %s", err)
	}
	defer layerManager.Close()

	sizeLayerContent := int64(2 * kilobyte)

	layerFile, digest, fileInfo, err := createLayer(filepath.Join(tmpDir, "layerdir1"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	layer := layermanager.LayerInfo{LayerID: "LayerId1", Digest: digest, AosVersion: 1}

	if err = layerManager.InstallLayer(layer, layerFile, fileInfo); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}

	if err = layerManager.UseLayer(digest); err != nil {
		t.Errorf("Can't set use layer: %v", err)
	}

	layerInfo, err := layerManager.GetLayerInfoByDigest(digest)
	if err != nil {
		t.Fatalf("Can't get layer info: %v", err)
	}

	if layerInfo.Cached {
		t.Error("Layer should not be cached")
	}

	if err := layerManager.RemoveLayer(digest); err != nil {
		t.Fatalf("Can't remove layer: %v", err)
	}

	if layerInfo, err = layerManager.GetLayerInfoByDigest(digest); err != nil {
		t.Fatalf("Can't get layer info by digest: %v", err)
	}

	if !layerInfo.Cached {
		t.Error("Layer should be cached")
	}

	if err := layerManager.RestoreLayer(digest); err != nil {
		t.Fatalf("Can't restore layer: %v", err)
	}

	if layerInfo, err = layerManager.GetLayerInfoByDigest(digest); err != nil {
		t.Fatalf("Can't get layer info by digest: %v", err)
	}

	if layerInfo.Cached {
		t.Error("Layer should not be cached")
	}
}

func TestRemoveDemageLayerFolder(t *testing.T) {
	testStorage := &testLayerStorage{}

	layerAllocator = &testAllocator{}

	defer func() {
		if err := os.RemoveAll(layersDir); err != nil {
			t.Errorf("Can't remove layers dir: %v", err)
		}
	}()

	layerManager, err := layermanager.New(
		&config.Config{
			LayersDir:   layersDir,
			ExtractDir:  filepath.Join(tmpDir, "extract"),
			DownloadDir: filepath.Join(tmpDir, "download"),
		}, testStorage)
	if err != nil {
		t.Fatalf("Can't create layer manager: %v", err)
	}

	sizeLayerContent := int64(1 * kilobyte)

	layerFile, digest, fileInfo, err := createLayer(filepath.Join(tmpDir, "layerdir1"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	if err = layerManager.InstallLayer(
		layermanager.LayerInfo{LayerID: "LayerId1", Digest: digest, AosVersion: 1},
		layerFile, fileInfo); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}

	testLayerDir := filepath.Join(tmpDir, "layers/sha256/test")
	if err := os.MkdirAll(testLayerDir, 0o755); err != nil {
		t.Fatalf("Can't create test layer: %v", err)
	}

	if _, err := os.Stat(testLayerDir); err != nil {
		t.Fatalf("Test layer folder does not exist: %v", err)
	}

	layerAllocator = &testAllocator{}

	layerManager, err = layermanager.New(
		&config.Config{
			LayersDir:   layersDir,
			ExtractDir:  filepath.Join(tmpDir, "extract"),
			DownloadDir: filepath.Join(tmpDir, "download"),
		}, testStorage)
	if err != nil {
		t.Fatalf("Can't create layer manager: %v", err)
	}

	if _, err := os.Stat(testLayerDir); err == nil {
		t.Error("Test layer folder should be deleted")
	}

	layerFile, digest, fileInfo, err = createLayer(filepath.Join(tmpDir, "layerdir2"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	if err = layerManager.InstallLayer(
		layermanager.LayerInfo{LayerID: "LayerId2", Digest: digest, AosVersion: 1},
		layerFile, fileInfo); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}

	layer, err := layerManager.GetLayerInfoByDigest(digest)
	if err != nil {
		t.Fatalf("Can't get layer: %v", err)
	}

	if err = os.RemoveAll(layer.Path); err != nil {
		t.Fatalf("Can't remove layer path: %v", err)
	}

	layerManager.Close()

	layerAllocator = &testAllocator{}

	layerManager, err = layermanager.New(
		&config.Config{
			LayersDir:   layersDir,
			ExtractDir:  filepath.Join(tmpDir, "extract"),
			DownloadDir: filepath.Join(tmpDir, "download"),
		}, testStorage)
	if err != nil {
		t.Fatalf("Can't create layer manager: %v", err)
	}
	defer layerManager.Close()

	if _, err = layerManager.GetLayerInfoByDigest(digest); err == nil {
		t.Fatal("Should be error: layer doesn't exist")
	}
}

func TestRemoteDownloadLayer(t *testing.T) {
	layerAllocator = &testAllocator{}

	defer func() {
		if err := os.RemoveAll(layersDir); err != nil {
			t.Errorf("Can't remove layers dir: %v", err)
		}
	}()

	layerManager, err := layermanager.New(
		&config.Config{
			LayersDir:   layersDir,
			ExtractDir:  filepath.Join(tmpDir, "extract"),
			DownloadDir: filepath.Join(tmpDir, "download"),
		}, &testLayerStorage{})
	if err != nil {
		t.Fatalf("Can't create layer manager: %v", err)
	}
	defer layerManager.Close()

	sizeLayerContent := int64(10 * kilobyte)

	layerFile, digest, fileInfo, err := createLayer(filepath.Join(tmpDir, "layerdir1"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	fileServerDir, err := ioutil.TempDir("", "layers_fileserver")
	if err != nil {
		t.Fatalf("Error create temporary dir: %v", err)
	}

	defer os.RemoveAll(fileServerDir)

	urlVal, err := url.Parse(layerFile)
	if err != nil {
		t.Fatalf("Can't parse url: %v", err)
	}

	if err = os.Rename(urlVal.Path, filepath.Join(fileServerDir, "downloadImage")); err != nil {
		t.Fatalf("Can't rename directory: %v", err)
	}

	server := &http.Server{
		Addr:              ":9000",
		Handler:           http.FileServer(http.Dir(fileServerDir)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("Can't serve http server: %v", err)
		}
	}()

	time.Sleep(1 * time.Second)

	defer server.Close()

	if err = layerManager.InstallLayer(
		layermanager.LayerInfo{LayerID: "LayerId1", Digest: digest, AosVersion: 1},
		"http://:9000/downloadImage", fileInfo); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}
}

func TestInstallLayerNotEnoughSpace(t *testing.T) {
	layerAllocator = &testAllocator{
		totalSize: 1 * megabyte,
	}

	defer func() {
		if err := os.RemoveAll(layersDir); err != nil {
			t.Errorf("Can't remove layers dir: %v", err)
		}
	}()

	layerManager, err := layermanager.New(
		&config.Config{
			LayersDir:    layersDir,
			ExtractDir:   filepath.Join(tmpDir, "extract"),
			DownloadDir:  filepath.Join(tmpDir, "download"),
			LayerTTLDays: 2,
		}, &testLayerStorage{})
	if err != nil {
		t.Fatalf("Can't create layer manager: %v", err)
	}
	defer layerManager.Close()

	sizeLayerContent := int64(512 * kilobyte)

	layerFile, digest, fileInfo, err := createLayer(filepath.Join(tmpDir, "layerdir1"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	if err = layerManager.InstallLayer(
		layermanager.LayerInfo{LayerID: "LayerId1", Digest: digest, AosVersion: 1},
		layerFile, fileInfo); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}

	sizeLayerContent = int64(520 * kilobyte)

	layerFile1, digest1, fileInfo1, err := createLayer(filepath.Join(tmpDir, "layerdir2"), sizeLayerContent)
	if err != nil {
		t.Fatalf("Can't create layer: %v", err)
	}

	if err = layerManager.InstallLayer(
		layermanager.LayerInfo{LayerID: "LayerId2", Digest: digest1, AosVersion: 1},
		layerFile1, fileInfo1); err == nil {
		t.Fatal("Should be error install layer")
	}

	if err := layerManager.UseLayer(digest); err != nil {
		t.Fatalf("Can't set use layer: %v", err)
	}

	if err := layerManager.RemoveLayer(digest); err != nil {
		t.Fatalf("Can't uninstall layer: %v", err)
	}

	layerInfo, err := layerManager.GetLayerInfoByDigest(digest)
	if err != nil {
		t.Fatalf("Can't get layer info: %v", err)
	}

	if !layerInfo.Cached {
		t.Fatal("Layer should be cached")
	}

	if err = layerManager.InstallLayer(
		layermanager.LayerInfo{LayerID: "LayerId2", Digest: digest1, AosVersion: 1},
		layerFile1, fileInfo1); err != nil {
		t.Fatalf("Can't install layer: %v", err)
	}
}

/***********************************************************************************************************************
 * Interfaces
 **********************************************************************************************************************/

func newSpaceAllocator(
	path string, partLimit uint, remover spaceallocator.ItemRemover,
) (spaceallocator.Allocator, error) {
	switch path {
	case layersDir:
		layerAllocator.remover = remover
		return layerAllocator, nil

	default:
		return &testAllocator{remover: remover}, nil
	}
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

func (infoProvider *testLayerStorage) AddLayer(layerInfo layermanager.LayerInfo) (err error) {
	if infoProvider.addLayerFail {
		return aoserrors.New("can't add layer")
	}

	for _, layer := range infoProvider.layers {
		if layer.Digest == layerInfo.Digest {
			return aoserrors.New("layer exist")
		}
	}

	infoProvider.layers = append(infoProvider.layers, layerInfo)

	return nil
}

func (infoProvider *testLayerStorage) DeleteLayerByDigest(digest string) (err error) {
	for i, layer := range infoProvider.layers {
		if layer.Digest == digest {
			infoProvider.layers = append(infoProvider.layers[:i], infoProvider.layers[i+1:]...)

			return nil
		}
	}

	return aoserrors.New("layer not found")
}

func (infoProvider *testLayerStorage) GetLayersInfo() (layersList []layermanager.LayerInfo, err error) {
	if infoProvider.getLayerFail {
		return nil, aoserrors.New("can't get layers info")
	}

	layersList = infoProvider.layers

	return layersList, nil
}

func (infoProvider *testLayerStorage) GetLayerInfoByDigest(
	digest string,
) (layerInfo layermanager.LayerInfo, err error) {
	for _, layer := range infoProvider.layers {
		if layer.Digest == digest {
			return layer, nil
		}
	}

	return layerInfo, aoserrors.New("layer not found")
}

func (infoProvider *testLayerStorage) SetLayerCached(digest string, cached bool) error {
	for i, layer := range infoProvider.layers {
		if layer.Digest == digest {
			infoProvider.layers[i].Cached = cached

			return nil
		}
	}

	return aoserrors.New("layer not found")
}

func (infoProvider *testLayerStorage) SetLayerTimestamp(digest string, timestamp time.Time) error {
	for i, layer := range infoProvider.layers {
		if layer.Digest == digest {
			infoProvider.layers[i].Timestamp = timestamp

			return nil
		}
	}

	return aoserrors.New("layer not found")
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func setup() (err error) {
	if tmpDir, err = ioutil.TempDir("", "aos_"); err != nil {
		return aoserrors.Wrap(err)
	}

	layersDir = filepath.Join(tmpDir, "layers")

	layermanager.NewSpaceAllocator = newSpaceAllocator

	return nil
}

func cleanup() {
	os.RemoveAll(tmpDir)
}

func createLayer(
	dir string, sizeLayerContent int64,
) (layerFile string, digest string, fileInfo image.FileInfo, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}
	defer os.RemoveAll(dir)

	tmpLayerFolder := filepath.Join(tmpDir, "tmpLayerDir")
	if err := os.MkdirAll(tmpLayerFolder, 0o755); err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	file, err := os.Create(filepath.Join(tmpLayerFolder, "layer.txt"))
	if err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	defer os.RemoveAll(tmpLayerFolder)

	if err := file.Truncate(sizeLayerContent); err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	tarFile := filepath.Join(dir, "layer.tar")

	if output, err := exec.Command("tar", "-C", tmpLayerFolder, "-cf", tarFile, "./").CombinedOutput(); err != nil {
		return "", "", fileInfo, aoserrors.New(fmt.Sprintf("error: %s, code: %s", string(output), err))
	}
	defer os.Remove(tarFile)

	file, err = os.Open(tarFile)
	if err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}
	defer file.Close()

	byteValue, err := ioutil.ReadAll(file)
	if err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	layerDigest, err := generateAndSaveDigest(dir, byteValue)
	if err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	layerDescriptor := imagespec.Descriptor{
		MediaType: "application/vnd.aos.image.layer.v1.tar+gzip",
		Digest:    layerDigest,
		Size:      sizeLayerContent,
	}

	dataJSON, err := json.Marshal(layerDescriptor)
	if err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	jsonFile, err := os.Create(filepath.Join(dir, "layer.json"))
	if err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	if _, err := jsonFile.Write(dataJSON); err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	layerFile = filepath.Join(tmpDir, layerDigest.Hex()+".tar.gz")
	if output, err := exec.Command("tar", "-C", dir, "-czf", layerFile, "./").CombinedOutput(); err != nil {
		return "", "", fileInfo, aoserrors.New(fmt.Sprintf("error: %s, code: %s", string(output), err))
	}

	if fileInfo, err = image.CreateFileInfo(context.Background(), layerFile); err != nil {
		return "", "", fileInfo, aoserrors.Wrap(err)
	}

	return "file://" + layerFile, string(layerDigest), fileInfo, nil
}

func generateAndSaveDigest(folder string, data []byte) (retDigest digest.Digest, err error) {
	h := sha256.New()
	h.Write(data)

	retDigest = digest.NewDigest("sha256", h)

	file, err := os.Create(filepath.Join(folder, retDigest.Hex()))
	if err != nil {
		return "", aoserrors.Wrap(err)
	}
	defer file.Close()

	if _, err = file.Write(data); err != nil {
		return "", aoserrors.Wrap(err)
	}

	return retDigest, nil
}
