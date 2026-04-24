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

ENV_FILE="${ENV_FILE:-.env.production}"
COMPOSE_ENV_FILE=""
if [[ -f "$ENV_FILE" ]]; then
  COMPOSE_ENV_FILE="$ENV_FILE"
  echo "Using compose env file: $COMPOSE_ENV_FILE"
else
  echo "Notice: $ENV_FILE not found. Compose will use shell environment variables and defaults."
fi

compose() {
  if [[ -n "$COMPOSE_ENV_FILE" ]]; then
    $COMPOSE_CMD --env-file "$COMPOSE_ENV_FILE" "$@"
  else
    $COMPOSE_CMD "$@"
  fi
}

ENV_FILE_DATA_ROOT=""
if [[ -n "$COMPOSE_ENV_FILE" ]]; then
  ENV_FILE_DATA_ROOT="$(grep -E '^DATA_ROOT=' "$COMPOSE_ENV_FILE" | tail -n 1 | cut -d '=' -f 2- || true)"
  ENV_FILE_DATA_ROOT="${ENV_FILE_DATA_ROOT%\"}"
  ENV_FILE_DATA_ROOT="${ENV_FILE_DATA_ROOT#\"}"
  ENV_FILE_DATA_ROOT="${ENV_FILE_DATA_ROOT%\'}"
  ENV_FILE_DATA_ROOT="${ENV_FILE_DATA_ROOT#\'}"
fi

DATA_ROOT="${DATA_ROOT:-${ENV_FILE_DATA_ROOT:-/data}}"
echo "Data root: $DATA_ROOT"

if [[ -d "nginx" ]]; then
  echo "Notice: Please make sure nginx domain, certificate, and key files are configured correctly before production deployment."
fi

ensure_data_dirs() {
  local postgres_dir="$DATA_ROOT/postgres"
  local minio_dir="$DATA_ROOT/minio"

  echo "Ensuring data directories exist:"
  echo "  $postgres_dir"
  echo "  $minio_dir"

  if mkdir -p "$postgres_dir" "$minio_dir" 2>/dev/null; then
    return
  fi

  if command -v sudo >/dev/null 2>&1; then
    echo "Normal mkdir failed. Trying with sudo..."
    sudo mkdir -p "$postgres_dir" "$minio_dir"
    return
  fi

  echo "Error: failed to create data directories under $DATA_ROOT"
  echo "Please create them manually, for example:"
  echo "  sudo mkdir -p $postgres_dir $minio_dir"
  exit 1
}

ensure_data_dirs

echo "Stopping existing containers..."
compose down

echo "Building and starting services..."
compose up -d --build

echo "Deployment complete."
echo "Frontend: http://localhost"
echo "MinIO Console: http://localhost:9001"
