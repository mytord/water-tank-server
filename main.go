package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"
)

const (
	// TankEmptyDistanceCm Calibration:
	// 32.5 cm -> 0 L
	// 20.5 cm -> 12 L
	TankEmptyDistanceCm = 32.5
	TankFullDistanceCm  = 20.5
	TankMaxVolumeLiters = 12.0
)

type LevelMessage struct {
	Reason       string   `json:"reason"`
	UptimeMs     uint64   `json:"uptime_ms"`
	DistanceCm   *float64 `json:"distance_cm"`
	VolumeLiters *float64 `json:"volume_liters,omitempty"`
	LevelPercent *float64 `json:"level_percent,omitempty"`
}

type PressureMessage struct {
	Reason      string   `json:"reason"`
	UptimeMs    uint64   `json:"uptime_ms"`
	PressureMPa *float64 `json:"pressure_mpa"`
	PressureV   *float64 `json:"pressure_v"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if err := godotenv.Load(); err != nil {
		log.Printf("warning: .env file not loaded: %v", err)
	} else {
		log.Println(".env loaded successfully")
	}

	log.Println("=== starting water tank MQTT reader ===")

	broker := getEnv("MQTT_BROKER", "tcp://localhost:1883")
	username := os.Getenv("MQTT_USER")
	password := os.Getenv("MQTT_PASSWORD")
	topicLevel := getEnv("MQTT_TOPIC_LEVEL", "water_tank/level")
	topicPressure := getEnv("MQTT_TOPIC_PRESSURE", "water_tank/pressure")

	clientID := fmt.Sprintf("go-water-tank-reader-%d", time.Now().UnixNano())

	log.Printf("config: broker=%s", broker)
	log.Printf("config: topicLevel=%s", topicLevel)
	log.Printf("config: topicPressure=%s", topicPressure)
	log.Printf("config: username_set=%t password_set=%t", username != "", password != "")

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetKeepAlive(30 * time.Second).
		SetPingTimeout(10 * time.Second).
		SetOrderMatters(false)

	if username != "" {
		opts.SetUsername(username)
		opts.SetPassword(password)
	}

	opts.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		log.Printf("[UNHANDLED] topic=%s payload=%s", msg.Topic(), string(msg.Payload()))
	})

	opts.OnConnect = func(c mqtt.Client) {
		log.Println("MQTT connected")

		if token := c.Subscribe(topicLevel, 0, handleLevelMessage); token.Wait() && token.Error() != nil {
			log.Printf("subscribe error (%s): %v", topicLevel, token.Error())
		} else {
			log.Printf("subscribed: %s", topicLevel)
		}

		if token := c.Subscribe(topicPressure, 0, handlePressureMessage); token.Wait() && token.Error() != nil {
			log.Printf("subscribe error (%s): %v", topicPressure, token.Error())
		} else {
			log.Printf("subscribed: %s", topicPressure)
		}
	}

	opts.OnConnectionLost = func(c mqtt.Client, err error) {
		log.Printf("MQTT connection lost: %v", err)
	}

	opts.OnReconnecting = func(c mqtt.Client, opts *mqtt.ClientOptions) {
		log.Println("MQTT reconnecting...")
	}

	client := mqtt.NewClient(opts)

	log.Println("connecting to MQTT...")
	token := client.Connect()
	token.Wait()

	if token.Error() != nil {
		log.Fatalf("MQTT connect error: %v", token.Error())
	}

	log.Println("MQTT connect() successful")
	log.Println("waiting for messages...")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("shutting down...")
	client.Disconnect(250)
}

func handleLevelMessage(_ mqtt.Client, msg mqtt.Message) {
	log.Printf("[RAW LEVEL] topic=%s payload=%s", msg.Topic(), string(msg.Payload()))

	var payload LevelMessage
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		log.Printf("level json parse error: %v", err)
		return
	}

	distance := "null"
	if payload.DistanceCm != nil {
		distance = fmt.Sprintf("%.1f cm", *payload.DistanceCm)
	}

	volumeLiters, levelPercent := calculateVolumeAndPercent(payload.DistanceCm)

	volume := "null"
	if volumeLiters != nil {
		volume = fmt.Sprintf("%.2f L", *volumeLiters)
	}

	percent := "null"
	if levelPercent != nil {
		percent = fmt.Sprintf("%.1f%%", *levelPercent)
	}

	log.Printf(
		"[LEVEL] reason=%s uptime_ms=%d distance=%s volume=%s level=%s",
		payload.Reason,
		payload.UptimeMs,
		distance,
		volume,
		percent,
	)
}

func handlePressureMessage(_ mqtt.Client, msg mqtt.Message) {
	log.Printf("[RAW PRESSURE] topic=%s payload=%s", msg.Topic(), string(msg.Payload()))

	var payload PressureMessage
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		log.Printf("pressure json parse error: %v", err)
		return
	}

	pressureMPa := "null"
	if payload.PressureMPa != nil {
		pressureMPa = fmt.Sprintf("%.3f MPa", *payload.PressureMPa)
	}

	pressureV := "null"
	if payload.PressureV != nil {
		pressureV = fmt.Sprintf("%.3f V", *payload.PressureV)
	}

	log.Printf(
		"[PRESSURE] reason=%s uptime_ms=%d pressure=%s voltage=%s",
		payload.Reason,
		payload.UptimeMs,
		pressureMPa,
		pressureV,
	)
}

func clamp(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func calculateVolumeAndPercent(distanceCm *float64) (*float64, *float64) {
	if distanceCm == nil {
		return nil, nil
	}

	percent := (TankEmptyDistanceCm - *distanceCm) / (TankEmptyDistanceCm - TankFullDistanceCm) * 100.0
	percent = clamp(percent, 0, 100)

	volume := TankMaxVolumeLiters * percent / 100.0

	return &volume, &percent
}

func getEnv(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
