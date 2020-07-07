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

// Package launcher provides set of API to controls services lifecycle
package launcher

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/docker/docker/pkg/stringid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	amqp "aos_servicemanager/amqphandler"
	"aos_servicemanager/config"
	"aos_servicemanager/monitoring"
	"aos_servicemanager/networkmanager"
	"aos_servicemanager/platform"
	"aos_servicemanager/resourcemanager"
	"aos_servicemanager/utils"
)

/*******************************************************************************
 * Consts
 ******************************************************************************/

// Service status
const (
	statusOk = iota
	statusError
)

// Service state
const (
	stateInit = iota
	stateRunning
	stateStopped
)

const (
	serviceDir = "services" // services directory

	runcName         = "runc"         // runc file name
	wonderShaperName = "wondershaper" // wondershaper name

	ocConfigFile = "config.json"
)

const (
	stateChannelSize = 32
)

const serviceTemplate = `# This is template file used to launch AOS services
# Known variables:
# * ${ID}            - service id
# * ${SERVICEPATH}   - path to service dir
# * ${RUNC}          - path to runc
[Unit]
Description=AOS Service
After=network.target

[Service]
Type=forking
Restart=always
RestartSec=1
ExecStartPre=${RUNC} delete -f ${ID}
ExecStart=${RUNC} run -d --pid-file ${SERVICEPATH}/.pid -b ${SERVICEPATH} ${ID}

ExecStop=${RUNC} kill ${ID} SIGKILL
ExecStopPost=${RUNC} delete -f ${ID}
PIDFile=${SERVICEPATH}/.pid
SuccessExitStatus=SIGKILL

[Install]
WantedBy=multi-user.target
`

const serviceTemplateFile = "template.service"

const (
	hostfsWiteoutsDir = "hostfs/whiteouts"
)

const (
	serviceMergedDir      = "merged"
	serviceRootfsDir      = "rootfs"
	serviceMountPointsDir = "mounts"
)

const defaultServiceProvider = "default"

/*******************************************************************************
 * Vars
 ******************************************************************************/

var defaultHostfsBinds = []string{"bin", "sbin", "lib", "lib64", "usr"}

/*******************************************************************************
 * Types
 ******************************************************************************/

// Launcher instance
type Launcher struct {
	// NewStateChannel used to notify about new service state
	NewStateChannel chan NewState

	crypt           utils.FcryptInterface
	sender          Sender
	serviceProvider ServiceProvider
	monitor         ServiceMonitor
	network         NetworkProvider
	devicemanager   DeviceManagement
	systemd         *dbus.Conn
	config          *config.Config
	layerProvider   layerProvider

	actionHandler  *actionHandler
	storageHandler *storageHandler

	downloader downloader

	users []string

	services sync.Map

	serviceTemplate  string
	runcPath         string
	wonderShaperPath string
}

// Service describes service structure
type Service struct {
	ID              string        // service id
	Version         uint64        // service version
	ServiceProvider string        // service provider
	Path            string        // path to service bundle
	UnitName        string        // systemd unit name
	UserName        string        // user used to run this service
	HostName        string        // service host name
	Permissions     string        // VIS permissions
	State           ServiceState  // service state
	Status          ServiceStatus // service status
	StartAt         time.Time     // time at which service was started
	TTL             uint64        // expiration service duration in days
	AlertRules      string        // alert rules in json format
	UploadSpeed     uint64        // upload traffic speed
	DownloadSpeed   uint64        // download traffic speed
	UploadLimit     uint64        // upload traffic limit
	DownloadLimit   uint64        // download traffic limit
	StorageLimit    uint64        // storage limit
	StateLimit      uint64        // state limit
	Layers          []string      // list layers dir
	Devices         string        // device resources in json format
}

// UsersService describes users service structure
type UsersService struct {
	Users         []string // user claims
	ServiceID     string   // service id
	StorageFolder string   // service storage folder
	StateChecksum []byte   // service state checksum
}

// ServiceProvider provides API to create, remove or access services DB
type ServiceProvider interface {
	AddService(service Service) (err error)
	UpdateService(service Service) (err error)
	RemoveService(serviceID string) (err error)
	GetService(serviceID string) (service Service, err error)
	GetServices() (services []Service, err error)
	GetServiceProviderServices(serviceProvider string) (services []Service, err error)
	GetServiceByUnitName(unitName string) (service Service, err error)
	SetServiceStatus(serviceID string, status ServiceStatus) (err error)
	SetServiceState(serviceID string, state ServiceState) (err error)
	SetServiceStartTime(serviceID string, time time.Time) (err error)
	AddServiceToUsers(users []string, serviceID string) (err error)
	RemoveServiceFromUsers(users []string, serviceID string) (err error)
	GetUsersServices(users []string) (services []Service, err error)
	RemoveServiceFromAllUsers(serviceID string) (err error)
	GetUsersService(users []string, serviceID string) (userService UsersService, err error)
	GetUsersServicesByServiceID(serviceID string) (userServices []UsersService, err error)
	SetUsersStorageFolder(users []string, serviceID string, storageFolder string) (err error)
	SetUsersStateChecksum(users []string, serviceID string, checksum []byte) (err error)
}

// ServiceMonitor provides API to start/stop service monitoring
type ServiceMonitor interface {
	StartMonitorService(serviceID string, monitoringConfig monitoring.ServiceMonitoringConfig) (err error)
	StopMonitorService(serviceID string) (err error)
}

// Sender provides API to send messages to the cloud
type Sender interface {
	SendServiceStatus(serviceStatus amqp.ServiceInfo) (err error)
	SendStateRequest(serviceID string, defaultState bool) (err error)
}

// NetworkProvider provides network interface
type NetworkProvider interface {
	GetID() (id string)
	CreateNetwork(spID string) (err error)
	NetworkExists(spID string) (err error)
	DeleteNetwork(spID string) (err error)
	AddServiceToNetwork(serviceID, spID, servicePath string, params networkmanager.NetworkParams) (err error)
	RemoveServiceFromNetwork(serviceID, spID string) (err error)
	GetServiceIP(serviceID, spID string) (ip string, err error)
}

// DeviceManagement provides API to validate, request and release devices
type DeviceManagement interface {
	AreResourcesValid() (err error)
	RequestDeviceResourceByName(name string) (deviceResource resourcemanager.DeviceResource, err error)
	RequestDevice(device string, serviceID string) (err error)
	ReleaseDevice(device string, serviceID string) (err error)
}

// NewState new state message
type NewState struct {
	CorrelationID string
	ServiceID     string
	State         string
	Checksum      string
}

// ServiceStatus service status
type ServiceStatus int

// ServiceState service state
type ServiceState int

type actionType int

type downloader interface {
	downloadService(serviceInfo serviceInfoToInstall, crypt utils.FcryptInterface) (outputFile string, err error)
}

type stateAcceptance struct {
	correlationID string
	acceptance    amqp.StateAcceptance
}

type layerProvider interface {
	GetLayerPathByDigest(layerDigest string) (layerPath string, err error)
	DeleteUnneededLayers() (err error)
}

type serviceInfoToInstall struct {
	serviceDetails amqp.ServiceInfoFromCloud
	chains         []amqp.CertificateChain
	certs          []amqp.Certificate
}

/*******************************************************************************
 * Public
 ******************************************************************************/

// New creates new launcher object
func New(config *config.Config, crypt utils.FcryptInterface, sender Sender, serviceProvider ServiceProvider,
	layerProvider layerProvider, monitor ServiceMonitor, network NetworkProvider, devicemanager DeviceManagement) (launcher *Launcher, err error) {
	log.Debug("New launcher")

	launcher = &Launcher{
		config:          config,
		crypt:           crypt,
		sender:          sender,
		serviceProvider: serviceProvider,
		layerProvider:   layerProvider,
		monitor:         monitor,
		network:         network,
		devicemanager:   devicemanager,
	}

	launcher.NewStateChannel = make(chan NewState, stateChannelSize)

	if launcher.actionHandler, err = newActionHandler(); err != nil {
		return nil, err
	}

	if launcher.storageHandler, err = newStorageHandler(config.StorageDir, serviceProvider,
		launcher.NewStateChannel, sender); err != nil {
		return nil, err
	}

	launcher.downloader = &imageHandler{}

	// Check and create service dir
	dir := path.Join(config.WorkingDir, serviceDir)
	if _, err = os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if err = os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}

	// Create systemd connection
	launcher.systemd, err = dbus.NewSystemConnection()
	if err != nil {
		return nil, err
	}

	// Get systemd service template
	launcher.serviceTemplate, err = getSystemdServiceTemplate(config.WorkingDir)
	if err != nil {
		return nil, err
	}

	// Retrieve runc abs path
	launcher.runcPath, err = exec.LookPath(runcName)
	if err != nil {
		return nil, err
	}

	// Retrieve wondershaper abs path
	launcher.wonderShaperPath, _ = filepath.Abs(path.Join(config.WorkingDir, wonderShaperName))
	if _, err := os.Stat(launcher.wonderShaperPath); err != nil {
		// check system PATH
		launcher.wonderShaperPath, err = exec.LookPath(wonderShaperName)
		if err != nil {
			return nil, err
		}
	}

	if err = launcher.prepareHostfsDir(); err != nil {
		return nil, err
	}

	return launcher, nil
}

// Close closes launcher
func (launcher *Launcher) Close() {
	log.Debug("Close launcher")

	launcher.stopServices()

	launcher.systemd.Close()

	launcher.storageHandler.Close()
}

// GetServiceVersion returns installed version of requested service
func (launcher *Launcher) GetServiceVersion(id string) (version uint64, err error) {
	log.WithField("id", id).Debug("Get service version")

	service, err := launcher.serviceProvider.GetService(id)
	if err != nil {
		return version, err
	}

	version = service.Version

	return version, nil
}

// InstallService installs and runs service
func (launcher *Launcher) InstallService(serviceInfo amqp.ServiceInfoFromCloud,
	chains []amqp.CertificateChain, certs []amqp.Certificate) {
	serviceInfoForInstall := serviceInfoToInstall{
		serviceDetails: serviceInfo,
		chains:         chains,
		certs:          certs,
	}

	launcher.actionHandler.PutInQueue(serviceAction{serviceInfo.ID, serviceInfoForInstall, launcher.doActionInstall})
}

// UninstallService stops and removes service
func (launcher *Launcher) UninstallService(id string) {
	launcher.actionHandler.PutInQueue(serviceAction{id, nil, launcher.doActionUninstall})
}

// FinishProcessingLayers triggers layers cleanup
func (launcher *Launcher) FinishProcessingLayers() {
	launcher.actionHandler.PutInQueue(serviceAction{"", nil, launcher.doFinishProcessingLayers})
}

// GetServicesInfo returns information about all installed services
func (launcher *Launcher) GetServicesInfo() (info []amqp.ServiceInfo, err error) {
	log.Debug("Get services info")

	services, err := launcher.serviceProvider.GetUsersServices(launcher.users)
	if err != nil {
		return info, err
	}

	info = make([]amqp.ServiceInfo, len(services))

	for i, service := range services {
		info[i] = amqp.ServiceInfo{ID: service.ID, Version: service.Version, Status: service.Status.String()}

		userService, err := launcher.serviceProvider.GetUsersService(launcher.users, service.ID)
		if err != nil {
			return info, err
		}

		if service.StateLimit != 0 {
			info[i].StateChecksum = hex.EncodeToString(userService.StateChecksum)
		}
	}

	return info, nil
}

// SetUsers sets users for services
func (launcher *Launcher) SetUsers(users []string) (err error) {
	log.WithFields(log.Fields{"new": users, "old": launcher.users}).Debug("Set users")

	if isUsersEqual(launcher.users, users) {
		return nil
	}

	launcher.stopServices()

	launcher.users = users

	// run stored service configuration only in case system resources are valid
	// check available devices with system ones
	if err = launcher.devicemanager.AreResourcesValid(); err != nil {
		log.Errorf("Validation resources: %s", err)

		//Do not fail here, just print message
		return nil
	}

	launcher.startServices()

	if err = launcher.cleanServicesDB(); err != nil {
		log.Errorf("Error cleaning DB: %s", err)
	}

	return nil
}

// StateAcceptance notifies launcher about new state acceptance
func (launcher *Launcher) StateAcceptance(acceptance amqp.StateAcceptance, correlationID string) {
	launcher.actionHandler.PutInQueue(serviceAction{acceptance.ServiceID,
		stateAcceptance{correlationID, acceptance}, launcher.doStateAcceptance})
}

// UpdateState updates service state
func (launcher *Launcher) UpdateState(state amqp.UpdateState) {
	launcher.actionHandler.PutInQueue(serviceAction{state.ServiceID, state, launcher.doUpdateState})
}

// Cleanup deletes all AOS services, their storages and states
func Cleanup(cfg *config.Config) (err error) {
	systemd, err := dbus.NewSystemConnection()
	if err != nil {
		log.Errorf("Can't connect to systemd: %s", err)
	}

	if systemd != nil {
		unitFiles, err := systemd.ListUnitFiles()
		if err != nil {
			log.Errorf("Can't list systemd units: %s", err)
		} else {
			for _, unitFile := range unitFiles {
				serviceName := filepath.Base(unitFile.Path)

				if !strings.HasPrefix(serviceName, "aos_") {
					continue
				}

				desc, err := systemd.GetUnitProperty(serviceName, "Description")
				if err != nil {
					log.WithField("name", serviceName).Errorf("Can't get unit property: %s", err)
					continue
				}

				value, ok := desc.Value.Value().(string)
				if !ok {
					log.WithField("name", serviceName).Error("Can't convert description")
					continue
				}

				if value == "AOS Service" {
					log.WithField("name", serviceName).Debug("Deleting systemd service")

					channel := make(chan string)
					if _, err := systemd.StopUnit(serviceName, "replace", channel); err != nil {
						log.WithField("name", serviceName).Errorf("Can't stop unit: %s", err)
					} else {
						<-channel
					}

					if _, err := systemd.DisableUnitFiles([]string{serviceName}, false); err != nil {
						log.WithField("name", serviceName).Error("Can't disable unit: ", err)
					}

					// Delete service user
					serviceID := strings.TrimSuffix(strings.TrimPrefix(serviceName, "aos_"), ".service")

					if err := platform.DeleteUser(serviceID); err != nil {
						log.WithField("serviceID", serviceID).Error("Can't delete user: ", err)
					}
				}
			}
		}

		if err := systemd.Reload(); err != nil {
			log.Errorf("Can't reload systemd: %s", err)
		}
	}

	serviceDir := path.Join(cfg.WorkingDir, serviceDir)

	log.WithField("dir", serviceDir).Debug("Remove service dir")

	if err := os.RemoveAll(serviceDir); err != nil {
		log.Fatalf("Can't remove service folder: %s", err)
	}

	log.WithField("dir", cfg.StorageDir).Debug("Remove storage dir")

	if err := os.RemoveAll(cfg.StorageDir); err != nil {
		log.Fatalf("Can't remove storage folder: %s", err)
	}

	log.WithField("dir", cfg.UpgradeDir).Debug("Remove upgrade dir")

	if err := os.RemoveAll(cfg.UpgradeDir); err != nil {
		log.Fatalf("Can't remove upgrade folder: %s", err)
	}

	return nil
}

func (state ServiceState) String() string {
	return [...]string{"Init", "Running", "Stopped"}[state]
}

func (status ServiceStatus) String() string {
	return [...]string{"OK", "Error"}[status]
}

/*******************************************************************************
 * Private
 ******************************************************************************/

func (launcher *Launcher) prepareHostfsDir() (err error) {
	witeoutsDir := path.Join(launcher.config.WorkingDir, hostfsWiteoutsDir)

	if err = os.MkdirAll(witeoutsDir, 0755); err != nil {
		return err
	}

	allowedDirs := defaultHostfsBinds

	if len(launcher.config.HostBinds) > 0 {
		allowedDirs = launcher.config.HostBinds
	}

	rootContent, err := ioutil.ReadDir("/")
	if err != nil {
		return err
	}

	for _, item := range rootContent {
		itemPath := path.Join(witeoutsDir, item.Name())

		if _, err = os.Stat(itemPath); err == nil {
			// skip already exists items
			continue
		}

		if !os.IsNotExist(err) {
			return err
		}

		allowed := false

		for _, allowedItem := range allowedDirs {
			if item.Name() == allowedItem {
				allowed = true

				break
			}
		}

		if allowed {
			continue
		}

		// Create whiteout for not allowed items
		if err = syscall.Mknod(itemPath, syscall.S_IFCHR, int(unix.Mkdev(0, 0))); err != nil {
			return err
		}
	}

	return nil
}

func isUsersEqual(users1, users2 []string) (result bool) {
	if users1 == nil && users2 == nil {
		return true
	}

	if users1 == nil || users2 == nil {
		return false
	}

	if len(users1) != len(users2) {
		return false
	}

	for i := range users1 {
		if users1[i] != users2[i] {
			return false
		}
	}

	return true
}

func (launcher *Launcher) doActionInstall(id string, data interface{}) {
	var err error

	status := amqp.ServiceInfo{ID: id, Status: "installed"}

	defer func() {
		if err != nil {
			status.Status = "error"
			status.Error = "Can't install service: " + err.Error()
			if launcher.sender != nil {
				launcher.sender.SendServiceStatus(status)
			}
		}
	}()

	serviceInfo, ok := data.(serviceInfoToInstall)
	if !ok {
		err = errors.New("wrong data type")
		return
	}

	status.Version = serviceInfo.serviceDetails.Version

	if err = launcher.installService(serviceInfo); err != nil {
		return
	}

	var userService UsersService

	if userService, err = launcher.serviceProvider.GetUsersService(launcher.users, id); err != nil {
		return
	}

	status.StateChecksum = hex.EncodeToString(userService.StateChecksum)

	if launcher.sender != nil {
		launcher.sender.SendServiceStatus(status)
	}
}

func (launcher *Launcher) doActionUninstall(id string, data interface{}) {
	var err error

	status := amqp.ServiceInfo{ID: id, Status: "removed"}
	status.Version, err = launcher.uninstallService(status.ID)
	if err != nil {
		status.Status = "error"
		status.Error = err.Error()
	}

	if launcher.sender != nil {
		launcher.sender.SendServiceStatus(status)
	}
}

func (launcher *Launcher) doFinishProcessingLayers(id string, data interface{}) {
	if err := launcher.layerProvider.DeleteUnneededLayers(); err != nil {
		log.Error("DeleteUnneededLayers error: ", err)
	}
}

func (launcher *Launcher) installService(serviceInfo serviceInfoToInstall) (err error) {
	if launcher.users == nil {
		return errors.New("users are not set")
	}

	// Install service only in case system resources are valid
	// check available devices with system ones
	if err = launcher.devicemanager.AreResourcesValid(); err != nil {
		log.Errorf("Validation resources: %s", err)

		return err
	}

	service, err := launcher.serviceProvider.GetService(serviceInfo.serviceDetails.ID)
	if err != nil && !strings.Contains(err.Error(), "not exist") {
		return err
	}
	serviceExists := err == nil

	// Skip incorrect version
	if serviceExists && serviceInfo.serviceDetails.Version < service.Version {
		return errors.New("version mistmatch")
	}

	// If same service version exists, just start the service
	if serviceExists && serviceInfo.serviceDetails.Version == service.Version {
		if err = launcher.addServiceToCurrentUsers(serviceInfo.serviceDetails.ID); err != nil {
			return err
		}

		if err = launcher.startService(service); err != nil {
			return err
		}

		return nil
	}

	unpackDir, err := ioutil.TempDir("", "aos_")
	defer os.RemoveAll(unpackDir)

	// download and unpack
	if err = downloadAndUnpackImage(launcher.downloader, serviceInfo, unpackDir, launcher.crypt); err != nil {
		return err
	}

	if err = validateUnpackedImage(unpackDir); err != nil {
		return err
	}

	// We need to install or update the service

	// create install dir
	installDir, err := ioutil.TempDir(path.Join(launcher.config.WorkingDir, serviceDir), "")
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			log.WithField("serviceID", serviceInfo.serviceDetails.ID).Errorf("Error install service: %s", err)

			if !serviceExists {
				// Remove system user
				if platform.IsUserExist(serviceInfo.serviceDetails.ID) {
					if err := platform.DeleteUser(serviceInfo.serviceDetails.ID); err != nil {
						log.WithField("serviceID", serviceInfo.serviceDetails.ID).Errorf("Can't delete user: %s", err)
					}
				}
			}

			// Remove install dir if exists
			if _, err := os.Stat(installDir); err == nil {
				if err := os.RemoveAll(installDir); err != nil {
					log.WithField("serviceID", serviceInfo.serviceDetails.ID).Errorf("Can't remove service dir: %s", err)
				}
			}
		}
	}()

	log.WithFields(log.Fields{"dir": installDir, "serviceID": serviceInfo.serviceDetails.ID}).Debug("Create install dir")

	var newService Service
	if newService, err = launcher.prepareService(unpackDir, installDir, serviceInfo.serviceDetails); err != nil {
		return err
	}

	if !serviceExists {
		if err = launcher.addService(newService); err != nil {
			return err
		}
	} else {
		if err = launcher.updateService(service, newService); err != nil {
			return err
		}
	}

	return nil
}

func (launcher *Launcher) uninstallService(id string) (version uint64, err error) {
	service, err := launcher.serviceProvider.GetService(id)
	if err != nil {
		return 0, err
	}

	version = service.Version

	if launcher.users == nil {
		return version, errors.New("users are not set")
	}

	if err := launcher.stopService(service); err != nil {
		return version, err
	}

	userService, err := launcher.serviceProvider.GetUsersService(launcher.users, service.ID)
	if err != nil {
		return version, err
	}

	if userService.StorageFolder != "" {
		log.WithFields(log.Fields{
			"folder":    userService.StorageFolder,
			"serviceID": service.ID}).Debug("Remove storage folder")

		if err = os.RemoveAll(userService.StorageFolder); err != nil {
			return version, err
		}
	}

	if err = launcher.serviceProvider.RemoveServiceFromUsers(launcher.users, service.ID); err != nil {
		return version, err
	}

	return version, nil
}

func (launcher *Launcher) doUpdateState(id string, data interface{}) {
	service, err := launcher.serviceProvider.GetService(id)
	if err != nil {
		log.Errorf("Can't get service: %s", err)
		return
	}

	if err = launcher.stopService(service); err != nil {
		log.Errorf("Can't stop service: %s", err)
		return
	}

	state, ok := data.(amqp.UpdateState)
	if !ok {
		log.Error("Wrong data type")
		return
	}

	if err = launcher.storageHandler.UpdateState(launcher.users, service, state.State, state.Checksum); err != nil {
		log.Errorf("Can't update state: %s", err)
		return
	}

	if err = launcher.startService(service); err != nil {
		log.Errorf("Can't start service: %s", err)
		return
	}
}

func (launcher *Launcher) doStateAcceptance(id string, data interface{}) {
	stateAcceptance, ok := data.(stateAcceptance)
	if !ok {
		log.Error("Wrong data type")
		return
	}

	if err := launcher.storageHandler.StateAcceptance(stateAcceptance.acceptance, stateAcceptance.correlationID); err != nil {
		log.Errorf("Can't accept state: %s", err)
		return
	}
}

func (launcher *Launcher) updateServiceState(id string, state ServiceState, status ServiceStatus) (err error) {
	service, err := launcher.serviceProvider.GetService(id)
	if err != nil {
		return err
	}

	if launcher.monitor != nil && !reflect.ValueOf(launcher.monitor).IsNil() {
		if err = launcher.updateMonitoring(service, state); err != nil {
			log.WithField("id", id).Error("Can't update monitoring: ", err)
		}
	}

	if service.State != state {
		log.WithField("id", id).Debugf("Set service state: %s", state)

		if err = launcher.serviceProvider.SetServiceState(id, state); err != nil {
			return err
		}
	}

	if service.Status != status {
		log.WithField("id", id).Debugf("Set service status: %s", status)

		if err = launcher.serviceProvider.SetServiceStatus(id, status); err != nil {
			return err
		}
	}

	return nil
}

func (launcher *Launcher) mountLayers(service Service) (err error) {
	mergedDir := path.Join(service.Path, serviceMergedDir)

	// create merged dir
	if err = os.MkdirAll(mergedDir, 0755); err != nil {
		return err
	}

	log.WithFields(log.Fields{"path": mergedDir, "id": service.ID}).Debug("Mount service layers")

	layerDirs := []string{path.Join(service.Path, serviceMountPointsDir), path.Join(service.Path, serviceRootfsDir)}
	layerDirs = append(layerDirs, service.Layers...)
	layerDirs = append(layerDirs, path.Join(launcher.config.WorkingDir, hostfsWiteoutsDir))
	layerDirs = append(layerDirs, string("/"))

	if err = overlayMount(mergedDir, layerDirs, "", ""); err != nil {
		return err
	}

	return nil
}

func (launcher *Launcher) umountLayers(service Service) (err error) {
	mergedDir := path.Join(service.Path, serviceMergedDir)

	log.WithFields(log.Fields{"path": mergedDir, "id": service.ID}).Debug("Unmount service layers")

	if err = umountWithRetry(mergedDir, 0); err != nil {
		return err
	}

	return nil
}

func (launcher *Launcher) updateStorageFolder(spec *serviceSpec, service Service) (err error) {
	storageFolder, err := launcher.storageHandler.PrepareStorageFolder(launcher.users, service)
	if err != nil {
		return err
	}

	if storageFolder != "" {
		if err = spec.addBindMount(storageFolder, serviceStorageFolder, "rw"); err != nil {
			return err
		}

		if err = os.MkdirAll(path.Join(service.Path, serviceMountPointsDir, serviceStorageFolder), 0755); err != nil {
			return err
		}
	} else {
		spec.removeBindMount(serviceStorageFolder)
		os.RemoveAll(path.Join(service.Path, serviceMountPointsDir, serviceStorageFolder))
	}

	return nil
}

func (launcher *Launcher) updateNetwork(spec *serviceSpec, service Service) (err error) {
	networkFiles := []string{"/etc/hosts", "/etc/resolv.conf"}

	if launcher.network == nil {
		for _, networkFile := range networkFiles {
			spec.removeBindMount(networkFile)
			os.RemoveAll(path.Join(service.Path, serviceMountPointsDir, networkFile))
		}

		return nil
	}

	if launcher.network != nil {
		params := networkmanager.NetworkParams{Hostname: service.HostName}

		if params.Hostname != "" {
			params.Aliases = append(params.Aliases, params.Hostname)
		}

		if err = launcher.network.AddServiceToNetwork(service.ID, service.ServiceProvider, service.Path, params); err != nil {
			return err
		}

		for _, networkFile := range networkFiles {
			if err = spec.addBindMount(path.Join(service.Path, networkFile), networkFile, "ro"); err != nil {
				return err
			}

			file, err := os.OpenFile(path.Join(service.Path, serviceMountPointsDir, networkFile), os.O_CREATE, 0644)
			if err != nil {
				return err
			}
			file.Close()
		}

		if err = spec.createPrestartHook(path.Join("/proc", strconv.Itoa(os.Getpid()), "exe"), []string{
			"libnetwork-setkey",
			"-exec-root=/run/aos",
			service.ID,
			stringid.TruncateID(launcher.network.GetID())}); err != nil {
			return err
		}

		// TODO: set traffic speed

	}

	return nil
}

func (launcher *Launcher) prestartService(service Service) (err error) {
	spec, err := loadServiceSpec(path.Join(service.Path, ocConfigFile))
	if err != nil {
		return err
	}
	defer func() {
		if specErr := spec.save(); specErr != nil {
			if err == nil {
				err = specErr
			}
		}
	}()

	var devices []Device
	if err := json.Unmarshal([]byte(service.Devices), &devices); err != nil {
		return err
	}

	//Update Devices in spec
	_, err = launcher.setDevices(spec, devices)
	if err != nil {
		return err
	}

	if err = launcher.updateStorageFolder(spec, service); err != nil {
		return err
	}

	if err = launcher.updateNetwork(spec, service); err != nil {
		return err
	}

	if err = launcher.mountLayers(service); err != nil {
		return err
	}

	if err = launcher.requestDeviceResources(service); err != nil {
		return err
	}

	return nil
}

func (launcher *Launcher) addServiceToSystemd(service Service) (err error) {
	fileName, err := filepath.Abs(path.Join(service.Path, service.UnitName))
	if err != nil {
		return err
	}

	// Use launcher.systemd.EnableUnitFiles if services should be started automatically
	// on system restart
	if _, err = launcher.systemd.LinkUnitFiles([]string{fileName}, false, true); err != nil {
		return err
	}

	if err = launcher.systemd.Reload(); err != nil {
		return err
	}

	return nil
}

func (launcher *Launcher) requestDeviceResources(service Service) (err error) {
	var devices []Device
	if err := json.Unmarshal([]byte(service.Devices), &devices); err != nil {
		return err
	}

	for _, device := range devices {
		log.Debugf("Request device %s, for %s service", device.Name, service.ID)

		if err = launcher.devicemanager.RequestDevice(device.Name, service.ID); err != nil {
			return err
		}
	}

	return nil
}

func (launcher *Launcher) startService(service Service) (err error) {
	if err = launcher.prestartService(service); err != nil {
		return err
	}

	channel := make(chan string)
	if _, err = launcher.systemd.StartUnit(service.UnitName, "replace", channel); err != nil {
		return err
	}
	status := <-channel

	log.WithFields(log.Fields{"name": service.UnitName, "status": status}).Debug("Start service")

	if err = launcher.updateServiceState(service.ID, stateRunning, statusOk); err != nil {
		log.WithField("id", service.ID).Warnf("Can't update service state: %s", err)
	}

	if err = launcher.serviceProvider.SetServiceStartTime(service.ID, time.Now()); err != nil {
		log.WithField("id", service.ID).Warnf("Can't set service start time: %s", err)
	}

	launcher.services.Store(service.UnitName, service.ID)

	return nil
}

func (launcher *Launcher) startServices() {
	log.WithField("users", launcher.users).Debug("Start user services")

	services, err := launcher.serviceProvider.GetUsersServices(launcher.users)
	if err != nil {
		log.Errorf("Can't start services: %s", err)
	}

	statusChannel := make(chan error, len(services))

	// Start all services in parallel
	for _, service := range services {
		launcher.actionHandler.PutInQueue(serviceAction{service.ID, service,
			func(id string, data interface{}) {
				service, ok := data.(Service)
				if !ok {
					statusChannel <- errors.New("wrong data type")
					return
				}

				statusChannel <- launcher.startService(service)
			}})
	}

	// Wait all services are started
	for i := 0; i < len(services); i++ {
		<-statusChannel
	}
}

func (launcher *Launcher) releaseDeviceResources(service Service) (err error) {
	var devices []Device
	if err := json.Unmarshal([]byte(service.Devices), &devices); err != nil {
		return err
	}

	for _, device := range devices {
		log.Debugf("Release device %s, for %s service", device.Name, service.ID)

		if err = launcher.devicemanager.ReleaseDevice(device.Name, service.ID); err != nil {
			return err
		}
	}

	return nil
}

func (launcher *Launcher) poststopService(service Service) (retErr error) {
	if err := launcher.umountLayers(service); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't umount layers: %s", err)
			retErr = err
		}
	}

	if err := launcher.storageHandler.StopStateWatching(launcher.users, service); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't stop state watching: %s", err)
			retErr = err
		}
	}

	if err := launcher.releaseDeviceResources(service); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't release devices: %s", err)
			retErr = err
		}
	}

	if launcher.network != nil {
		if err := launcher.network.RemoveServiceFromNetwork(
			service.ID, service.ServiceProvider); err != nil && !strings.Contains(err.Error(), "not found") {
			if retErr == nil {
				log.WithField("id", service.ID).Errorf("Can't remove service from network: %s", err)
				retErr = err
			}
		}
	}

	return nil
}

func (launcher *Launcher) stopService(service Service) (retErr error) {
	launcher.services.Delete(service.UnitName)

	channel := make(chan string)
	if _, err := launcher.systemd.StopUnit(service.UnitName, "replace", channel); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't stop systemd unit: %s", err)
			retErr = err
		}
	} else {
		status := <-channel
		log.WithFields(log.Fields{"id": service.ID, "status": status}).Debug("Stop service")
	}

	if err := launcher.poststopService(service); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't perform post stop: %s", err)
			retErr = err
		}
	}

	if err := launcher.updateServiceState(service.ID, stateStopped, statusOk); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't update service state: %s", err)
			retErr = err
		}
	}

	return retErr
}

func (launcher *Launcher) stopServices() {
	log.WithField("users", launcher.users).Debug("Stop user services")

	var services []Service
	var err error

	if launcher.users == nil {
		services, err = launcher.serviceProvider.GetServices()
		if err != nil {
			log.Errorf("Can't stop services: %s", err)
		}
	} else {
		services, err = launcher.serviceProvider.GetUsersServices(launcher.users)
		if err != nil {
			log.Errorf("Can't stop services: %s", err)
		}
	}

	statusChannel := make(chan error, len(services))

	// Stop all services in parallel
	for _, service := range services {
		launcher.actionHandler.PutInQueue(serviceAction{service.ID, service,
			func(id string, data interface{}) {
				service, ok := data.(Service)
				if !ok {
					statusChannel <- errors.New("wrong data type")
					return
				}
				statusChannel <- launcher.stopService(service)
			}})
	}

	// Wait all services are stopped
	for i := 0; i < len(services); i++ {
		<-statusChannel
	}
}

func (launcher *Launcher) restoreService(service Service) (retErr error) {
	log.WithField("id", service.ID).Warn("Restore previous service version")

	if err := launcher.serviceProvider.UpdateService(service); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't update service in DB: %s", err)
			retErr = err
		}
	}

	if err := platform.SetUserFSQuota(launcher.config.StorageDir, service.StorageLimit, service.UserName); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't set user FS quoate: %s", err)
			retErr = err
		}
	}

	if err := launcher.addServiceToSystemd(service); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't add service to systemd: %s", err)
			retErr = err
		}
	}

	if err := launcher.startService(service); err != nil {
		if retErr == nil {
			log.WithField("id", service.ID).Errorf("Can't install service: %s", err)
			retErr = err
		}
	}

	return retErr
}

func (launcher *Launcher) createMountPoints(serviceDir string, spec *serviceSpec) (err error) {
	mountPointsDir := path.Join(serviceDir, serviceMountPointsDir)

	if err = os.MkdirAll(mountPointsDir, 0755); err != nil {
		return err
	}

	for _, mount := range spec.ocSpec.Mounts {

		var permissions uint64

		for _, option := range mount.Options {
			nameValue := strings.Split(strings.TrimSpace(option), "=")

			if len(nameValue) > 1 && nameValue[0] == "mode" {
				if permissions, err = strconv.ParseUint(nameValue[1], 8, 32); err != nil {
					return err
				}
			}
		}

		itemPath := path.Join(mountPointsDir, mount.Destination)

		switch mount.Type {
		case "proc", "tmpfs", "sysfs":
			if err = os.MkdirAll(itemPath, 0755); err != nil {
				return err
			}

			if permissions != 0 {
				if err = os.Chmod(itemPath, os.FileMode(permissions)); err != nil {
					return err
				}
			}

		case "bind":
			stat, err := os.Stat(mount.Source)
			if err != nil {
				return err
			}

			if stat.IsDir() {
				if err = os.MkdirAll(itemPath, 0755); err != nil {
					return err
				}
			} else {
				if err = os.MkdirAll(filepath.Dir(itemPath), 0755); err != nil {
					return err
				}

				file, err := os.OpenFile(itemPath, os.O_CREATE, 0644)
				if err != nil {
					return err
				}
				file.Close()
			}

			if permissions != 0 {
				if err = os.Chmod(itemPath, os.FileMode(permissions)); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (launcher *Launcher) setDevices(spec *serviceSpec, devices []Device) (deviceBytes []byte, err error) {
	// get devices from aos service configuration
	// and get all resource information for device from device manager
	// and add groups and host devices for class device

	// clear spec before adding devices
	if err = spec.clearDeviceData(); err != nil {
		return []byte{}, err
	}

	if err = spec.clearAdditionalGroup(); err != nil {
		return []byte{}, err
	}

	for _, device := range devices {
		deviceResource, err := launcher.devicemanager.RequestDeviceResourceByName(device.Name)
		if err != nil {
			return []byte{}, err
		}

		for _, hostDevice := range deviceResource.HostDevices {
			// use absolute path from host devices and permissions from aos configuration
			if err = spec.addHostDevice(Device{hostDevice, device.Permissions}); err != nil {
				return []byte{}, err
			}
		}

		for _, group := range deviceResource.Groups {
			if err = spec.addAdditionalGroup(group); err != nil {
				return []byte{}, err
			}
		}

	}

	deviceBytes, err = json.Marshal(devices)
	if err != nil {
		return []byte{}, err
	}

	return deviceBytes, nil
}

func (launcher *Launcher) prepareService(unpackDir, installDir string,
	serviceInfo amqp.ServiceInfoFromCloud) (service Service, err error) {
	userName, err := platform.CreateUser(serviceInfo.ID)
	if err != nil {
		return service, err
	}

	imageParts, err := getImageParts(unpackDir)
	if err != nil {
		return service, err
	}

	//unpack rootfs layer
	if err := utils.UnpackTarImage(imageParts.serviceFSLayerPath, path.Join(installDir, serviceRootfsDir)); err != nil {
		return service, err
	}

	// generate config.json
	spec, err := generateSpecFromImageConfig(imageParts.imageConfigPath, path.Join(installDir, ocConfigFile))
	if err != nil {
		return service, err
	}

	defer func() {
		if specErr := spec.save(); specErr != nil {
			if err == nil {
				err = specErr
			}
		}
	}()

	aosConfig, err := getAosServiceConfig(imageParts.aosSrvConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return service, err
	}

	if err == nil {
		if err = spec.applyAosServiceConfig(aosConfig); err != nil {
			return service, err
		}
	}

	if err = spec.bindHostDirs(launcher.config.WorkingDir); err != nil {
		return service, err
	}

	if err = spec.setUser(userName); err != nil {
		return service, err
	}

	deviceResourcesForService, err := launcher.setDevices(spec, aosConfig.Devices)
	if err != nil {
		return service, err
	}

	if err = spec.setRootfs(serviceMergedDir); err != nil {
		return service, err
	}

	if err = launcher.createMountPoints(installDir, spec); err != nil {
		return service, err
	}

	serviceName := "aos_" + serviceInfo.ID + ".service"

	if err = launcher.createSystemdService(installDir, serviceName, serviceInfo.ID, aosConfig); err != nil {
		return service, err
	}

	alertRules, err := json.Marshal(serviceInfo.AlertRules)
	if err != nil {
		return service, err
	}

	service = Service{
		ID:         serviceInfo.ID,
		Version:    serviceInfo.Version,
		Path:       installDir,
		UnitName:   serviceName,
		UserName:   userName,
		State:      stateInit,
		Status:     statusOk,
		AlertRules: string(alertRules),
		Devices:    string(deviceResourcesForService)}

	for _, layerDigest := range imageParts.layersDigest {
		layerPath, err := launcher.layerProvider.GetLayerPathByDigest(layerDigest)
		if err != nil {
			return service, err
		}

		service.Layers = append(service.Layers, layerPath)
	}

	if err = launcher.updateServiceFromAosSrvConfig(&service, aosConfig); err != nil {
		return service, err
	}

	return service, nil
}

func (launcher *Launcher) addService(service Service) (err error) {
	// We can't remove service if it is not in serviceProvider. Just return error and rollback will be
	// handled by parent function

	if err = platform.SetUserFSQuota(launcher.config.StorageDir,
		service.StorageLimit+service.StateLimit, service.UserName); err != nil {
		return err
	}

	if err = launcher.serviceProvider.AddService(service); err != nil {
		return err
	}

	defer func() {
		if err != nil {
			log.WithField("id", service.ID).Errorf("Error adding service: %s", err)

			launcher.removeService(service)
		}
	}()

	if launcher.network != nil {
		if err = launcher.network.NetworkExists(service.ServiceProvider); err != nil {
			if err = launcher.network.CreateNetwork(service.ServiceProvider); err != nil {
				return err
			}
		}
	}

	if err = launcher.addServiceToCurrentUsers(service.ID); err != nil {
		return err
	}

	if err = launcher.addServiceToSystemd(service); err != nil {
		return err
	}

	if err = launcher.startService(service); err != nil {
		return err
	}

	return err
}

func (launcher *Launcher) updateService(oldService, newService Service) (err error) {
	defer func() {
		if err != nil {
			log.WithField("id", newService.ID).Errorf("Update service error: %s", err)

			if err = launcher.stopService(newService); err != nil {
				log.WithField("id", newService.ID).Errorf("Can't stop service: %s", err)
			}

			if err = os.RemoveAll(newService.Path); err != nil {
				log.WithField("id", newService.ID).Errorf("Can't remove new service dir: %s", err)
			}

			if err := launcher.restoreService(oldService); err != nil {
				launcher.removeService(oldService)
				if launcher.sender != nil {
					launcher.sender.SendServiceStatus(amqp.ServiceInfo{
						ID:      oldService.ID,
						Version: oldService.Version,
						Status:  "removed"})
				}
			}
		}
	}()

	launcher.services.Delete(oldService.UnitName)

	if err = launcher.updateServiceState(oldService.ID, stateStopped, statusOk); err != nil {
		return err
	}

	if err = launcher.addServiceToCurrentUsers(newService.ID); err != nil {
		return err
	}

	if err = platform.SetUserFSQuota(launcher.config.StorageDir, newService.StorageLimit, newService.UserName); err != nil {
		return err
	}

	if err = launcher.stopService(oldService); err != nil {
		return err
	}

	if err = os.RemoveAll(oldService.Path); err != nil {
		return err
	}

	if err = launcher.addServiceToSystemd(newService); err != nil {
		return err
	}

	if err = launcher.startService(newService); err != nil {
		return err
	}

	if err = launcher.serviceProvider.UpdateService(newService); err != nil {
		return err
	}

	if launcher.sender != nil {
		launcher.sender.SendServiceStatus(amqp.ServiceInfo{
			ID:      oldService.ID,
			Version: oldService.Version,
			Status:  "removed"})
	}

	return nil
}

func (launcher *Launcher) removeService(service Service) (retErr error) {
	log.WithFields(log.Fields{"id": service.ID, "version": service.Version}).Debug("Remove service")

	if err := launcher.stopService(service); err != nil {
		if retErr == nil {
			retErr = err
		}
	}

	if _, err := launcher.systemd.DisableUnitFiles([]string{service.UnitName}, false); err != nil {
		if retErr == nil {
			log.WithField("name", service.ID).Errorf("Can't disable systemd unit: %s", err)
			retErr = err
		}
	}

	usersServices, err := launcher.serviceProvider.GetUsersServicesByServiceID(service.ID)
	if err != nil {
		if retErr == nil {
			log.WithField("name", service.ID).Errorf("Can't get users services: %s", err)
			retErr = err
		}
	}

	for _, userService := range usersServices {
		if userService.StorageFolder != "" {
			log.WithFields(log.Fields{
				"folder":    userService.StorageFolder,
				"serviceID": service.ID}).Debug("Remove storage folder")

			if err := os.RemoveAll(userService.StorageFolder); err != nil {
				if retErr == nil {
					log.WithField("name", service.ID).Errorf("Can't remove storage folder: %s", err)
					retErr = err
				}
			}
		}
	}

	if err := launcher.serviceProvider.RemoveServiceFromAllUsers(service.ID); err != nil {
		if retErr == nil {
			log.WithField("name", service.ID).Errorf("Can't delete users from DB: %s", err)
			retErr = err
		}
	}

	if err := launcher.serviceProvider.RemoveService(service.ID); err != nil {
		if retErr == nil {
			log.WithField("name", service.ID).Errorf("Can't remove service from DB: %s", err)
			retErr = err
		}
	}

	if launcher.network != nil {
		spServices, err := launcher.serviceProvider.GetServiceProviderServices(service.ServiceProvider)
		if err != nil {
			if retErr == nil {
				log.WithField("name", service.ID).Errorf("Can't get service provider services: %s", err)
				retErr = err
			}
		} else {
			if len(spServices) == 0 {
				if err := launcher.network.DeleteNetwork(service.ServiceProvider); err != nil {
					if retErr == nil {
						log.WithField("name", service.ID).Errorf("Can't remove network: %s", err)
						retErr = err
					}
				}
			}
		}
	}

	if err := os.RemoveAll(service.Path); err != nil {
		if retErr == nil {
			log.WithField("name", service.ID).Errorf("Can't remove service folder: %s", err)
			retErr = err
		}
	}

	if err := platform.DeleteUser(service.ID); err != nil {
		if retErr == nil {
			log.WithField("name", service.ID).Errorf("Can't delete user: %s", err)
			retErr = err
		}
	}

	return retErr
}

func getSystemdServiceTemplate(workingDir string) (template string, err error) {
	fileName := path.Join(workingDir, serviceTemplateFile)
	fileContent, err := ioutil.ReadFile(fileName)
	if err != nil {
		if !os.IsNotExist(err) {
			return template, err
		}

		log.Warnf("Service template file does not exist. Creating %s", fileName)

		if err = ioutil.WriteFile(fileName, []byte(serviceTemplate), 0644); err != nil {
			return template, err
		}

		return serviceTemplate, nil
	}

	return string(fileContent), nil
}

func (launcher *Launcher) createSystemdService(installDir, serviceName, id string, aosConfig aosServiceConfig) (err error) {
	f, err := os.Create(path.Join(installDir, serviceName))
	if err != nil {
		return err
	}
	defer f.Close()

	absServicePath, err := filepath.Abs(installDir)
	if err != nil {
		return err
	}

	lines := strings.SplitAfter(launcher.serviceTemplate, "\n")
	for _, line := range lines {
		// skip comments
		if strings.HasPrefix(line, "#") {
			continue
		}

		// replaces variables with values
		line = strings.Replace(line, "${RUNC}", launcher.runcPath, -1)
		line = strings.Replace(line, "${ID}", id, -1)
		line = strings.Replace(line, "${SERVICEPATH}", absServicePath, -1)

		fmt.Fprint(f, line)
	}

	return err
}

func (launcher *Launcher) updateMonitoring(service Service, state ServiceState) (err error) {
	switch state {
	case stateRunning:
		var rules amqp.ServiceAlertRules

		if err := json.Unmarshal([]byte(service.AlertRules), &rules); err != nil {
			return err
		}

		var ipAddress string

		if launcher.network != nil {
			if ipAddress, err = launcher.network.GetServiceIP(service.ID, service.ServiceProvider); err != nil {
				return err
			}
		}

		if err = launcher.monitor.StartMonitorService(service.ID, monitoring.ServiceMonitoringConfig{
			ServiceDir:    service.Path,
			IPAddress:     ipAddress,
			User:          service.UserName,
			UploadLimit:   uint64(service.UploadLimit),
			DownloadLimit: uint64(service.DownloadLimit),
			ServiceRules:  &rules}); err != nil {
			return err
		}

	case stateStopped:
		if err = launcher.monitor.StopMonitorService(service.ID); err != nil {
			return err
		}
	}

	return nil
}

func (launcher *Launcher) updateServiceFromAosSrvConfig(service *Service, aosSrvConfig aosServiceConfig) (err error) {
	service.TTL = launcher.config.DefaultServiceTTL

	if aosSrvConfig.ServiceTTL != nil {
		service.TTL = *aosSrvConfig.ServiceTTL
	}

	if aosSrvConfig.Quotas.UploadLimit != nil {
		service.UploadLimit = *aosSrvConfig.Quotas.UploadLimit
	}

	if aosSrvConfig.Quotas.DownloadLimit != nil {
		service.DownloadLimit = *aosSrvConfig.Quotas.DownloadLimit
	}

	if aosSrvConfig.Quotas.UploadSpeed != nil {
		service.UploadSpeed = *aosSrvConfig.Quotas.UploadSpeed
	}

	if aosSrvConfig.Quotas.DownloadSpeed != nil {
		service.DownloadSpeed = *aosSrvConfig.Quotas.DownloadSpeed
	}

	if aosSrvConfig.Quotas.StorageLimit != nil {
		service.StorageLimit = *aosSrvConfig.Quotas.StorageLimit
	}

	if aosSrvConfig.Quotas.StateLimit != nil {
		service.StateLimit = *aosSrvConfig.Quotas.StateLimit
	}

	service.Permissions = aosSrvConfig.Quotas.VisPermissions

	if aosSrvConfig.Hostname != nil {
		service.HostName = *aosSrvConfig.Hostname
	}

	service.ServiceProvider = aosSrvConfig.ServiceProvider
	if service.ServiceProvider == "" {
		service.ServiceProvider = defaultServiceProvider
	}

	return nil
}

func (launcher *Launcher) addServiceToCurrentUsers(serviceID string) (err error) {
	_, err = launcher.serviceProvider.GetUsersService(launcher.users, serviceID)
	if err == nil {
		return nil
	}

	if !strings.Contains(err.Error(), "not exist") {
		return err
	}

	if err = launcher.serviceProvider.AddServiceToUsers(launcher.users, serviceID); err != nil {
		return err
	}

	return nil
}

func (launcher *Launcher) cleanServicesDB() (err error) {
	log.Debug("Clean services DB")

	startedServices, err := launcher.serviceProvider.GetUsersServices(launcher.users)
	if err != nil {
		return err
	}

	allServices, err := launcher.serviceProvider.GetServices()
	if err != nil {
		return err
	}

	now := time.Now()

	servicesToBeRemoved := 0
	statusChannel := make(chan error, len(allServices))

	for _, service := range allServices {
		// check if service just started
		justStarted := false

		for _, startedService := range startedServices {
			if service.ID == startedService.ID {
				justStarted = true
				break
			}
		}

		if justStarted {
			continue
		}

		if service.StartAt.Add(time.Hour*24*time.Duration(service.TTL)).Before(now) == true {
			servicesToBeRemoved++

			go func(service Service) {
				statusChannel <- launcher.removeService(service)
			}(service)
		}
	}

	// Wait all services are removed
	for i := 0; i < servicesToBeRemoved; i++ {
		<-statusChannel
	}

	return nil
}
