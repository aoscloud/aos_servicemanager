package main

import (
	"flag"
	"os"
	"os/signal"
	"path"
	"reflect"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	amqp "gitpct.epam.com/epmd-aepr/aos_servicemanager/amqphandler"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/config"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/database"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/dbushandler"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/fcrypt"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/launcher"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/monitoring"
	"gitpct.epam.com/epmd-aepr/aos_servicemanager/visclient"
)

const (
	reconnectTimeout = 3 * time.Second
)

func init() {
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: false,
		TimestampFormat:  "2006-01-02 15:04:05.000",
		FullTimestamp:    true})
	log.SetOutput(os.Stdout)
}

func sendInitialSetup(amqpHandler *amqp.AmqpHandler, launcherHandler *launcher.Launcher) (err error) {
	initialList, err := launcherHandler.GetServicesInfo()
	if err != nil {
		log.Fatalf("Can't get services: %s", err)
	}

	if err = amqpHandler.SendInitialSetup(initialList); err != nil {
		return err
	}

	return nil
}

func processAmqpMessage(data interface{}, amqpHandler *amqp.AmqpHandler, launcherHandler *launcher.Launcher) (err error) {
	switch data := data.(type) {
	case []amqp.ServiceInfoFromCloud:
		log.WithField("len", len(data)).Info("Receive services info")

		currentList, err := launcherHandler.GetServicesInfo()
		if err != nil {
			log.Error("Error getting services info: ", err)
			return err
		}

		type serviceDesc struct {
			serviceInfo          *amqp.ServiceInfo
			serviceInfoFromCloud *amqp.ServiceInfoFromCloud
		}

		servicesMap := make(map[string]*serviceDesc)

		for _, serviceInfo := range currentList {
			servicesMap[serviceInfo.ID] = &serviceDesc{serviceInfo: &serviceInfo}
		}

		for _, serviceInfoFromCloud := range data {
			if _, ok := servicesMap[serviceInfoFromCloud.ID]; !ok {
				servicesMap[serviceInfoFromCloud.ID] = &serviceDesc{}
			}
			servicesMap[serviceInfoFromCloud.ID].serviceInfoFromCloud = &serviceInfoFromCloud
		}

		for _, service := range servicesMap {
			if service.serviceInfoFromCloud != nil && service.serviceInfo != nil {
				// Update
				if service.serviceInfoFromCloud.Version > service.serviceInfo.Version {
					log.WithFields(log.Fields{
						"id":   service.serviceInfo.ID,
						"from": service.serviceInfo.Version,
						"to":   service.serviceInfoFromCloud.Version}).Info("Update service")

					launcherHandler.InstallService(*service.serviceInfoFromCloud)
				}
			} else if service.serviceInfoFromCloud != nil {
				// Install
				log.WithFields(log.Fields{
					"id":      service.serviceInfoFromCloud.ID,
					"version": service.serviceInfoFromCloud.Version}).Info("Install service")

				launcherHandler.InstallService(*service.serviceInfoFromCloud)
			} else if service.serviceInfo != nil {
				// Remove
				log.WithFields(log.Fields{
					"id":      service.serviceInfo.ID,
					"version": service.serviceInfo.Version}).Info("Remove service")

				launcherHandler.RemoveService(service.serviceInfo.ID)
			}
		}

		return nil

	default:
		log.Warn("Receive unsupported amqp message: ", reflect.TypeOf(data))
		return nil
	}
}

func run(amqpHandler *amqp.AmqpHandler, launcherHandler *launcher.Launcher,
	visHandler *visclient.VisClient, monitorHandler *monitoring.Monitor) {
	if err := sendInitialSetup(amqpHandler, launcherHandler); err != nil {
		log.Errorf("Can't send initial setup: %s", err)
		// reconnect
		return
	}

	for {
		select {
		case amqpMessage := <-amqpHandler.MessageChannel:
			// check for error
			if err, ok := amqpMessage.(error); ok {
				log.Error("Receive amqp error: ", err)
				// reconnect
				return
			}

			if err := processAmqpMessage(amqpMessage, amqpHandler, launcherHandler); err != nil {
				log.Error("Error processing amqp result: ", err)
			}

		case serviceStatus := <-launcherHandler.StatusChannel:
			info := amqp.ServiceInfo{ID: serviceStatus.ID, Version: serviceStatus.Version}

			switch serviceStatus.Action {
			case launcher.ActionInstall:
				if serviceStatus.Err != nil {
					info.Status = "error"
					errorMsg := amqp.ServiceError{ID: -1, Message: "Can't install service"}
					info.Error = &errorMsg
					log.WithFields(log.Fields{"id": serviceStatus.ID, "version": serviceStatus.Version}).Error("Can't install service: ", serviceStatus.Err)
				} else {
					info.Status = "installed"
					log.WithFields(log.Fields{"id": serviceStatus.ID, "version": serviceStatus.Version}).Info("Service successfully installed")
				}

			case launcher.ActionRemove:
				if serviceStatus.Err != nil {
					info.Status = "error"
					errorMsg := amqp.ServiceError{ID: -1, Message: "Can't remove service"}
					info.Error = &errorMsg
					log.WithFields(log.Fields{"id": serviceStatus.ID, "version": serviceStatus.Version}).Error("Can't remove service: ", serviceStatus.Err)
				} else {
					info.Status = "removed"
					log.WithFields(log.Fields{"id": serviceStatus.ID, "version": serviceStatus.Version}).Info("Service successfully removed")
				}
			}

			err := amqpHandler.SendServiceStatusMsg(info)
			if err != nil {
				log.Error("Error send service status message: ", err)
			}

		case data := <-monitorHandler.DataChannel:
			err := amqpHandler.SendMonitoringData(data)
			if err != nil {
				log.Error("Error send monitoring data: ", err)
			}

		case users := <-visHandler.UsersChangedChannel:
			log.WithField("users", users).Info("Users changed")
			// reconnect
			return
		}
	}
}

func main() {
	// Initialize command line flags
	configFile := flag.String("c", "aos_servicemanager.cfg", "path to config file")
	strLogLevel := flag.String("v", "info", `log level: "debug", "info", "warn", "error", "fatal", "panic"`)

	flag.Parse()

	// Set log level
	logLevel, err := log.ParseLevel(*strLogLevel)
	if err != nil {
		log.Fatalf("Error: %s", err)
	}
	log.SetLevel(logLevel)

	log.WithField("configFile", *configFile).Info("Start service manager")

	// Create config
	config, err := config.New(*configFile)
	if err != nil {
		log.Fatal("Error while opening configuration file: ", err)
	}

	// Initialize fcrypt
	fcrypt.Init(config.Crypt)

	// Create DB
	db, err := database.New(path.Join(config.WorkingDir, "servicemanager.db"))
	if err != nil {
		log.Fatalf("Can't open database: %s", err)
	}
	defer db.Close()

	// Create amqp
	amqpHandler, err := amqp.New()
	if err != nil {
		log.Fatal("Can't create amqpHandler: ", err)
	}
	defer amqpHandler.Close()

	// Create monitor
	monitor, err := monitoring.New(config)
	if err != nil {
		if err == monitoring.ErrDisabled {
			log.Warn(err)
		} else {
			log.Fatal("Can't create monitor: ", err)
		}
	}

	// Create launcher
	launcherHandler, err := launcher.New(config, db, monitor)
	if err != nil {
		log.Fatalf("Can't create launcher: %s", err)
	}
	defer launcherHandler.Close()

	// Create D-Bus server
	dbusServer, err := dbushandler.New(db)
	if err != nil {
		log.Fatal("Can't create D-BUS server %v", err)
	}

	// Create VIS client
	vis, err := visclient.New(config.VISServerURL)
	if err != nil {
		log.Fatalf("Can't connect to VIS: %s", err)
	}
	defer vis.Close()

	// Handle SIGTERM
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		launcherHandler.Close()
		amqpHandler.Close()
		dbusServer.Close()
		vis.Close()
		monitor.Close()
		os.Exit(1)
	}()

	// Run all systems
	log.WithField("url", config.ServiceDiscoveryURL).Debug("Start connection")

	// Get vin code
	vin, err := vis.GetVIN()
	if err != nil {
		log.Fatalf("Can't get VIN: %s", err)
	}

	for {
		// Get vin code
		users, err := vis.GetUsers()
		if err != nil {
			log.Fatalf("Can't get users: %s", err)
		}

		err = launcherHandler.SetUsers(users)
		if err != nil {
			log.Fatalf("Can't set users: %s", err)
		}

		err = amqpHandler.InitAmqphandler(config.ServiceDiscoveryURL, vin, users)
		if err == nil {
			run(amqpHandler, launcherHandler, vis, monitor)
		} else {
			log.Error("Can't establish connection: ", err)

			time.Sleep(reconnectTimeout)
		}

		amqpHandler.Close()

		log.Debug("Reconnecting...")
	}
}
