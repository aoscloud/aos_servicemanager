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

package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	pb "github.com/aoscloud/aos_common/api/servicemanager/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/aoscloud/aos_servicemanager/launcher"
	"github.com/aoscloud/aos_servicemanager/servicemanager"
)

/*******************************************************************************
 * Variables
 ******************************************************************************/

var (
	tmpDir string
	db     *Database
)

/*******************************************************************************
 * Init
 ******************************************************************************/

func init() {
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: false,
		TimestampFormat:  "2006-01-02 15:04:05.000",
		FullTimestamp:    true,
	})
	log.SetLevel(log.DebugLevel)
	log.SetOutput(os.Stdout)
}

/*******************************************************************************
 * Main
 ******************************************************************************/

func TestMain(m *testing.M) {
	var err error

	tmpDir, err = ioutil.TempDir("", "sm_")
	if err != nil {
		log.Fatalf("Error create temporary dir: %s", err)
	}

	dbPath := path.Join(tmpDir, "test.db")

	db, err = New(dbPath, tmpDir, tmpDir)
	if err != nil {
		log.Fatalf("Can't create database: %s", err)
	}

	ret := m.Run()

	if err = os.RemoveAll(tmpDir); err != nil {
		log.Fatalf("Error cleaning up: %s", err)
	}

	db.Close()

	os.Exit(ret)
}

/*******************************************************************************
 * Tests
 ******************************************************************************/

func TestAddGetService(t *testing.T) {
	// AddService
	service := servicemanager.ServiceInfo{
		ServiceID: "service1", AosVersion: 1, ServiceProvider: "sp1", Description: "", ImagePath: "to/service1",
		GID: 5001, IsActive: false,
	}

	if err := db.AddService(service); err != nil {
		t.Errorf("Can't add service: %s", err)
	}

	service.AosVersion = 2
	if err := db.AddService(service); err != nil {
		t.Errorf("Can't add service: %s", err)
	}

	// GetService
	serviceFromDB, err := db.GetService("service1")
	if err != nil {
		t.Errorf("Can't get service: %s", err)
	}

	if !reflect.DeepEqual(serviceFromDB, service) {
		t.Error("service1 doesn't match stored one")
	}

	service2 := servicemanager.ServiceInfo{
		ServiceID: "service2", AosVersion: 1, ServiceProvider: "sp1", Description: "", ImagePath: "to/service1",
		GID: 5001, IsActive: false,
	}

	if err := db.AddService(service2); err != nil {
		t.Errorf("Can't add service: %s", err)
	}

	if err := db.AddService(service2); err == nil {
		t.Error("Should be error can't add service")
	}

	services, err := db.GetAllServiceVersions("service1")
	if err != nil {
		t.Errorf("Can't get all service versions: %s", err)
	}

	if len(services) != 2 {
		t.Errorf("incorrect count of services %d", len(services))
	}

	if services, err = db.GetAllServices(); err != nil {
		t.Errorf("Can't get all services: %s", err)
	}

	if len(services) != 3 {
		t.Errorf("incorrect count of all services %d", len(services))
	}

	// Clear DB
	if err = db.removeAllServices(); err != nil {
		t.Errorf("Can't remove all services: %s", err)
	}
}

func TestNotExistService(t *testing.T) {
	// GetService
	_, err := db.GetService("service3")
	if err == nil {
		t.Error("Error in non existed service")
	} else if !errors.Is(err, servicemanager.ErrNotExist) {
		t.Errorf("Can't get service: %s", err)
	}
}

func TestRemoveService(t *testing.T) {
	// AddService
	service := servicemanager.ServiceInfo{
		ServiceID: "service1", AosVersion: 1, ServiceProvider: "sp1", Description: "", ImagePath: "to/service1",
		GID: 5001, IsActive: false,
	}

	if err := db.AddService(service); err != nil {
		t.Errorf("Can't add service: %s", err)
	}

	// GetService
	serviceFromDB, err := db.GetService("service1")
	if err != nil {
		t.Errorf("Can't get service: %s", err)
	}

	if !reflect.DeepEqual(serviceFromDB, service) {
		t.Error("service1 doesn't match stored one")
	}

	if err := db.RemoveService(service); err != nil {
		t.Error("Can't delete service: ", err)
	}

	if _, err := db.GetService("service1"); err == nil {
		t.Errorf("Should be error not exist ")
	}

	// Clear DB
	if err = db.removeAllServices(); err != nil {
		t.Errorf("Can't remove all services: %s", err)
	}
}

func TestActivateService(t *testing.T) {
	// AddService
	service := servicemanager.ServiceInfo{
		ServiceID: "serviceActivate", AosVersion: 1, ServiceProvider: "sp1", Description: "", ImagePath: "to/service1",
		GID: 5001, IsActive: false,
	}

	if err := db.AddService(service); err != nil {
		t.Errorf("Can't add service: %s", err)
	}

	if err := db.ActivateService(service); err != nil {
		t.Errorf("Can't activate service: %s", err)
	}

	// GetService
	serviceFromDB, err := db.GetService("serviceActivate")
	if err != nil {
		t.Errorf("Can't get service: %s", err)
	}

	if serviceFromDB.IsActive != true {
		t.Error("Wrong active value")
	}
}

func TestAddSameUsersService(t *testing.T) {
	// Add service
	err := db.AddServiceToUsers([]string{"user0", "user1"}, "service1")
	if err != nil {
		t.Errorf("Can't add users service: %s", err)
	}

	// Add service
	err = db.AddServiceToUsers([]string{"user0", "user1"}, "service1")
	if err == nil {
		t.Error("Error adding same users service")
	}

	// Clear DB
	if err = db.removeAllUsers(); err != nil {
		t.Errorf("Can't remove all users: %s", err)
	}
}

func TestNotExistUsersServices(t *testing.T) {
	// GetService
	_, err := db.GetUsersService([]string{"user2", "user3"}, "service18")
	if err != nil && !errors.Is(err, ErrNotExist) {
		t.Fatalf("Can't check if service in users: %s", err)
	}

	if err == nil {
		t.Errorf("Error users service: %s", err)
	}
}

func TestRemoveUsersService(t *testing.T) {
	// Add service
	err := db.AddServiceToUsers([]string{"user0", "user1"}, "service1")
	if err != nil {
		t.Errorf("Can't add users service: %s", err)
	}

	// Remove service
	err = db.RemoveServiceFromUsers([]string{"user0", "user1"}, "service1")
	if err != nil {
		t.Errorf("Can't remove users service: %s", err)
	}

	_, err = db.GetUsersService([]string{"user0", "user1"}, "service1")
	if err != nil && !errors.Is(err, ErrNotExist) {
		t.Fatalf("Can't check if service in users: %s", err)
	}

	if err == nil {
		t.Errorf("Error users service: %s", err)
	}
}

func TestAddUsersList(t *testing.T) {
	numUsers := 5
	numServices := 3

	for i := 0; i < numUsers; i++ {
		users := []string{fmt.Sprintf("user%d", i)}
		for j := 0; j < numServices; j++ {
			err := db.AddServiceToUsers(users, fmt.Sprintf("service%d", j))
			if err != nil {
				t.Errorf("Can't add users service: %s", err)
			}
		}
	}

	// Check users list
	usersList, err := db.getUsersList()
	if err != nil {
		t.Fatalf("Can't get users list: %s", err)
	}

	if len(usersList) != numUsers {
		t.Fatal("Wrong users count")
	}

	for _, users := range usersList {
		ok := false

		for i := 0; i < numUsers; i++ {
			if users[0] == fmt.Sprintf("user%d", i) {
				ok = true

				break
			}
		}

		if !ok {
			t.Errorf("Invalid users: %s", users)
		}
	}

	for j := 0; j < numServices; j++ {
		serviceID := fmt.Sprintf("service%d", j)

		usersServices, err := db.GetUsersServicesByServiceID(serviceID)
		if err != nil {
			t.Errorf("Can't get users services: %s", err)
		}

		for _, userService := range usersServices {
			if userService.ServiceID != serviceID {
				t.Errorf("Invalid serviceID: %s", userService.ServiceID)
			}

			ok := false

			for i := 0; i < numUsers; i++ {
				if userService.Users[0] == fmt.Sprintf("user%d", i) {
					ok = true

					break
				}
			}

			if !ok {
				t.Errorf("Invalid users: %s", userService.Users)
			}
		}

		err = db.RemoveServiceFromAllUsers(serviceID)
		if err != nil {
			t.Errorf("Can't delete users: %s", err)
		}
	}

	usersList, err = db.getUsersList()
	if err != nil {
		t.Fatalf("Can't get users list: %s", err)
	}

	if len(usersList) != 0 {
		t.Fatal("Wrong users count")
	}

	// Clear DB
	if err = db.removeAllUsers(); err != nil {
		t.Errorf("Can't remove all users: %s", err)
	}
}

func TestUsersStorage(t *testing.T) {
	// Add users service
	err := db.AddServiceToUsers([]string{"user1"}, "service1")
	if err != nil {
		t.Errorf("Can't add users service: %s", err)
	}

	// Check default values
	usersService, err := db.GetUsersService([]string{"user1"}, "service1")
	if err != nil {
		t.Errorf("Can't get users service: %s", err)
	}

	if usersService.StorageFolder != "" || len(usersService.StateChecksum) != 0 {
		t.Error("Wrong users service value")
	}

	if err = db.SetUsersStorageFolder([]string{"user1"}, "service1", "stateFolder1"); err != nil {
		t.Errorf("Can't set users storage folder: %s", err)
	}

	if err = db.SetUsersStateChecksum([]string{"user1"}, "service1", []byte{0, 1, 2, 3, 4, 5}); err != nil {
		t.Errorf("Can't set users state checksum: %s", err)
	}

	usersService, err = db.GetUsersService([]string{"user1"}, "service1")
	if err != nil {
		t.Errorf("Can't get users service: %s", err)
	}

	if usersService.StorageFolder != "stateFolder1" ||
		!reflect.DeepEqual(usersService.StateChecksum, []byte{0, 1, 2, 3, 4, 5}) {
		t.Error("Wrong users service value")
	}

	// Clear DB
	if err = db.removeAllUsers(); err != nil {
		t.Errorf("Can't remove all users: %s", err)
	}
}

func TestOverideEnvVars(t *testing.T) {
	// Add users service
	if err := db.AddServiceToUsers([]string{"subject1"}, "service1"); err != nil {
		t.Errorf("Can't add subject service: %s", err)
	}

	ttl := time.Now().UTC()

	envVars := []*pb.EnvVarInfo{}
	envVars = append(envVars, &pb.EnvVarInfo{VarId: "some", Variable: "log=10", Ttl: timestamppb.New(ttl)})

	if err := db.UpdateOverrideEnvVars([]string{"subject1"}, "service2", envVars); err != nil {
		if !errors.Is(err, ErrNotExist) {
			t.Errorf("Should be error: %s", ErrNotExist)
		}
	}

	if err := db.UpdateOverrideEnvVars([]string{"subject1"}, "service1", envVars); err != nil {
		t.Errorf("Can't update override env vars: %s", err)
	}

	allVars, err := db.GetAllOverrideEnvVars()
	if err != nil {
		t.Errorf("Can't get all env vars: %s", err)
	}

	if len(allVars) != 1 {
		t.Error("Count of all env vars should be 1")
	}

	if reflect.DeepEqual(allVars[0].Vars, envVars) == false {
		t.Error("Incorrect env vars in get all override env vars request")
	}
}

func TestTrafficMonitor(t *testing.T) {
	setTime := time.Now()
	setValue := uint64(100)

	if err := db.SetTrafficMonitorData("chain1", setTime, setValue); err != nil {
		t.Fatalf("Can't set traffic monitor: %s", err)
	}

	getTime, getValue, err := db.GetTrafficMonitorData("chain1")
	if err != nil {
		t.Fatalf("Can't get traffic monitor: %s", err)
	}

	if !getTime.Equal(setTime) || getValue != setValue {
		t.Fatalf("Wrong value time: %s, value %d", getTime, getValue)
	}

	if err := db.RemoveTrafficMonitorData("chain1"); err != nil {
		t.Fatalf("Can't remove traffic monitor: %s", err)
	}

	if _, _, err := db.GetTrafficMonitorData("chain1"); err == nil {
		t.Fatal("Entry should be removed")
	}

	// Clear DB
	if err := db.removeAllTrafficMonitor(); err != nil {
		t.Errorf("Can't remove all traffic monitor: %s", err)
	}
}

func TestOperationVersion(t *testing.T) {
	var setOperationVersion uint64 = 123

	if err := db.SetOperationVersion(setOperationVersion); err != nil {
		t.Fatalf("Can't set operation version: %s", err)
	}

	getOperationVersion, err := db.GetOperationVersion()
	if err != nil {
		t.Fatalf("Can't get operation version: %s", err)
	}

	if setOperationVersion != getOperationVersion {
		t.Errorf("Wrong operation version: %d", getOperationVersion)
	}
}

func TestCursor(t *testing.T) {
	setCursor := "cursor123"

	if err := db.SetJournalCursor(setCursor); err != nil {
		t.Fatalf("Can't set logging cursor: %s", err)
	}

	getCursor, err := db.GetJournalCursor()
	if err != nil {
		t.Fatalf("Can't get logger cursor: %s", err)
	}

	if getCursor != setCursor {
		t.Fatalf("Wrong cursor value: %s", getCursor)
	}
}

// func TestGetServiceByUnitName(t *testing.T) {
// 	// AddService
// 	service1 := launcher.Service{
// 		ID: "service1", AosVersion: 1, VendorVersion: "", ServiceProvider: "sp1",
// 		Path: "to/service1", UnitName: "service1.service", UID: 5001, GID: 5001, State: 0,
// 		StartAt: time.Now().UTC(), AlertRules: "", Description: "",
// 	}

// 	err := db.AddService(service1)
// 	if err != nil {
// 		t.Errorf("Can't add service: %s", err)
// 	}

// 	// GetService
// 	service, err := db.GetServiceByUnitName("service1.service")
// 	if err != nil {
// 		t.Errorf("Can't get service: %s", err)
// 	}

// 	if !reflect.DeepEqual(service, service1) {
// 		t.Error("service1 doesn't match stored one")
// 	}

// 	// Clear DB
// 	if err = db.removeAllServices(); err != nil {
// 		t.Errorf("Can't remove all services: %s", err)
// 	}
// }

func TestMultiThread(t *testing.T) {
	const numIterations = 1000

	var wg sync.WaitGroup

	wg.Add(4)

	go func() {
		defer wg.Done()

		for i := 0; i < numIterations; i++ {
			if err := db.SetOperationVersion(uint64(i)); err != nil {
				t.Errorf("Can't set operation version: %s", err)

				return
			}
		}
	}()

	go func() {
		defer wg.Done()

		_, err := db.GetOperationVersion()
		if err != nil {
			t.Errorf("Can't get Operation Version : %s", err)

			return
		}
	}()

	go func() {
		defer wg.Done()

		for i := 0; i < numIterations; i++ {
			if err := db.SetJournalCursor(strconv.Itoa(i)); err != nil {
				t.Errorf("Can't set journal cursor: %s", err)

				return
			}
		}
	}()

	go func() {
		defer wg.Done()

		for i := 0; i < numIterations; i++ {
			if _, err := db.GetJournalCursor(); err != nil {
				t.Errorf("Can't get journal cursor: %s", err)

				return
			}
		}
	}()

	wg.Wait()
}

func TestLayers(t *testing.T) {
	if err := db.AddLayer("sha256:1", "id1", "path1", "1", "1.0", "some layer 1", 1); err != nil {
		t.Errorf("Can't add layer %s", err)
	}

	if err := db.AddLayer("sha256:2", "id2", "path2", "1", "2.0", "some layer 2", 2); err != nil {
		t.Errorf("Can't add layer %s", err)
	}

	if err := db.AddLayer("sha256:3", "id3", "path3", "1", "1.0", "some layer 3", 3); err != nil {
		t.Errorf("Can't add layer %s", err)
	}

	path, err := db.GetLayerPathByDigest("sha256:2")
	if err != nil {
		t.Errorf("Can't get layer path %s", err)
	}

	if path != "path2" {
		t.Errorf("Path form db %s != path2", path)
	}

	layerInfo, err := db.GetLayerInfoByDigest("sha256:3")
	if err != nil {
		t.Errorf("Can't get layer ino by digest path %s", err)
	}

	if layerInfo.LayerId != "id3" {
		t.Error("Incorrect layerID ", layerInfo.LayerId)
	}

	if _, err := db.GetLayerPathByDigest("sha256:12345"); err == nil {
		t.Errorf("Should be error: entry does not exist")
	}

	if _, err := db.GetLayerPathByDigest("sha256:12345"); err == nil {
		t.Errorf("Should be error: entry does not exist")
	}

	if err := db.DeleteLayerByDigest("sha256:2"); err != nil {
		t.Errorf("Can't delete layer %s", err)
	}

	layers, err := db.GetLayersInfo()
	if err != nil {
		t.Errorf("Can't get layers info %s", err)
	}

	if len(layers) != 2 {
		t.Errorf("Count of layers in DB %d != 2", len(layers))
	}

	if layers[0].AosVersion != 1 {
		t.Errorf("Layer AosVersion should be 1")
	}
}

func TestMigrationToV1(t *testing.T) {
	migrationDB := path.Join(tmpDir, "test_migration.db")
	mergedMigrationDir := path.Join(tmpDir, "mergedMigration")

	if err := os.MkdirAll(mergedMigrationDir, 0o755); err != nil {
		t.Fatalf("Error creating merged migration dir: %s", err)
	}

	defer func() {
		if err := os.RemoveAll(mergedMigrationDir); err != nil {
			t.Fatalf("Error removing merged migration dir: %s", err)
		}
	}()

	if err := createDatabaseV0(migrationDB); err != nil {
		t.Fatalf("Can't create initial database %s", err)
	}

	// Migration upward
	db, err := newDatabase(migrationDB, "migration", mergedMigrationDir, 1)
	if err != nil {
		t.Fatalf("Can't create database: %s", err)
	}

	if err = isDatabaseVer1(db.sql); err != nil {
		t.Fatalf("Error checking db version: %s", err)
	}

	db.Close()

	// Migration downward
	db, err = newDatabase(migrationDB, "migration", mergedMigrationDir, 0)
	if err != nil {
		t.Fatalf("Can't create database: %s", err)
	}

	if err = isDatabaseVer0(db.sql); err != nil {
		t.Fatalf("Error checking db version: %s", err)
	}

	db.Close()
}

/*******************************************************************************
 * Private
 ******************************************************************************/

func (db *Database) getUsersList() (usersList [][]string, err error) {
	rows, err := db.sql.Query("SELECT DISTINCT users FROM users")
	if err != nil {
		return usersList, aoserrors.Wrap(err)
	}
	defer rows.Close()

	usersList = make([][]string, 0)

	for rows.Next() {
		var usersJSON []byte
		if err := rows.Scan(&usersJSON); err != nil {
			return usersList, aoserrors.Wrap(err)
		}

		var users []string

		if err = json.Unmarshal(usersJSON, &users); err != nil {
			return usersList, aoserrors.Wrap(err)
		}

		usersList = append(usersList, users)
	}

	return usersList, aoserrors.Wrap(rows.Err())
}

func createDatabaseV0(name string) (err error) {
	sqlite, err := sql.Open("sqlite3", fmt.Sprintf("%s?_busy_timeout=%d&_journal_mode=%s&_sync=%s",
		name, busyTimeout, journalMode, syncMode))
	if err != nil {
		return aoserrors.Wrap(err)
	}
	defer sqlite.Close()

	if _, err = sqlite.Exec(
		`CREATE TABLE config (
			operationVersion INTEGER,
			cursor TEXT)`); err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err = sqlite.Exec(
		`INSERT INTO config (
			operationVersion,
			cursor) values(?, ?)`, launcher.OperationVersion, ""); err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err = sqlite.Exec(`CREATE TABLE IF NOT EXISTS services (id TEXT NOT NULL PRIMARY KEY,
															   aosVersion INTEGER,
															   serviceProvider TEXT,
															   path TEXT,
															   unit TEXT,
															   uid INTEGER,
															   gid INTEGER,
															   hostName TEXT,
															   permissions TEXT,
															   state INTEGER,
															   status INTEGER,
															   startat TIMESTAMP,
															   ttl INTEGER,
															   alertRules TEXT,
															   ulLimit INTEGER,
															   dlLimit INTEGER,
															   ulSpeed INTEGER,
															   dlSpeed INTEGER,
															   storageLimit INTEGER,
															   stateLimit INTEGER,
															   layerList TEXT,
															   deviceResources TEXT,
															   boardResources TEXT,
															   vendorVersion TEXT,
															   description TEXT)`); err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err = sqlite.Exec(`CREATE TABLE IF NOT EXISTS users (users TEXT NOT NULL,
															serviceid TEXT NOT NULL,
															storageFolder TEXT,
															stateCheckSum BLOB,
															PRIMARY KEY(users, serviceid))`); err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err = sqlite.Exec(`CREATE TABLE IF NOT EXISTS trafficmonitor (chain TEXT NOT NULL PRIMARY KEY,
																	 time TIMESTAMP,
																	 value INTEGER)`); err != nil {
		return aoserrors.Wrap(err)
	}

	if _, err = sqlite.Exec(`CREATE TABLE IF NOT EXISTS layers (digest TEXT NOT NULL PRIMARY KEY,
															 layerId TEXT,
															 path TEXT,
															 osVersion TEXT,
															 vendorVersion TEXT,
															 description TEXT,
															 aosVersion INTEGER)`); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func isDatabaseVer1(sqlite *sql.DB) (err error) {
	rows, err := sqlite.Query(
		"SELECT COUNT(*) AS CNTREC FROM pragma_table_info('config') WHERE name='componentsUpdateInfo'")
	if err != nil {
		return aoserrors.Wrap(err)
	}
	defer rows.Close()

	if rows.Err() != nil {
		return aoserrors.Wrap(rows.Err())
	}

	var count int
	if rows.Next() {
		if err = rows.Scan(&count); err != nil {
			return aoserrors.Wrap(err)
		}

		if count == 0 {
			return ErrNotExist
		}
	}

	servicesRows, err := sqlite.Query(
		"SELECT COUNT(*) AS CNTREC FROM pragma_table_info('services') WHERE name='exposedPorts'")
	if err != nil {
		return aoserrors.Wrap(err)
	}
	defer servicesRows.Close()

	if servicesRows.Err() != nil {
		return aoserrors.Wrap(servicesRows.Err())
	}

	if !servicesRows.Next() {
		return ErrNotExist
	}

	count = 0
	if err = servicesRows.Scan(&count); err != nil {
		return aoserrors.Wrap(err)
	}

	if count == 0 {
		return ErrNotExist
	}

	return nil
}

func isDatabaseVer0(sqlite *sql.DB) (err error) {
	rows, err := sqlite.Query(
		"SELECT COUNT(*) AS CNTREC FROM pragma_table_info('config') WHERE name='componentsUpdateInfo'")
	if err != nil {
		return aoserrors.Wrap(err)
	}
	defer rows.Close()

	if rows.Err() != nil {
		return aoserrors.Wrap(rows.Err())
	}

	var count int
	if rows.Next() {
		if err = rows.Scan(&count); err != nil {
			return aoserrors.Wrap(err)
		}

		if count != 0 {
			return ErrNotExist
		}
	}

	servicesRows, err := sqlite.Query(
		"SELECT COUNT(*) AS CNTREC FROM pragma_table_info('config') WHERE name='exposedPorts'")
	if err != nil {
		return aoserrors.Wrap(err)
	}
	defer servicesRows.Close()

	if servicesRows.Err() != nil {
		return aoserrors.Wrap(servicesRows.Err())
	}

	if !servicesRows.Next() {
		return ErrNotExist
	}

	count = 0
	if err = servicesRows.Scan(&count); err != nil {
		return aoserrors.Wrap(err)
	}

	if count != 0 {
		return ErrNotExist
	}

	return nil
}
