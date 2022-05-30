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

package iamclient_test

import (
	"context"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aoscloud/aos_common/aoserrors"
	"github.com/aoscloud/aos_common/api/cloudprotocol"
	pb "github.com/aoscloud/aos_common/api/iamanager/v2"
	"github.com/golang/protobuf/ptypes/empty"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/aoscloud/aos_servicemanager/config"
	"github.com/aoscloud/aos_servicemanager/iamclient"
)

/***********************************************************************************************************************
 * Consts
 **********************************************************************************************************************/

const (
	protectedServerURL = "localhost:8089"
	publicServerURL    = "localhost:8090"
	secretLength       = 8
	secretSymbols      = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

type testPublicServer struct {
	pb.UnimplementedIAMPublicServiceServer
	grpcServer             *grpc.Server
	subjects               []string
	subjectsChangedChannel chan []string
	permissionsCache       map[string]servicePermissions
}

type testProtectedServer struct {
	pb.UnimplementedIAMProtectedServiceServer
	grpcServer       *grpc.Server
	permissionsCache map[string]servicePermissions
}

type servicePermissions struct {
	instaneIdent cloudprotocol.InstanceIdent
	permissions  map[string]map[string]string
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

	tmpDir, err = ioutil.TempDir("", "iam_")
	if err != nil {
		log.Fatalf("Error create temporary dir: %s", err)
	}

	ret := m.Run()

	if err := os.RemoveAll(tmpDir); err != nil {
		log.Fatalf("Error removing tmp dir: %s", err)
	}

	os.Exit(ret)
}

/***********************************************************************************************************************
 * Tests
 **********************************************************************************************************************/

func TestGetSubjects(t *testing.T) {
	permissionsCache := make(map[string]servicePermissions)

	publicServer, protectedServer, err := newTestServers(publicServerURL, protectedServerURL, permissionsCache)
	if err != nil {
		t.Fatalf("Can't create test server: %s", err)
	}

	defer publicServer.close()
	defer protectedServer.close()

	publicServer.subjects = []string{"subject1", "subject2", "subject3"}

	client, err := iamclient.New(&config.Config{
		IAMServerURL:       protectedServerURL,
		IAMPublicServerURL: publicServerURL,
	}, nil, true)
	if err != nil {
		t.Fatalf("Can't create IAM client: %s", err)
	}
	defer client.Close()

	if !reflect.DeepEqual(publicServer.subjects, client.GetSubjects()) {
		t.Errorf("Invalid subjects: %s", client.GetSubjects())
	}

	newSubjects := []string{"newSubjects1", "newSubjects2", "newSubjects3"}

	publicServer.subjectsChangedChannel <- newSubjects

	select {
	case subjects := <-client.GetSubjectsChangedChannel():
		if !reflect.DeepEqual(subjects, newSubjects) {
			t.Errorf("Invalid subjects: %s", subjects)
		}

	case <-time.After(5 * time.Second):
		t.Error("Wait subjects changed timeout")
	}
}

func TestRegisterService(t *testing.T) {
	permissionsCache := make(map[string]servicePermissions)

	publicServer, protectedServer, err := newTestServers(publicServerURL, protectedServerURL, permissionsCache)
	if err != nil {
		t.Fatalf("Can't create test server: %s", err)
	}

	defer publicServer.close()
	defer protectedServer.close()

	client, err := iamclient.New(&config.Config{
		IAMServerURL:       protectedServerURL,
		IAMPublicServerURL: publicServerURL,
	}, nil, true)
	if err != nil {
		t.Fatalf("Can't create IAM client: %s", err)
	}

	defer client.Close()

	var (
		permissions      = map[string]map[string]string{"vis": {"*": "rw", "test": "r"}}
		registerInstance = cloudprotocol.InstanceIdent{ServiceID: "serviceID1", SubjectID: "s1", Instance: 1}
	)

	secret, err := client.RegisterInstance(registerInstance, permissions)
	if err != nil || secret == "" {
		t.Errorf("Can't send a request: %s", err)
	}

	secret, err = client.RegisterInstance(registerInstance, permissions)
	if err == nil || secret != "" {
		t.Error("Re-registration of the service is prohibited")
	}

	err = client.UnregisterInstance(registerInstance)
	if err != nil {
		t.Errorf("Can't send a request: %s", err)
	}

	secret, err = client.RegisterInstance(registerInstance, permissions)
	if err != nil || secret == "" {
		t.Errorf("Can't send a request: %s", err)
	}
}

func TestGetPermissions(t *testing.T) {
	permissionsCache := make(map[string]servicePermissions)

	publicServer, protectedServer, err := newTestServers(publicServerURL, protectedServerURL, permissionsCache)
	if err != nil {
		t.Fatalf("Can't create test server: %s", err)
	}

	defer publicServer.close()
	defer protectedServer.close()

	client, err := iamclient.New(&config.Config{
		IAMServerURL:       protectedServerURL,
		IAMPublicServerURL: publicServerURL,
	}, nil, true)
	if err != nil {
		t.Fatalf("Can't create IAM client: %s", err)
	}

	defer client.Close()

	var (
		permissions      = map[string]map[string]string{"vis": {"*": "rw", "test": "r"}}
		registerInstance = cloudprotocol.InstanceIdent{ServiceID: "serviceID1", SubjectID: "s1", Instance: 1}
	)

	secret, err := client.RegisterInstance(registerInstance, permissions)
	if err != nil || secret == "" {
		t.Errorf("Can't send a request: %s", err)
	}

	receivedInstacne, respPermissions, err := client.GetPermissions(secret, "vis")
	if err != nil {
		t.Errorf("Can't send a request: %s", err)
	}

	if !reflect.DeepEqual(respPermissions, permissions["vis"]) {
		t.Errorf("Wrong permissions: %v", respPermissions)
	}

	if receivedInstacne != registerInstance {
		t.Error("Incorrect received instance")
	}

	err = client.UnregisterInstance(registerInstance)
	if err != nil {
		t.Errorf("Can't send a request: %s", err)
	}

	_, _, err = client.GetPermissions(secret, "vis")
	if err == nil {
		t.Error("Getting permissions after removing a service")
	}
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func newTestServers(publicServerURL, protectedServerURL string, permissionsCache map[string]servicePermissions) (
	publicServer *testPublicServer, protectedServer *testProtectedServer, err error,
) {
	publicServer = &testPublicServer{
		subjectsChangedChannel: make(chan []string, 1),
		permissionsCache:       permissionsCache,
	}

	publicListener, err := net.Listen("tcp", publicServerURL)
	if err != nil {
		return nil, nil, aoserrors.Wrap(err)
	}

	publicServer.grpcServer = grpc.NewServer()

	pb.RegisterIAMPublicServiceServer(publicServer.grpcServer, publicServer)

	go func() {
		if err := publicServer.grpcServer.Serve(publicListener); err != nil {
			log.Errorf("Can't serve grpc server: %s", err)
		}
	}()

	protectedServer = &testProtectedServer{permissionsCache: permissionsCache}

	protectedListener, err := net.Listen("tcp", protectedServerURL)
	if err != nil {
		return nil, nil, aoserrors.Wrap(err)
	}

	protectedServer.grpcServer = grpc.NewServer()

	pb.RegisterIAMProtectedServiceServer(protectedServer.grpcServer, protectedServer)

	go func() {
		if err := protectedServer.grpcServer.Serve(protectedListener); err != nil {
			log.Errorf("Can't serve grpc server: %s", err)
		}
	}()

	return publicServer, protectedServer, nil
}

func (server *testPublicServer) close() {
	if server.grpcServer != nil {
		server.grpcServer.Stop()
	}
}

func (server *testProtectedServer) close() {
	if server.grpcServer != nil {
		server.grpcServer.Stop()
	}
}

func (server *testProtectedServer) RegisterInstance(
	context context.Context, req *pb.RegisterInstanceRequest,
) (rsp *pb.RegisterInstanceResponse, err error) {
	rsp = &pb.RegisterInstanceResponse{}

	instanceIdent := pbToInstanceIdent(req.Instance)

	if secret := server.findSecret(instanceIdent); secret != "" {
		return rsp, aoserrors.New("service is already registered")
	}

	secret := randomString()
	rsp.Secret = secret

	permissions := make(map[string]map[string]string)
	for key, value := range req.Permissions {
		permissions[key] = value.Permissions
	}

	server.permissionsCache[secret] = servicePermissions{instaneIdent: instanceIdent, permissions: permissions}

	return rsp, nil
}

func (server *testProtectedServer) UnregisterInstance(
	ctx context.Context, req *pb.UnregisterInstanceRequest,
) (rsp *empty.Empty, err error) {
	rsp = &empty.Empty{}

	secret := server.findSecret(pbToInstanceIdent(req.Instance))
	if secret == "" {
		return rsp, aoserrors.New("service is not registered ")
	}

	delete(server.permissionsCache, secret)

	return rsp, nil
}

func (server *testPublicServer) GetPermissions(
	ctx context.Context, req *pb.PermissionsRequest,
) (rsp *pb.PermissionsResponse, err error) {
	rsp = &pb.PermissionsResponse{}

	funcServersPermissions, ok := server.permissionsCache[req.Secret]
	if !ok {
		return rsp, aoserrors.New("secret not found")
	}

	permissions, ok := funcServersPermissions.permissions[req.FunctionalServerId]
	if !ok {
		return rsp, aoserrors.New("permissions for functional server not found")
	}

	rsp.Permissions = &pb.Permissions{Permissions: permissions}
	rsp.Instance = instanceIdentToPB(funcServersPermissions.instaneIdent)

	return rsp, nil
}

func (server *testPublicServer) GetSubjects(context context.Context, req *empty.Empty) (rsp *pb.Subjects, err error) {
	rsp = &pb.Subjects{Subjects: server.subjects}

	return rsp, nil
}

func (server *testPublicServer) SubscribeSubjectsChanged(req *empty.Empty,
	stream pb.IAMPublicService_SubscribeSubjectsChangedServer,
) (err error) {
	for {
		select {
		case <-stream.Context().Done():
			return nil

		case subjects := <-server.subjectsChangedChannel:
			if err := stream.Send(&pb.Subjects{Subjects: subjects}); err != nil {
				return aoserrors.Wrap(err)
			}

			return nil
		}
	}
}

func (server *testProtectedServer) findSecret(instance cloudprotocol.InstanceIdent) (secret string) {
	for key, value := range server.permissionsCache {
		if value.instaneIdent == instance {
			return key
		}
	}

	return ""
}

func randomString() string {
	secret := make([]byte, secretLength)

	rand.Seed(time.Now().UnixNano())

	for i := range secret {
		secret[i] = secretSymbols[rand.Intn(len(secretSymbols))] // nolint:gosec
	}

	return string(secret)
}

func instanceIdentToPB(ident cloudprotocol.InstanceIdent) *pb.InstanceIdent {
	return &pb.InstanceIdent{ServiceId: ident.ServiceID, SubjectId: ident.SubjectID, Instance: int64(ident.Instance)}
}

func pbToInstanceIdent(ident *pb.InstanceIdent) cloudprotocol.InstanceIdent {
	return cloudprotocol.InstanceIdent{
		ServiceID: ident.ServiceId, SubjectID: ident.SubjectId, Instance: uint64(ident.Instance),
	}
}
