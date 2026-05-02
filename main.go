package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	TankEmptyDistanceCm = 32.5 // 32.5 cm -> 0 L
	TankFullDistanceCm  = 20.5 // 20.5 cm -> 12 L
	TankMaxVolumeLiters = 12.0
)

type TuyaMetricGroup int

const (
	TuyaGroupLevel TuyaMetricGroup = iota
	TuyaGroupPressure
	TuyaGroupAll
)

type LevelMessage struct {
	Reason     string   `json:"reason"`
	UptimeMs   uint64   `json:"uptime_ms"`
	DistanceCm *float64 `json:"distance_cm"`
}

type PressureMessage struct {
	Reason      string   `json:"reason"`
	UptimeMs    uint64   `json:"uptime_ms"`
	PressureMPa *float64 `json:"pressure_mpa"`
	PressureV   *float64 `json:"pressure_v"`
}

type TankState struct {
	DistanceCm   *float64
	VolumeLiters *float64
	LevelPercent *float64
	PressureMPa  *float64
	PressureV    *float64
}

var (
	tuyaClient   mqtt.Client
	tuyaDeviceID string
	stateMu      sync.Mutex
	tankState    TankState

	distanceCmGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "water_tank_distance_cm",
		Help: "Measured distance from the sensor to the water surface in centimeters.",
	})
	volumeLitersGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "water_tank_volume_liters",
		Help: "Calculated water volume in liters.",
	})
	levelPercentGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "water_tank_level_percent",
		Help: "Calculated water level percentage.",
	})
	pressureMPaGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "water_tank_pressure_mpa",
		Help: "Measured pressure in megapascals.",
	})
	pressureVoltageGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "water_tank_pressure_voltage_volts",
		Help: "Measured pressure sensor voltage in volts.",
	})
	sensorUptimeGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "water_tank_sensor_uptime_ms",
		Help: "Sensor-reported uptime in milliseconds.",
	}, []string{"kind"})
	lastMessageTimestampGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "water_tank_last_message_timestamp_seconds",
		Help: "Unix timestamp when the reader received the last sensor message.",
	}, []string{"kind"})
)

func init() {
	prometheus.MustRegister(
		distanceCmGauge,
		volumeLitersGauge,
		levelPercentGauge,
		pressureMPaGauge,
		pressureVoltageGauge,
		sensorUptimeGauge,
		lastMessageTimestampGauge,
	)
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
	metricsAddr := getEnv("METRICS_ADDR", ":2112")

	tuyaDeviceID = getEnv("TUYA_DEVICE_ID", "")
	tuyaDeviceSecret := getEnv("TUYA_DEVICE_SECRET", "")
	tuyaHost := getEnv("TUYA_HOST", "m1.tuyacn.com")
	tuyaPort := getEnvInt("TUYA_PORT", 8883)

	clientID := fmt.Sprintf("go-water-tank-reader-%d", time.Now().UnixNano())

	log.Printf("config: broker=%s", broker)
	log.Printf("config: topicLevel=%s", topicLevel)
	log.Printf("config: topicPressure=%s", topicPressure)
	log.Printf("config: metricsAddr=%s", metricsAddr)
	log.Printf("config: username_set=%t password_set=%t", username != "", password != "")
	log.Printf("tuya: host=%s port=%d deviceId_set=%t secret_set=%t",
		tuyaHost, tuyaPort, tuyaDeviceID != "", tuyaDeviceSecret != "")
	log.Printf("calibration: empty=%.1f cm full=%.1f cm max=%.1f L",
		TankEmptyDistanceCm, TankFullDistanceCm, TankMaxVolumeLiters)

	metricsServer := startMetricsServer(metricsAddr)

	if tuyaDeviceID != "" && tuyaDeviceSecret != "" {
		tuyaClient = newTuyaClient(tuyaDeviceID, tuyaDeviceSecret, tuyaHost, tuyaPort)
	} else {
		log.Println("[TUYA] disabled: TUYA_DEVICE_ID or TUYA_DEVICE_SECRET is empty")
	}

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetConnectTimeout(10 * time.Second).
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
	metricsServer.Close()

	if tuyaClient != nil && tuyaClient.IsConnected() {
		tuyaClient.Disconnect(250)
	}
}

func startMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("metrics server listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics server error: %v", err)
		}
	}()

	return server
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

	stateMu.Lock()
	tankState.DistanceCm = cloneFloatPtr(payload.DistanceCm)
	tankState.VolumeLiters = cloneFloatPtr(volumeLiters)
	tankState.LevelPercent = cloneFloatPtr(levelPercent)
	snapshot := copyStateLocked()
	stateMu.Unlock()

	updateLevelMetrics(payload, volumeLiters, levelPercent)
	publishToTuya(snapshot, TuyaGroupLevel)
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

	stateMu.Lock()
	tankState.PressureMPa = cloneFloatPtr(payload.PressureMPa)
	tankState.PressureV = cloneFloatPtr(payload.PressureV)
	snapshot := copyStateLocked()
	stateMu.Unlock()

	updatePressureMetrics(payload)
	publishToTuya(snapshot, TuyaGroupPressure)
}

func updateLevelMetrics(payload LevelMessage, volumeLiters, levelPercent *float64) {
	if payload.DistanceCm != nil {
		distanceCmGauge.Set(*payload.DistanceCm)
	}
	if volumeLiters != nil {
		volumeLitersGauge.Set(*volumeLiters)
	}
	if levelPercent != nil {
		levelPercentGauge.Set(*levelPercent)
	}

	sensorUptimeGauge.WithLabelValues("level").Set(float64(payload.UptimeMs))
	lastMessageTimestampGauge.WithLabelValues("level").Set(float64(time.Now().Unix()))
}

func updatePressureMetrics(payload PressureMessage) {
	if payload.PressureMPa != nil {
		pressureMPaGauge.Set(*payload.PressureMPa)
	}
	if payload.PressureV != nil {
		pressureVoltageGauge.Set(*payload.PressureV)
	}

	sensorUptimeGauge.WithLabelValues("pressure").Set(float64(payload.UptimeMs))
	lastMessageTimestampGauge.WithLabelValues("pressure").Set(float64(time.Now().Unix()))
}

func newTuyaClient(deviceID, deviceSecret, host string, port int) mqtt.Client {
	ts := time.Now().Unix()

	clientID := "tuyalink_" + deviceID
	username := fmt.Sprintf(
		"%s|signMethod=hmacSha256,timestamp=%d,secureMode=1,accessType=1",
		deviceID, ts,
	)
	content := fmt.Sprintf(
		"deviceId=%s,timestamp=%d,secureMode=1,accessType=1",
		deviceID, ts,
	)
	password := hmacSha256Hex(deviceSecret, content)

	log.Println("[TUYA] Connecting...")
	log.Printf("[TUYA] host=%s port=%d", host, port)
	log.Printf("[TUYA] clientId=%s", clientID)
	log.Printf("[TUYA] username=%s", username)
	log.Printf("[TUYA] content=%s", content)
	log.Printf("[TUYA] password=%s", password)

	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("ssl://%s:%d", host, port)).
		SetClientID(clientID).
		SetUsername(username).
		SetPassword(password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetConnectTimeout(15 * time.Second).
		SetKeepAlive(60 * time.Second).
		SetPingTimeout(10 * time.Second).
		SetOrderMatters(false)

	opts.SetTLSConfig(&tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	})

	opts.OnConnect = func(c mqtt.Client) {
		log.Println("[TUYA] Connected OK")
	}

	opts.OnConnectionLost = func(c mqtt.Client, err error) {
		log.Printf("[TUYA] connection lost: %v", err)
	}

	opts.OnReconnecting = func(c mqtt.Client, opts *mqtt.ClientOptions) {
		log.Println("[TUYA] reconnecting...")
	}

	client := mqtt.NewClient(opts)

	token := client.Connect()
	token.Wait()
	if token.Error() != nil {
		log.Printf("[TUYA] connect error: %v", token.Error())
	}

	return client
}

func publishToTuya(st TankState, group TuyaMetricGroup) bool {
	if tuyaClient == nil || !tuyaClient.IsConnected() {
		log.Println("[TUYA] publish skipped (not connected)")
		return false
	}

	topic := "tylink/" + tuyaDeviceID + "/thing/property/report"

	data := map[string]interface{}{}

	switch group {
	case TuyaGroupLevel:
		if st.VolumeLiters != nil {
			data["volume_liters"] = toScaledInt(*st.VolumeLiters, 2)
		}
		if st.LevelPercent != nil {
			data["level_percent"] = toScaledInt(*st.LevelPercent, 1)
		}
		//if st.DistanceCm != nil {
		//	data["distance_cm"] = toScaledInt(*st.DistanceCm, 1)
		//}

	case TuyaGroupPressure:
		if st.PressureMPa != nil {
			data["pressure_mpa"] = toScaledInt(*st.PressureMPa, 3)
		}
		//if st.PressureV != nil {
		//	data["pressure_v"] = toScaledInt(*st.PressureV, 3)
		//}

	case TuyaGroupAll:
		if st.PressureMPa != nil {
			data["pressure_mpa"] = toScaledInt(*st.PressureMPa, 3)
		}
		if st.VolumeLiters != nil {
			data["volume_liters"] = toScaledInt(*st.VolumeLiters, 2)
		}
		if st.LevelPercent != nil {
			data["level_percent"] = toScaledInt(*st.LevelPercent, 1)
		}
		//if st.DistanceCm != nil {
		//	data["distance_cm"] = toScaledInt(*st.DistanceCm, 1)
		//}
		//if st.PressureV != nil {
		//	data["pressure_v"] = toScaledInt(*st.PressureV, 3)
		//}
	}

	if len(data) == 0 {
		log.Println("[TUYA] publish skipped (empty payload)")
		return false
	}

	doc := map[string]interface{}{
		"msgId": fmt.Sprintf("%d", time.Now().UnixNano()),
		"time":  time.Now().Unix(),
		"data":  data,
	}

	body, err := json.Marshal(doc)
	if err != nil {
		log.Printf("[TUYA] marshal error: %v", err)
		return false
	}

	log.Printf("[TUYA] topic=%s", topic)
	log.Printf("[TUYA] payload=%s", string(body))

	token := tuyaClient.Publish(topic, 0, false, body)
	token.Wait()
	if token.Error() != nil {
		log.Printf("[TUYA] publish error: %v", token.Error())
		return false
	}

	log.Println("[TUYA] publish ok")
	return true
}

func hmacSha256Hex(key, data string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func cloneFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func copyStateLocked() TankState {
	return TankState{
		DistanceCm:   cloneFloatPtr(tankState.DistanceCm),
		VolumeLiters: cloneFloatPtr(tankState.VolumeLiters),
		LevelPercent: cloneFloatPtr(tankState.LevelPercent),
		PressureMPa:  cloneFloatPtr(tankState.PressureMPa),
		PressureV:    cloneFloatPtr(tankState.PressureV),
	}
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

func toScaledInt(v float64, scale int) int {
	pow := 1.0
	for i := 0; i < scale; i++ {
		pow *= 10
	}
	return int(v*pow + 0.5)
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

func getEnvInt(name string, fallback int) int {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("warning: invalid int in %s=%q, using fallback=%d", name, v, fallback)
		return fallback
	}

	return n
}
