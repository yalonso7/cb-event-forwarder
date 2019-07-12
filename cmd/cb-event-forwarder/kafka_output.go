package main

import (
	"encoding/json"
	"fmt"
	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/rcrowley/go-metrics"
	log "github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

type KafkaOutput struct {
	brokers           []string
	topicSuffix       string
	topic             string
	producers         []*kafka.Producer
	droppedEventCount int64
	eventSentCount    int64
	EventSent         metrics.Meter
	EventSentBytes    metrics.Meter
	DroppedEvent      metrics.Meter
	sync.RWMutex
}

type KafkaStatistics struct {
	DroppedEventCount int64 `json:"dropped_event_count"`
	EventSentCount    int64 `json:"event_sent_count"`
}

func (o *KafkaOutput) Initialize(unused string) error {
	o.Lock()
	defer o.Unlock()

	o.brokers = strings.Split(*(config.KafkaBrokers), ",")
	o.topicSuffix = config.KafkaTopicSuffix
	o.topic = config.KafkaTopic
	o.producers = make([]*kafka.Producer, len(o.brokers))

	o.EventSent = metrics.NewRegisteredMeter("kafka_events_sent", metrics.DefaultRegistry)
	o.DroppedEvent = metrics.NewRegisteredMeter("kafka_dropped_events", metrics.DefaultRegistry)
	o.EventSentBytes = metrics.NewRegisteredMeter("kafka_event_sent_Bytes", metrics.DefaultRegistry)

	var kafkaConfig kafka.ConfigMap = nil
	//PLAINTEXT, SSL, SASL_PLAINTEXT, SASL_SSL
	switch config.KafkaProtocol {
	case "PLAINTEXT":
		kafkaConfig = kafka.ConfigMap{"bootstrap.servers": *config.KafkaBrokers}
	case "SASL":
		kafkaConfig = kafka.ConfigMap{"bootstrap.servers": *config.KafkaBrokers,
			"security.protocol": config.KafkaProtocol,
			"sasl.mechanism":    config.KafkaMechanism,
			"sasl.username":     config.KafkaUsername,
			"sasl.password":     config.KafkaPassword}
	case "SASL+SSL":
		kafkaConfig = kafka.ConfigMap{"boostrap.servers": *config.KafkaBrokers,
			"security.protocol": config.KafkaProtocol,
			"sasl.mechanism":    config.KafkaMechanism,
			"sasl.username":     config.KafkaUsername,
			"sasl.password":     config.KafkaPassword}

		if config.KafkaSSLKeystoreLocation != nil && config.KafkaSSLKeystoreFilename != nil && config.KafkaSSLKeystorePassword != nil {
			kafkaConfig["ssl.keystore.location"] = *config.KafkaSSLKeystoreLocation
			kafkaConfig["ssl.keystore.filename"] = *config.KafkaSSLKeystoreFilename
			kafkaConfig["ssl.keystore.password"] = *config.KafkaSSLKeystorePassword
		}
		if config.KafkaSSLCAKeyFilename != nil && config.KafkaSSLCAKeyLocation != nil {
			kafkaConfig["ssl.ca.location"] = *config.KafkaSSLCAKeyLocation
			kafkaConfig["ssl.ca.filename"] = *config.KafkaSSLCAKeyFilename
		}
		if config.KafkaSSLKeyPassword != nil {
			kafkaConfig["ssl.key.password"] = *config.KafkaSSLKeyPassword
		}
		if len(config.KafkaSSLEnabledProtocols) > 0 {
			kafkaConfig["ssl.enabled.protocols"] = config.KafkaSSLEnabledProtocols
		}
	case "SSL":
		kafkaConfig = kafka.ConfigMap{"bootstrap.servers": *config.KafkaBrokers,
			"security.protocol": config.KafkaProtocol}
		/*if config.KafkaSSLTrustStoreLocation != nil {
			kafkaConfig["ssl.truststore.location"] = *config.KafkaSSLTrustStoreLocation
			kafkaConfig["ssl.truststore.password"] = *config.KafkaSSLTrustStorePassword
		}*/
		if config.KafkaSSLKeystoreLocation != nil && config.KafkaSSLKeystoreFilename != nil && config.KafkaSSLKeystorePassword != nil {
			kafkaConfig["ssl.keystore.location"] = *config.KafkaSSLKeystoreLocation
			kafkaConfig["ssl.keystore.filename"] = *config.KafkaSSLKeystoreFilename
			kafkaConfig["ssl.keystore.password"] = *config.KafkaSSLKeystorePassword
		}
		if config.KafkaSSLKeyPassword != nil {
			kafkaConfig["ssl.key.password"] = *config.KafkaSSLKeyPassword
		}
		if config.KafkaSSLCAKeyFilename != nil && config.KafkaSSLCAKeyLocation != nil {
			kafkaConfig["ssl.ca.location"] = *config.KafkaSSLCAKeyLocation
			kafkaConfig["ssl.ca.filename"] = *config.KafkaSSLCAKeyFilename
		}
		if len(config.KafkaSSLEnabledProtocols) > 0 {
			kafkaConfig["ssl.enabled.protocols"] = config.KafkaSSLEnabledProtocols
		}
	default:
		kafkaConfig = kafka.ConfigMap{"bootstrap.servers": *config.KafkaBrokers}
	}

	if config.KafkaCompressionType != nil {
		kafkaConfig["compression.type"] = *config.KafkaCompressionType
	}

	for index, _ := range o.brokers {
		p, err := kafka.NewProducer(&kafkaConfig)
		if err != nil {
			panic(err)
		}
		o.producers[index] = p
	}

	return nil
}

func (o *KafkaOutput) Go(messages <-chan string, errorChan chan<- error) error {

	joinEventsChan := make(chan (kafka.Event))
	sigs := make(chan os.Signal, 1)
	stopProdChans := make([]chan struct{}, len(o.producers))

	signal.Notify(sigs, syscall.SIGHUP)
	signal.Notify(sigs, syscall.SIGTERM)
	signal.Notify(sigs, syscall.SIGINT)

	defer signal.Stop(sigs)

	for workernum, producer := range o.producers {
		stopProdChans[workernum] = make(chan struct{}, 1)
		go func(workernum int32, producer *kafka.Producer, stopProdChan <-chan struct{}) {

			defer producer.Close()

			partition := kafka.PartitionAny
			shouldStop := false
			if (len(o.producers)) > 0 {
				partition = workernum
			}

			for {
				select {
				case message := <-messages:
					var topic string = config.KafkaTopic
					if topic == "" {
						var parsedMsg map[string]interface{}
						json.Unmarshal([]byte(message), &parsedMsg)
						topicRaw := parsedMsg["type"]
						if topicString, ok := topicRaw.(string); ok {
							topicString = strings.Replace(topicString, "ingress.event.", "", -1)
							topicString += o.topicSuffix
							topic = topicString
						} else {
							log.Info("ERROR: Topic was not a string")
						}
					}
					partition := kafka.TopicPartition{Topic: &topic, Partition: partition}
					output(message, o.producers[workernum], partition)
					o.EventSentBytes.Mark(int64(len(message)))
				case <-stopProdChan:
					shouldStop = true
				case e := <-producer.Events():
					joinEventsChan <- e
				default:
					if shouldStop {
						return
					}
				}
			}
		}(int32(workernum), producer, stopProdChans[workernum])
	}
	go func() {
		for {
			select {
			case e := <-joinEventsChan:
				m := e.(*kafka.Message)
				if m.TopicPartition.Error != nil {
					//log.Debugf("Delivery failed: %v\n", m.TopicPartition.Error)
					atomic.AddInt64(&o.droppedEventCount, 1)
					o.DroppedEvent.Mark(1)
					errorChan <- m.TopicPartition.Error
				} else {
					/*log.Debugf("Delivered message to topic %s [%d] at offset %v\n",
					*m.TopicPartition.Topic, m.TopicPartition.Partition, m.TopicPartition.Offset)*/
					atomic.AddInt64(&o.eventSentCount, 1)
					o.EventSent.Mark(1)
				}
			case sig := <-sigs:
				switch sig {
				case syscall.SIGTERM, syscall.SIGINT:
					for _, stopChan := range stopProdChans {
						stopChan <- struct{}{}
					}
					return
				default:
					log.Debugf("Signal was %s", sig)
				}
			}
		}
	}()
	return nil
}

func (o *KafkaOutput) Statistics() interface{} {
	o.RLock()
	defer o.RUnlock()

	return KafkaStatistics{DroppedEventCount: o.droppedEventCount, EventSentCount: o.eventSentCount}
}

func (o *KafkaOutput) String() string {
	o.RLock()
	defer o.RUnlock()

	return fmt.Sprintf("Brokers %s", o.brokers)
}

func (o *KafkaOutput) Key() string {
	o.RLock()
	defer o.RUnlock()

	return fmt.Sprintf("brokers:%s", o.brokers)
}

func output(m string, producer *kafka.Producer, partition kafka.TopicPartition) {
	kafkamsg := &kafka.Message{
		TopicPartition: partition,
		Value:          []byte(m),
	}
	for producer.Produce(kafkamsg, nil) != nil {
		producer.Flush(1)
	}

}
