#!/bin/bash

set -euo pipefail

echo "Deploying FileCodeBox TUS System..."

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if [[ ! -f "docker-compose.yml" ]]; then
  echo "Error: docker-compose.yml not found in $SCRIPT_DIR"
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "Error: Docker is not installed."
  exit 1
fi

if ! docker info >/dev/null 2>&1; then
  echo "Error: Docker is not running or current user has no permission to access Docker."
  exit 1
fi

if docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE_CMD="docker-compose"
else
  echo "Error: Neither 'docker compose' nor 'docker-compose' is available."
  exit 1
fi

echo "Using compose command: $COMPOSE_CMD"
echo "Project directory: $SCRIPT_DIR"
DATA_ROOT="${DATA_ROOT:-/data}"
echo "Data root: $DATA_ROOT"

if [[ -d "nginx" ]]; then
  echo "Notice: Please make sure nginx domain, certificate, and key files are configured correctly before production deployment."
fi

if [[ ! -d "$DATA_ROOT" ]]; then
  echo "Error: DATA_ROOT directory does not exist: $DATA_ROOT"
  echo "Please mount your data disk first and create the directory, for example:"
  echo "  sudo mkdir -p $DATA_ROOT/postgres $DATA_ROOT/minio"
  exit 1
fi

mkdir -p "$DATA_ROOT/postgres" "$DATA_ROOT/minio"

echo "Stopping existing containers..."
$COMPOSE_CMD down

echo "Building and starting services..."
$COMPOSE_CMD up -d --build

echo "Deployment complete."
echo "Frontend: http://localhost"
echo "MinIO Console: http://localhost:9001"
