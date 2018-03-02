package amqphandler

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"time"

	"github.com/streadway/amqp"
)

const DEFAULT_CONFIG_FILE = "/etc/demo-application/demo_config.json"

type RequestStruct struct {
	User          string `json:"userName"`
	ApplianceId   string `json:"applianceId"`
	SendTimeStamp string `json:"SendTimeStamp"`
}

type PackageInfo struct {
	Name        string
	Version     string
	DownloadUrl string
}
type FileProperties struct {
	Name          string
	DownloadUrl   string
	Version       string
	TimeStamp     string
	ResponseCount int
}

type Configuration struct {
	Amqp_host   string
	Amqp_path   string
	Amqp_user   string
	Amqp_pass   string
	Root_cert   string
	Client_cert string
	Client_key  string
	Server_name string

	Ws_host          string
	Ws_path_download string
	Ws_path_update   string

	Rbt_device_queue_name string
	Rbt_auth_queue_name   string

	Usr_username     string
	Usr_appliance_id string
}

var configuration Configuration

var amqpChan chan PackageInfo

func sendRequestDownload(body string) {

	//todo send to channel

}

func sendRequestUpdate(body string) {

	//todo send to chnnel

}

func get_list_TLS() {
	var err error

	cfg := new(tls.Config)

	// see at the top
	cfg.RootCAs = x509.NewCertPool()

	if ca, err := ioutil.ReadFile(configuration.Root_cert); err == nil {
		log.Printf("append sert %v", configuration.Root_cert)
		cfg.RootCAs.AppendCertsFromPEM(ca)
	} else {
		log.Println("Fail AppendCertsFromPEM ", err)
		return
	}

	// Move the client cert and key to a location specific to your application
	// and load them here.
	log.Printf("OK, load %v %v %v", configuration.Root_cert, configuration.Client_cert, configuration.Client_key)

	if cert, err := tls.LoadX509KeyPair(configuration.Client_cert, configuration.Client_key); err == nil {
		log.Printf("LoadX509KeyPair")
		cfg.Certificates = append(cfg.Certificates, cert)
	} else {
		log.Printf("Fail LoadX509KeyPair :", err)
		return
	}

	cfg.ServerName = configuration.Server_name

	// see a note about Common Name (CN) at the top

	useerINFO := url.UserPassword(configuration.Amqp_user, configuration.Amqp_pass)
	urlRabbitMQ := url.URL{Scheme: "amqps", User: useerINFO, Host: configuration.Amqp_host, Path: configuration.Amqp_path}

	log.Printf("urlRabbitMQ: %v", urlRabbitMQ)
	//conn, err := amqp.DialTLS("amqps://demo_1:FusionSecurePass123@23.97.205.32:5671/", cfg)
	//conn, err := amqp.DialTLS(urlRabbitMQ.String(), cfg)

	conn, err := amqp.DialConfig(urlRabbitMQ.String(), amqp.Config{
		Heartbeat:       60,
		TLSClientConfig: cfg,
		Locale:          "en_US",
	})
	if err != nil {
		log.Println("Fail DialTLS: ", err)
		return
	}
	defer conn.Close()

	go func() {
		log.Printf("closing: %s \n", <-conn.NotifyClose(make(chan *amqp.Error)))
	}()

	log.Printf("connection of %v", urlRabbitMQ.String())

	ch, err := conn.Channel()
	if err != nil {
		log.Println("Failed to open a channel: ", err)
		return
	}
	defer ch.Close()

	q, err := ch.QueueDeclare(
		configuration.Rbt_device_queue_name, // name
		true,  // durable
		true,  // delete when unused
		false, // exclusive
		false, // noWait
		nil,   // arguments
	)
	if err != nil {
		log.Println("Failed to declare a queue: ", err)
		return
	}

	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		true,   // auto-ack
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)

	if err != nil {
		log.Println("Failed to register a consumer: ", err)
		return
	}

	corrId := "42"

	sendTime := time.Now().UTC()
	result := &RequestStruct{
		User:          configuration.Usr_username,
		ApplianceId:   configuration.Usr_appliance_id,
		SendTimeStamp: sendTime.Format(time.RFC3339), //"2017-12-11T07:59:34.437420", ,
	}
	body_JSON, _ := json.Marshal(result)
	log.Printf("send request %s", string(body_JSON))

	err = ch.Publish(
		"", // exchange
		configuration.Rbt_auth_queue_name, // routing key
		false, // mandatory
		false, // immediate
		amqp.Publishing{
			ContentType:   "application/json",
			DeliveryMode:  2,
			CorrelationId: corrId,
			ReplyTo:       q.Name,
			Body:          []byte(body_JSON),
		})

	if err != nil {
		log.Println("Failed to publish a message: ", err)
		return
	}

	for d := range msgs {
		log.Printf(" rseice message cor id %s \n", d.CorrelationId)
		log.Printf(string(d.Body))
		log.Printf("\n")
		if corrId == d.CorrelationId {
			log.Printf("sendRequestDownload \n")
			sendRequestDownload(string(d.Body))
		} else {

			log.Printf("sendRequestUpdate \n")
			sendRequestUpdate(string(d.Body))
		}

	}
	log.Printf(" \n END ")
}

func InitAmqphandler(outputchan chan PackageInfo) {

	amqpChan = outputchan

	config := DEFAULT_CONFIG_FILE

	file, err2 := os.Open(config)
	if err2 != nil {
		log.Println(" Config open  err: \n", err2)
		return
	}

	decoder := json.NewDecoder(file)
	err := decoder.Decode(&configuration)
	if err != nil {
		log.Println(" Decode error:", err)
		return
	}

	log.Printf(" ws host %v \n", configuration.Client_cert)
	log.Printf(" ws host %v \n", configuration.Root_cert)
	log.Printf(" ws host %v \n", configuration.Ws_path_download)

	//str := ("[{\"Name\":\"Demo application\",\"DownloadUrl\":\"https://fusionpoc1storage.blob.core.windows.net/images/Demo_application_1.2_demo_1.txt\",\"Version\":\"1.2\",\"TimeStamp\":\"2017-12-14T17:12:58.1443792Z\",\"ResponseCount\":0}]")
	//str2 := ("{\"Name\":\"Demo application\",\"DownloadUrl\":\"https://fusionpoc1storage.blob.core.windows.net/images/Demo_application_1.2_demo_1.txt\",\"Version\":\"1.2\",\"TimeStamp\":\"2017-12-14T17:12:58.1443792Z\",\"ResponseCount\":0}")
	//dec := json.NewDecoder(strings.NewReader(str))

	for {
		get_list_TLS()
		time.Sleep(5 * time.Second)
		log.Println("")
	}

	log.Printf(" [.] Got ")

}
