#!/bin/bash
echo "Deploying FileCodeBox TUS System..."

# Check if Docker is running
if ! docker info > /dev/null 2>&1; then
  echo "Error: Docker is not running or not installed."
  exit 1
fi

# Pull latest images (if using external images)
# docker-compose pull

# Build and Start
docker-compose down
docker-compose up -d --build

echo "Deployment Complete."
echo "Frontend: http://localhost"
echo "MinIO Console: http://localhost:9001"
