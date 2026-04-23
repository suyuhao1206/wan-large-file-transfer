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

if [[ -d "nginx" ]]; then
  echo "Notice: Please make sure nginx domain, certificate, and key files are configured correctly before production deployment."
fi

echo "Stopping existing containers..."
$COMPOSE_CMD down

echo "Building and starting services..."
$COMPOSE_CMD up -d --build

echo "Deployment complete."
echo "Frontend: http://localhost"
echo "MinIO Console: http://localhost:9001"
