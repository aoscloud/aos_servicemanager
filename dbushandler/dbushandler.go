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

package dbushandler

import (
	"errors"

	"github.com/godbus/dbus"
	"github.com/godbus/dbus/introspect"
	log "github.com/sirupsen/logrus"

	"aos_servicemanager/launcher"
)

/*******************************************************************************
 * Consts
 ******************************************************************************/

const (
	// ObjectPath object path
	ObjectPath = "/com/aos/servicemanager/vis"
	// InterfaceName interface name
	InterfaceName = "com.aos.servicemanager.vis"
)

const intro = `
<node>
	<interface name="com.aos.servicemanager.vis">
		<annotation name="org.gtk.GDBus.DocString" value="Returns VIS client permission"/>
		<method name="GetPermissions">
			<arg name="token" direction="in" type="s">
				<annotation name="org.gtk.GDBus.DocString"
					value="VIS client token (service id)"/>
			</arg>
			<arg name="permissions" direction="out" type="s">
				<annotation name="org.gtk.GDBus.DocString"
					value="VIS client permissions"/>
			</arg>
			<arg name="status" direction="out" type="s">
				<annotation name="org.gtk.GDBus.DocString"
					value="Status of getting VIS permissions: OK or error"/>
			</arg>
		</method>
	</interface>` + introspect.IntrospectDataString + `</node> `

/*******************************************************************************
 * Types
 ******************************************************************************/

// ServiceProvider provides service info
type ServiceProvider interface {
	GetService(serviceID string) (service launcher.Service, err error)
}

// DBusHandler d-bus interface structure
type DBusHandler struct {
	serviceProvider ServiceProvider
	dbusConn        *dbus.Conn
}

/*******************************************************************************
 * Public
 ******************************************************************************/

// New creates and launch d-bus server
func New(serviceProvider ServiceProvider) (dbusHandler *DBusHandler, err error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return dbusHandler, err
	}

	reply, err := conn.RequestName(InterfaceName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return dbusHandler, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return dbusHandler, errors.New("D-Bus name already taken")
	}

	log.Debug("Start D-Bus server")

	server := DBusHandler{dbusConn: conn, serviceProvider: serviceProvider}

	conn.Export(server, ObjectPath, InterfaceName)
	conn.Export(introspect.Introspectable(intro), ObjectPath,
		"org.freedesktop.DBus.Introspectable")

	dbusHandler = &server

	return dbusHandler, nil
}

// Close closes d-bus server
func (dbusHandler *DBusHandler) Close() (err error) {
	log.Debug("Close D-Bus server")

	reply, err := dbusHandler.dbusConn.ReleaseName(InterfaceName)
	if err != nil {
		return err
	}
	if reply != dbus.ReleaseNameReplyReleased {
		return errors.New("can't release D-Bus interface name")
	}

	return nil
}

/*******************************************************************************
 * D-BUS interface
 ******************************************************************************/

// GetPermission get permossion d-bus method
func (dbusHandler DBusHandler) GetPermission(token string) (result, status string, dbusErr *dbus.Error) {
	service, err := dbusHandler.serviceProvider.GetService(token)
	if err != nil {
		return "", err.Error(), nil
	}

	log.WithFields(log.Fields{"token": token, "perm": service.Permissions}).Debug("Get permissions")

	return service.Permissions, "OK", nil
}