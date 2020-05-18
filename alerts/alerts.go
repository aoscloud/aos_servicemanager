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

// Package alerts provides set of API to send system and services alerts
package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/sdjournal"
	log "github.com/sirupsen/logrus"

	amqp "aos_servicemanager/amqphandler"
	"aos_servicemanager/config"
	"aos_servicemanager/database"
)

/*******************************************************************************
 * Consts
 ******************************************************************************/

const (
	alertsDataAllocSize = 10
)

/*******************************************************************************
 * Types
 ******************************************************************************/

// ServiceProvider provides service info
type ServiceProvider interface {
	GetService(serviceID string) (service database.ServiceEntry, err error)
	GetServiceByUnitName(unitName string) (service database.ServiceEntry, err error)
}

// CursorStorage provides API to set and get journal cursor
type CursorStorage interface {
	SetJournalCursor(cursor string) (err error)
	GetJournalCursor() (cursor string, err error)
}

// Alerts instance
type Alerts struct {
	AlertsChannel chan amqp.Alerts

	config          config.Alerts
	cursorStorage   CursorStorage
	serviceProvider ServiceProvider
	filterRegexp    []*regexp.Regexp

	alertsSize       int
	skippedAlerts    uint32
	duplicatedAlerts uint32
	alerts           amqp.Alerts

	sync.Mutex

	journal      *sdjournal.Journal
	cursor       string
	ticker       *time.Ticker
	closeChannel chan bool
}

/*******************************************************************************
 * Variable
 ******************************************************************************/

// ErrDisabled indicates that alerts is disable in the config
var ErrDisabled = errors.New("alerts is disabled")

/*******************************************************************************
 * Public
 ******************************************************************************/

// New creates new alerts object
func New(config *config.Config,
	serviceProvider ServiceProvider,
	cursorStorage CursorStorage) (instance *Alerts, err error) {
	log.Debug("New alerts")

	if config.Alerts.Disabled {
		return nil, ErrDisabled
	}

	instance = &Alerts{config: config.Alerts, cursorStorage: cursorStorage, serviceProvider: serviceProvider}

	instance.AlertsChannel = make(chan amqp.Alerts, instance.config.MaxOfflineMessages)
	instance.closeChannel = make(chan bool)

	instance.ticker = time.NewTicker(instance.config.SendPeriod.Duration)

	instance.alerts = make([]amqp.AlertItem, 0, alertsDataAllocSize)

	for _, substr := range instance.config.Filter {
		if len(substr) == 0 {
			log.Warning("Filter value has an empty string")
			continue
		}

		tmpRegexp, err := regexp.Compile(substr)
		if err != nil {
			log.Errorf("Regexp compile error. Incorrect regexp: %s, error is: %s", substr, err)
			continue
		}

		instance.filterRegexp = append(instance.filterRegexp, tmpRegexp)
	}

	if err = instance.setupJournal(); err != nil {
		return nil, err
	}

	log.AddHook(instance)

	return instance, nil
}

// Close closes logging
func (instance *Alerts) Close() {
	log.Debug("Close Alerts")

	instance.closeChannel <- true

	instance.ticker.Stop()

	instance.journal.Close()
}

// SendResourceAlert sends resource alert
func (instance *Alerts) SendResourceAlert(source, resource string, time time.Time, value uint64) {
	log.WithFields(log.Fields{
		"timestamp": time,
		"source":    source,
		"resource":  resource,
		"value":     value}).Debug("Resource alert")

	var version *uint64

	if service, err := instance.serviceProvider.GetService(source); err == nil {
		version = &service.Version
	}

	instance.addAlert(amqp.AlertItem{
		Timestamp: time,
		Tag:       amqp.AlertTagResource,
		Source:    source,
		Version:   version,
		Payload: amqp.ResourceAlert{
			Parameter: resource,
			Value:     value}})
}

// Levels returns log levels which should be hooked (log Hook interface)
func (instance *Alerts) Levels() (levels []log.Level) {
	return []log.Level{log.ErrorLevel, log.FatalLevel, log.PanicLevel}
}

// Fire called to hook selected log (log hook interface)
func (instance *Alerts) Fire(entry *log.Entry) (err error) {
	message := entry.Message

	for field, value := range entry.Data {
		message = message + fmt.Sprintf(" %s=%v", field, value)
	}

	instance.addAlert(amqp.AlertItem{
		Timestamp: entry.Time,
		Tag:       amqp.AlertTagAosCore,
		Source:    "servicemanager",
		Payload:   amqp.SystemAlert{Message: message}})

	return nil
}

/*******************************************************************************
 * Private
 ******************************************************************************/

func (instance *Alerts) setupJournal() (err error) {
	if instance.journal, err = sdjournal.NewJournal(); err != nil {
		return err
	}

	if err = instance.journal.AddMatch("PRIORITY=0"); err != nil {
		return err
	}

	if err = instance.journal.AddMatch("PRIORITY=1"); err != nil {
		return err
	}

	if err = instance.journal.AddMatch("PRIORITY=2"); err != nil {
		return err
	}

	if err = instance.journal.AddMatch("PRIORITY=3"); err != nil {
		return err
	}

	if err = instance.journal.AddDisjunction(); err != nil {
		return err
	}

	if err = instance.journal.AddMatch("_SYSTEMD_UNIT=init.scope"); err != nil {
		return err
	}

	if err = instance.journal.SeekTail(); err != nil {
		return err
	}

	if _, err = instance.journal.Previous(); err != nil {
		return err
	}

	cursor, err := instance.cursorStorage.GetJournalCursor()
	if err != nil {
		return err
	}

	if cursor != "" {
		if err = instance.journal.SeekCursor(cursor); err != nil {
			return err
		}

		if _, err = instance.journal.Next(); err != nil {
			return err
		}

		instance.cursor = cursor
	}

	go func() {
		for {
			select {
			case <-instance.ticker.C:
				if err = instance.processJournal(); err != nil {
					log.Errorf("Journal process error: %s", err)
				}

				instance.sendAlerts()

			case <-instance.closeChannel:
				return
			}
		}
	}()

	return nil
}

func (instance *Alerts) processJournal() (err error) {
	currentCursor := instance.cursor

	for {
		count, err := instance.journal.Next()
		if err != nil {
			return err
		}

		if count == 0 {
			if currentCursor != instance.cursor {
				if err = instance.cursorStorage.SetJournalCursor(currentCursor); err != nil {
					return err
				}
			}

			return nil
		}

		entry, err := instance.journal.GetEntry()
		if err != nil {
			return err
		}

		currentCursor = entry.Cursor

		var version *uint64
		source := "system"

		if entry.Fields["_SYSTEMD_UNIT"] == "init.scope" {
			if priority, err := strconv.Atoi(entry.Fields["PRIORITY"]); err != nil || priority > 4 {
				continue
			}

			unit := entry.Fields["UNIT"]

			if strings.HasPrefix(unit, "aos") {
				service, err := instance.serviceProvider.GetServiceByUnitName(unit)
				if err != nil {
					continue
				}

				source = service.ID
				version = &service.Version
			}
		}

		t := time.Unix(int64(entry.RealtimeTimestamp/1000000),
			int64((entry.RealtimeTimestamp%1000000)*1000))

		log.WithFields(log.Fields{"time": t, "message": entry.Fields["MESSAGE"]}).Debug("System alert")

		skipsend := false

		for _, substr := range instance.filterRegexp {
			skipsend = substr.MatchString(entry.Fields["MESSAGE"])

			if skipsend {
				break
			}
		}

		if !skipsend {
			instance.addAlert(amqp.AlertItem{
				Timestamp: t,
				Tag:       amqp.AlertTagSystemError,
				Source:    source,
				Version:   version,
				Payload:   amqp.SystemAlert{Message: entry.Fields["MESSAGE"]}})
		}
	}
}

func (instance *Alerts) addAlert(item amqp.AlertItem) {
	instance.Lock()
	defer instance.Unlock()

	if len(instance.alerts) != 0 &&
		reflect.DeepEqual(instance.alerts[len(instance.alerts)-1].Payload, item.Payload) {
		instance.duplicatedAlerts++
		return
	}

	data, _ := json.Marshal(item)
	instance.alertsSize += len(data)

	if int(instance.alertsSize) <= instance.config.MaxMessageSize {
		instance.alerts = append(instance.alerts, item)
	} else {
		instance.skippedAlerts++
	}
}

func (instance *Alerts) sendAlerts() {
	instance.Lock()
	defer instance.Unlock()

	if instance.alertsSize != 0 {
		if len(instance.AlertsChannel) < cap(instance.AlertsChannel) {
			instance.AlertsChannel <- instance.alerts

			if instance.skippedAlerts != 0 {
				log.WithField("count", instance.skippedAlerts).Warn("Alerts skipped due to size limit")
			}
			if instance.duplicatedAlerts != 0 {
				log.WithField("count", instance.duplicatedAlerts).Warn("Alerts skipped due to duplication")
			}
		} else {
			log.Warn("Skip sending alerts due to channel is full")
		}

		instance.alerts = make([]amqp.AlertItem, 0, alertsDataAllocSize)
		instance.skippedAlerts = 0
		instance.duplicatedAlerts = 0
		instance.alertsSize = 0
	}
}
