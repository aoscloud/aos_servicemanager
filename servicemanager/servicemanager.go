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

package servicemanager

import (
	"bytes"
	"context"
	"errors"
	"io/ioutil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/image"
	"github.com/aoscloud/aos_common/spaceallocator"
	"github.com/aoscloud/aos_common/utils/action"
	"github.com/opencontainers/go-digest"
	log "github.com/sirupsen/logrus"
	"golang.org/x/mod/sumdb/dirhash"

	"github.com/aoscloud/aos_servicemanager/config"
	"github.com/aoscloud/aos_servicemanager/utils/uidgidpool"
	"github.com/aoscloud/aos_servicemanager/utils/whiteouts"
)

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

const (
	maxConcurrentActions = 10
	tmpRootFSDir         = "tmprootfs"
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

// ServiceStorage provides API to create, remove or access services DB.
type ServiceStorage interface {
	GetService(serviceID string) (ServiceInfo, error)
	GetLatestVersionServices() ([]ServiceInfo, error)
	GetCachedServices() ([]ServiceInfo, error)
	AddService(ServiceInfo) error
	GetAllServiceVersions(serviceID string) ([]ServiceInfo, error)
	RemoveService(ServiceInfo) error
	ActivateService(serviceID string, aosVersion uint64) error
	SetServiceTimestamp(serviceID string, aosVersion uint64, timestamp time.Time) error
	SetServiceCached(serviceID string, cached bool) error
}

// LayerProvider layer provider.
type LayerProvider interface {
	UseLayer(digest string) error
}

// ServiceManager instance.
type ServiceManager struct {
	sync.Mutex
	servicesDir            string
	downloadDir            string
	serviceTTLDays         uint64
	actionHandler          *action.Handler
	gidPool                *uidgidpool.IdentifierPool
	serviceInfoProvider    ServiceStorage
	layerProvider          LayerProvider
	serviceAllocator       spaceallocator.Allocator
	downloadAllocator      spaceallocator.Allocator
	validateTTLStopChannel chan struct{}
}

// ServiceInfo service information.
type ServiceInfo struct {
	ServiceID       string
	AosVersion      uint64
	ServiceProvider string
	Description     string
	ImagePath       string
	GID             int
	ManifestDigest  []byte
	Timestamp       time.Time
	IsActive        bool
	Cached          bool
	Size            uint64
}

/***********************************************************************************************************************
 * Vars
 **********************************************************************************************************************/

var (
	// ErrNotExist not exist service error.
	ErrNotExist = errors.New("service not exist")
	// ErrVersionMismatch new service version <= existing one.
	ErrVersionMismatch = errors.New("version mismatch")
)

// NewSpaceAllocator space allocator constructor.
// nolint:gochecknoglobals // used for unit test mock
var NewSpaceAllocator = spaceallocator.New

// RemoveCachedServicesPeriod global variable is used to be able to mocking the services TTL functionality in test.
// nolint:gochecknoglobals // used for unit test mock
var RemoveCachedServicesPeriod = 24 * time.Hour

/***********************************************************************************************************************
 * Public
 **********************************************************************************************************************/

// New creates new service manager object.
func New(
	config *config.Config, serviceInfoProvider ServiceStorage, layerProvider LayerProvider,
) (sm *ServiceManager, err error) {
	sm = &ServiceManager{
		servicesDir:            config.ServicesDir,
		downloadDir:            config.DownloadDir,
		serviceTTLDays:         config.ServiceTTLDays,
		actionHandler:          action.New(maxConcurrentActions),
		gidPool:                uidgidpool.NewGroupIDPool(),
		serviceInfoProvider:    serviceInfoProvider,
		layerProvider:          layerProvider,
		validateTTLStopChannel: make(chan struct{}),
	}

	if err = os.MkdirAll(sm.servicesDir, 0o755); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	if err := os.RemoveAll(sm.downloadDir); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	if err := os.MkdirAll(sm.downloadDir, 0o755); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	if sm.serviceAllocator, err = NewSpaceAllocator(
		sm.servicesDir, config.ServicesPartLimit, sm.removeOutdatedService); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	if sm.downloadAllocator, err = NewSpaceAllocator(sm.downloadDir, 0, nil); err != nil {
		return nil, aoserrors.Wrap(err)
	}

	services, err := sm.serviceInfoProvider.GetLatestVersionServices()
	if err != nil {
		return nil, aoserrors.Wrap(err)
	}

	for _, service := range services {
		if err = sm.gidPool.AddID(service.GID); err != nil {
			log.Errorf("Can't add service GID to pool: %v", err)
		}

		if service.Cached {
			size, err := sm.getServiceSize(service.ServiceID)
			if err != nil {
				return nil, aoserrors.Wrap(err)
			}

			if err = sm.serviceAllocator.AddOutdatedItem(
				service.ServiceID, size, service.Timestamp); err != nil {
				return nil, aoserrors.Wrap(err)
			}
		}
	}

	if err := sm.removeDamagedServiceFolders(services); err != nil {
		log.Errorf("Can't remove damaged service folders: %v", err)
	}

	if err := sm.removeOutdatedServices(); err != nil {
		log.Errorf("Can't remove outdated services: %v", err)
	}

	go sm.validateTTLs()

	return sm, nil
}

func (sm *ServiceManager) Close() {
	if err := sm.serviceAllocator.Close(); err != nil {
		log.Errorf("Can't close service allocator: %v", err)
	}

	if err := sm.downloadAllocator.Close(); err != nil {
		log.Errorf("Can't close download allocator: %v", err)
	}

	close(sm.validateTTLStopChannel)
}

// InstallService install service to the system.
func (sm *ServiceManager) InstallService(
	newService ServiceInfo, imageURL string, fileInfo image.FileInfo,
) (err error) {
	return <-sm.actionHandler.Execute(newService.ServiceID,
		func(id string) error {
			return sm.doInstallService(newService, imageURL, fileInfo)
		},
	)
}

func (sm *ServiceManager) RestoreService(serviceID string) error {
	return <-sm.actionHandler.Execute(serviceID,
		func(id string) error {
			return sm.doRestoreService(serviceID)
		},
	)
}

// RemoveService removes service from the system.
func (sm *ServiceManager) RemoveService(serviceID string) (err error) {
	return <-sm.actionHandler.Execute(serviceID,
		func(id string) error {
			return sm.doRemoveService(serviceID)
		},
	)
}

// GetAllServicesStatus gets all services status.
func (sm *ServiceManager) GetAllServicesStatus() ([]ServiceInfo, error) {
	services, err := sm.serviceInfoProvider.GetLatestVersionServices()
	return services, aoserrors.Wrap(err)
}

// GetServiceInfo gets service information by id.
func (sm *ServiceManager) GetServiceInfo(serviceID string) (serviceInfo ServiceInfo, err error) {
	serviceInfo, err = sm.serviceInfoProvider.GetService(serviceID)
	return serviceInfo, aoserrors.Wrap(err)
}

// GetImageParts gets image parts for the service.
func (sm *ServiceManager) GetImageParts(service ServiceInfo) (parts ImageParts, err error) {
	return getImageParts(service.ImagePath)
}

// ApplyService applies already installed service.
func (sm *ServiceManager) ApplyService(service ServiceInfo) (err error) {
	return <-sm.actionHandler.Execute(service.ServiceID,
		func(id string) error {
			return sm.doApplyService(service)
		},
	)
}

// RevertService reverts already installed service.
func (sm *ServiceManager) RevertService(service ServiceInfo) (retErr error) {
	return <-sm.actionHandler.Execute(service.ServiceID,
		func(id string) error {
			return sm.doRevertService(service)
		},
	)
}

// UseService sets last use of service.
func (sm *ServiceManager) UseService(serviceID string, aosVersion uint64) error {
	if err := sm.serviceInfoProvider.SetServiceTimestamp(serviceID, aosVersion, time.Now().UTC()); err != nil {
		return aoserrors.Wrap(err)
	}

	serviceInfo, err := sm.serviceInfoProvider.GetService(serviceID)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	manifest, err := getImageManifest(serviceInfo.ImagePath)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	for _, digest := range getLayersFromManifest(manifest) {
		if err := sm.layerProvider.UseLayer(digest); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	return nil
}

func (sm *ServiceManager) ValidateService(service ServiceInfo) error {
	manifestCheckSum, err := getManifestChecksum(service.ImagePath)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if !bytes.Equal(service.ManifestDigest, manifestCheckSum) {
		return aoserrors.New("manifest checksum mismatch")
	}

	return validateUnpackedImage(service.ImagePath)
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func (sm *ServiceManager) doRestoreService(serviceID string) error {
	log.WithFields(log.Fields{"serviceID": serviceID}).Debug("Restore service")

	service, err := sm.serviceInfoProvider.GetService(serviceID)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if !service.Cached {
		log.Warningf("Service %v not cached", serviceID)

		return nil
	}

	if err = sm.setServiceCached(service, false); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (sm *ServiceManager) doInstallService(newService ServiceInfo, imageURL string, fileInfo image.FileInfo) error {
	log.WithFields(log.Fields{"serviceID": newService.ServiceID}).Debug("Install service")

	serviceFromStorage, err := sm.serviceInfoProvider.GetService(newService.ServiceID)
	if err != nil && !errors.Is(err, ErrNotExist) {
		return aoserrors.Wrap(err)
	}

	var isServiceExist bool

	if err == nil {
		isServiceExist = true

		if newService.AosVersion <= serviceFromStorage.AosVersion {
			return ErrVersionMismatch
		}
	}

	var spacePackage, spaceService spaceallocator.Space

	newService.ImagePath, newService.Size, spacePackage, err = sm.extractPackageByURL(imageURL, fileInfo)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	defer func() {
		if err != nil {
			releaseAllocatedSpace(newService.ImagePath, spaceService, spacePackage)

			log.WithFields(log.Fields{
				"id":         newService.ServiceID,
				"aosVersion": newService.AosVersion,
				"imagePath":  newService.ImagePath,
			}).Errorf("Can't install service: %v", err)

			return
		}

		acceptAllocatedSpace(spaceService, spacePackage)
	}()

	if err = validateUnpackedImage(newService.ImagePath); err != nil {
		return aoserrors.Wrap(err)
	}

	if isServiceExist {
		newService.GID = serviceFromStorage.GID
	} else {
		newService.GID, err = sm.gidPool.GetFreeID()
		if err != nil {
			return aoserrors.Wrap(err)
		}
	}

	serviceSize, space, rootFSDigest, err := sm.prepareServiceFS(newService.ImagePath, newService.GID)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	spaceService = space
	newService.Size += uint64(serviceSize)

	if err = updateRootFSDigestInManifest(newService.ImagePath, rootFSDigest); err != nil {
		return aoserrors.Wrap(err)
	}

	newService.ManifestDigest, err = getManifestChecksum(newService.ImagePath)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if isServiceExist {
		if err = sm.removeObsoleteServiceVersions(serviceFromStorage); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	if err = sm.serviceInfoProvider.AddService(newService); err != nil {
		return aoserrors.Wrap(err)
	}

	log.WithFields(log.Fields{
		"id":         newService.ServiceID,
		"aosVersion": newService.AosVersion,
		"imagePath":  newService.ImagePath,
	}).Info("Service successfully installed")

	return nil
}

func (sm *ServiceManager) doApplyService(service ServiceInfo) (err error) {
	oldServices, err := sm.serviceInfoProvider.GetAllServiceVersions(service.ServiceID)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	for _, oldService := range oldServices {
		if oldService.AosVersion == service.AosVersion {
			continue
		}

		if err = sm.removeService(oldService); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	if err = sm.serviceInfoProvider.ActivateService(service.ServiceID, service.AosVersion); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (sm *ServiceManager) doRevertService(service ServiceInfo) error {
	return sm.removeService(service)
}

func (sm *ServiceManager) doRemoveService(serviceID string) error {
	log.WithFields(log.Fields{"serviceID": serviceID}).Debug("Remove service")

	services, err := sm.serviceInfoProvider.GetAllServiceVersions(serviceID)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	var serviceSize uint64

	for _, service := range services {
		serviceSize += service.Size
	}

	if serviceSize > 0 {
		if err = sm.setServiceCached(ServiceInfo{
			ServiceID: serviceID,
			Timestamp: services[len(services)-1].Timestamp,
			Size:      serviceSize,
		}, true); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	return nil
}

func (sm *ServiceManager) validateTTLs() {
	removeTicker := time.NewTicker(RemoveCachedServicesPeriod)
	defer removeTicker.Stop()

	for {
		select {
		case <-removeTicker.C:
			if err := sm.removeOutdatedServices(); err != nil {
				log.Errorf("Can't remove outdated services: %v", err)
			}

		case <-sm.validateTTLStopChannel:
			return
		}
	}
}

func (sm *ServiceManager) removeOutdatedServices() error {
	services, err := sm.serviceInfoProvider.GetCachedServices()
	if err != nil {
		return aoserrors.Wrap(err)
	}

	for _, service := range services {
		if service.Timestamp.Add(time.Hour * 24 * time.Duration(sm.serviceTTLDays)).Before(time.Now()) {
			if err := <-sm.actionHandler.Execute(service.ServiceID,
				func(id string) error {
					return sm.removeService(service)
				}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (sm *ServiceManager) removeOutdatedService(serviceID string) error {
	services, err := sm.serviceInfoProvider.GetAllServiceVersions(serviceID)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	for _, service := range services {
		if err := os.RemoveAll(service.ImagePath); err != nil {
			return aoserrors.Wrap(err)
		}

		if err := sm.serviceInfoProvider.RemoveService(service); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	return nil
}

func (sm *ServiceManager) removeObsoleteServiceVersions(service ServiceInfo) error {
	services, err := sm.serviceInfoProvider.GetAllServiceVersions(service.ServiceID)
	if err != nil && !errors.Is(err, ErrNotExist) {
		return aoserrors.Wrap(err)
	}

	for _, storageService := range services {
		if service.AosVersion != storageService.AosVersion {
			if err = sm.removeService(service); err != nil {
				return err
			}
		}
	}

	if service.Cached {
		if err = sm.setServiceCached(service, false); err != nil {
			return aoserrors.Wrap(err)
		}
	}

	return nil
}

func (sm *ServiceManager) setServiceCached(service ServiceInfo, cached bool) error {
	if err := sm.serviceInfoProvider.SetServiceCached(service.ServiceID, cached); err != nil {
		return aoserrors.Wrap(err)
	}

	if cached {
		if err := sm.serviceAllocator.AddOutdatedItem(
			service.ServiceID, service.Size, service.Timestamp); err != nil {
			return aoserrors.Wrap(err)
		}

		return nil
	}

	sm.serviceAllocator.RestoreOutdatedItem(service.ServiceID)

	return nil
}

func (sm *ServiceManager) removeService(service ServiceInfo) error {
	if err := os.RemoveAll(service.ImagePath); err != nil {
		return aoserrors.Wrap(err)
	}

	if service.Cached {
		sm.serviceAllocator.RestoreOutdatedItem(service.ServiceID)
	}

	sm.serviceAllocator.FreeSpace(service.Size)

	if err := sm.serviceInfoProvider.RemoveService(service); err != nil {
		return aoserrors.Wrap(err)
	}

	log.WithFields(log.Fields{"serviceID": service.ServiceID}).Info("Service successfully removed")

	return nil
}

func (sm *ServiceManager) prepareServiceFS(
	imagePath string, gid int,
) (serviceSize int64, space spaceallocator.Space, rootFSDigest digest.Digest, err error) {
	imageParts, err := getImageParts(imagePath)
	if err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	if serviceSize, err = image.GetUncompressedTarContentSize(imageParts.ServiceFSPath); err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	if space, err = sm.serviceAllocator.AllocateSpace(uint64(serviceSize)); err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	originRootFSPath := imageParts.ServiceFSPath

	tmpRootFS := filepath.Join(imagePath, tmpRootFSDir)

	// unpack rootfs layer
	if err = image.UnpackTarImage(imageParts.ServiceFSPath, tmpRootFS); err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	serviceFSArchiveSize, err := getFileSize(imageParts.ServiceFSPath)
	if err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	sm.serviceAllocator.FreeSpace(serviceFSArchiveSize)

	if err = os.RemoveAll(imageParts.ServiceFSPath); err != nil {
		log.Errorf("Can't remove temp file: %s", err)
	}

	if err = filepath.Walk(tmpRootFS, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return aoserrors.Wrap(err)
		}

		if err = os.Chown(name, 0, gid); err != nil {
			return aoserrors.Wrap(err)
		}

		return nil
	}); err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	if err := whiteouts.OCIWhiteoutsToOverlay(tmpRootFS, 0, gid); err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	rootFSHash, err := dirhash.HashDir(tmpRootFS, tmpRootFS, dirDigest)
	if err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	if rootFSDigest, err = digest.Parse(rootFSHash); err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	if err = os.Rename(tmpRootFS, filepath.Join(path.Dir(originRootFSPath), rootFSDigest.Hex())); err != nil {
		return 0, nil, "", aoserrors.Wrap(err)
	}

	return serviceSize, space, rootFSDigest, nil
}

func (sm *ServiceManager) removeDamagedServiceFolders(services []ServiceInfo) error {
	for _, service := range services {
		fi, err := os.Stat(service.ImagePath)
		if err != nil || !fi.Mode().IsDir() {
			log.Warnf("Service missing: %v", service.ImagePath)

			if err = sm.serviceInfoProvider.RemoveService(service); err != nil {
				return aoserrors.Wrap(err)
			}
		}
	}

	files, err := ioutil.ReadDir(sm.servicesDir)
	if err != nil {
		return aoserrors.Wrap(err)
	}

filesLoop:
	for _, file := range files {
		fullPath := filepath.Join(sm.servicesDir, file.Name())

		for _, service := range services {
			if fullPath == service.ImagePath {
				continue filesLoop
			}
		}

		log.Warnf("Service missing in storage: %v", fullPath)

		if err = os.RemoveAll(fullPath); err != nil {
			log.Errorf("Can't remove service directory: %v", err)
		}
	}

	return nil
}

func (sm *ServiceManager) extractPackageByURL(
	packageURL string, fileInfo image.FileInfo,
) (imagePath string, serviceSize uint64, space spaceallocator.Space, err error) {
	urlVal, err := url.Parse(packageURL)
	if err != nil {
		return "", 0, nil, aoserrors.Wrap(err)
	}

	var sourceFile string

	if urlVal.Scheme != "file" {
		space, err := sm.downloadAllocator.AllocateSpace(fileInfo.Size)
		if err != nil {
			return "", 0, nil, aoserrors.Wrap(err)
		}

		defer func() {
			if err := space.Release(); err != nil {
				log.Errorf("Can't release memory: %v", err)
			}
		}()

		if sourceFile, err = image.Download(context.Background(), sm.downloadDir, packageURL); err != nil {
			return "", 0, nil, aoserrors.Wrap(err)
		}

		defer os.RemoveAll(sourceFile)
	} else {
		sourceFile = urlVal.Path
	}

	if err = image.CheckFileInfo(context.Background(), sourceFile, fileInfo); err != nil {
		return "", 0, nil, aoserrors.Wrap(err)
	}

	size, err := image.GetUncompressedTarContentSize(sourceFile)
	if err != nil {
		return "", 0, nil, aoserrors.Wrap(err)
	}

	if space, err = sm.serviceAllocator.AllocateSpace(uint64(size)); err != nil {
		return "", 0, nil, aoserrors.Wrap(err)
	}

	imagePath, err = ioutil.TempDir(sm.servicesDir, "")
	if err != nil {
		return "", 0, nil, aoserrors.Wrap(err)
	}

	if err = image.UnpackTarImage(sourceFile, imagePath); err != nil {
		return "", 0, nil, aoserrors.Wrap(err)
	}

	return imagePath, uint64(size), space, nil
}

func (sm *ServiceManager) getServiceSize(serviceID string) (size uint64, err error) {
	services, err := sm.serviceInfoProvider.GetAllServiceVersions(serviceID)
	if err != nil {
		return 0, aoserrors.Wrap(err)
	}

	for _, service := range services {
		size += service.Size
	}

	return size, nil
}

func acceptAllocatedSpace(spaceService spaceallocator.Space, spacePackage spaceallocator.Space) {
	if err := spacePackage.Accept(); err != nil {
		log.Errorf("Can't accept memory: %v", err)
	}

	if err := spaceService.Accept(); err != nil {
		log.Errorf("Can't accept memory: %v", err)
	}
}

func releaseAllocatedSpace(
	imagePath string, spaceService spaceallocator.Space, spacePackage spaceallocator.Space,
) {
	if err := os.RemoveAll(imagePath); err != nil {
		log.Errorf("Can't remove service image: %v", err)
	}

	if err := spacePackage.Release(); err != nil {
		log.Errorf("Can't release memory: %v", err)
	}

	if spaceService != nil {
		if err := spaceService.Release(); err != nil {
			log.Errorf("Can't release memory: %v", err)
		}
	}
}

func getFileSize(fileName string) (size uint64, err error) {
	var stat syscall.Stat_t

	if err = syscall.Stat(fileName, &stat); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, aoserrors.Wrap(err)
	}

	return uint64(stat.Size), nil
}
