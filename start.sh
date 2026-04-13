#!/usr/bin/env bash

set -e

IMAGE_NAME="water-tank-reader"
CONTAINER_NAME="water-tank-reader"
ENV_FILE=".env"
NETWORK_NAME="mqtt-net"
MQTT_CONTAINER_NAME="mosquitto"

echo "=== starting water tank reader ==="

# --- проверка .env ---
if [ ! -f "$ENV_FILE" ]; then
  echo "ERROR: $ENV_FILE not found"
  exit 1
fi

# --- создаём сеть (идемпотентно) ---
if ! docker network inspect "$NETWORK_NAME" >/dev/null 2>&1; then
  echo "Creating network: $NETWORK_NAME"
  docker network create "$NETWORK_NAME"
else
  echo "Network exists: $NETWORK_NAME"
fi

# --- подключаем mosquitto к сети (идемпотентно) ---
if docker ps -a --format '{{.Names}}' | grep -qx "$MQTT_CONTAINER_NAME"; then
  echo "Ensuring mosquitto is in network..."

  if ! docker inspect "$MQTT_CONTAINER_NAME" \
    --format '{{json .NetworkSettings.Networks}}' | grep -q "$NETWORK_NAME"; then

    echo "Connecting mosquitto to $NETWORK_NAME"
    docker network connect "$NETWORK_NAME" "$MQTT_CONTAINER_NAME"
  else
    echo "mosquitto already in network"
  fi
else
  echo "WARNING: mosquitto container not found"
fi

# --- билд образа ---
echo "Building Docker image..."
docker build -t "$IMAGE_NAME" .

# --- удаляем старый контейнер (идемпотентно) ---
if docker ps -aq -f name="^${CONTAINER_NAME}$" | grep -q .; then
  echo "Removing existing container..."
  docker rm -f "$CONTAINER_NAME"
fi

# --- запуск ---
echo "Running container..."
docker run -d \
  --name "$CONTAINER_NAME" \
  --env-file "$ENV_FILE" \
  --network "$NETWORK_NAME" \
  --restart unless-stopped \
  "$IMAGE_NAME"

echo "Container started: $CONTAINER_NAME"

# --- логи ---
echo "Showing logs..."
docker logs -f "$CONTAINER_NAME"