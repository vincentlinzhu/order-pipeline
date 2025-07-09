# Order-Pipeline

A simple event-driven microservices project built with Go, Redis, and RabbitMQ. This pipeline demonstrates how to handle HTTP requests, perform idempotency checks, process messages asynchronously, and maintain cache state—all with best practices for reliability and scalability.

Next Steps:
- Add a proper database like CassandraBD and use a cache scheme (aside, read back, write around, write through, etc) to interact with the redis cache.
- Add a network layer (an http server). Ask Wangster about this?
- Change docker compose to Kubernetes for more modern approach to this architecture.

## Table of Contents

* [System Architecture](#system-architecture)
* [Prerequisites](#prerequisites)
* [Repository Structure](#repository-structure)
* [Setup](#setup)
* [Running Locally](#running-locally)
* [Service Overview](#service-overview)

  * [API Gateway](#api-gateway)
  * [Order Processor](#order-processor)
  * [Notification Service](#notification-service)
  * [Dashboard (Optional)](#dashboard-optional)
* [Testing & CI](#testing--ci)
* [Deployment](#deployment)
* [Best Practices](#best-practices)
* [Extensions](#extensions)

## System Architecture

```text
+--------+         +-------------+         +------------------+         +----------------------+  
| Client |--HTTP-->| API Gateway |--AMQP-->| Order Processor  |--AMQP-->| Notification Service |  
+--------+         +-------------+         +------------------+         +----------------------+  
                         |                          |                                   
                         v                          v                                   
                     +----------+               +--------------+                              
                     | Redis    |               | Redis        |                              
                     | (idemp   |               |(inventory &  |                         
                     |  & cache)|               | notification)|                         
                     +----------+               +--------------+                              

Optional Dashboard Service reads metrics from Redis and serves via HTTP.
```

## Prerequisites

* Go 1.20+
* Docker & Docker Compose
* Git & GitHub/GitLab account

## Repository Structure

```
order-pipeline/
├── api-gateway/         # HTTP service accepting orders
├── order-processor/     # Consumes orders, updates inventory, publishes events
├── notifier/            # Sends notifications on order.created
├── dashboard/           # (Optional) Exposes metrics via HTTP
├── docker-compose.yml   # RabbitMQ & Redis setup
└── README.md            # Project overview & instructions
```

## Setup

1. Clone the repo and navigate into it:

   ```bash
   git clone git@github.com:your-org/order-pipeline.git
   cd order-pipeline
   ```
2. Start RabbitMQ & Redis:

   ```bash
   docker-compose up -d
   ```
3. Verify services:

   * RabbitMQ Management UI: [http://localhost:15672](http://localhost:15672) (guest/guest)
   * Redis: `redis-cli -h localhost ping` → `PONG`

## Running Locally

The entire application stack, including the Go services, can be started with a single command:

```bash
# This will build the Go services and start all containers.
docker-compose up --build
```

By default:
*   API Gateway is exposed on port **8080** on your local machine.
*   The RabbitMQ Management UI is available at [http://localhost:15672](http://localhost:15672) and [http://localhost:15673](http://localhost:15673).
*   All services communicate with each other over a dedicated Docker network.

### Interacting with the Redis Data Store

You can directly interact with the Redis container to inspect or modify data. First, find the container name:

```bash
# This will output the name, e.g., "order-pipeline-redis-1"
docker ps --filter "name=redis" --format "{{.Names}}"
```

Then, use the container name in the following commands.

**General Commands**
```bash
# List all keys in the database
docker exec <container-name> redis-cli KEYS "*"

# Check the data type of a key (e.g., "string", "hash")
docker exec <container-name> redis-cli TYPE inventory:ABC

# Delete a key
docker exec <container-name> redis-cli DEL inventory:ABC
```

**Inventory Commands**
```bash
# Get the current inventory for a specific SKU
docker exec <container-name> redis-cli GET inventory:ABC

# Manually set the inventory for a SKU
docker exec <container-name> redis-cli SET inventory:ABC 100

# Increase the inventory for a SKU by 10
docker exec <container-name> redis-cli INCRBY inventory:ABC 10
```

**Idempotency Key Commands**
```bash
# Find all idempotency keys
docker exec <container-name> redis-cli KEYS "idemp:*"

# Check the remaining time-to-live (in seconds) of an idempotency key
docker exec <container-name> redis-cli TTL idemp:order123
```

## Service Overview

### API Gateway

*   **Endpoint:** `POST /orders`
*   **Responsibilities:**
    *   Parse & validate JSON payload
    *   Enforce idempotency via Redis `SETNX`
    *   Publish order messages to RabbitMQ exchange `orders.direct`

### Order Processor

*   **Consumes:** `orders.queue` bound to `orders.direct`
*   **Responsibilities:**
    *   Manual ACK/NACK for reliability
    *   Decrement inventory in Redis (`DECRBY`)
    *   Publish `order.created` events to `orders.topic`

### Notification Service

*   **Consumes:** `notifier.queue` bound to `orders.topic` routing key `order.created`
*   **Responsibilities:**
    *   Exactly-once delivery via Redis `SETNX`
    *   Simulate sending notification (log/email stub)
    *   Record status in Redis hash

### Dashboard (Optional)

*   **Endpoint:** `GET /metrics`
*   **Responsibilities:**
    *   Fetch cached metrics from Redis (order count, inventory levels, notification status)
    *   Serve JSON response for monitoring

## High Availability and Resilience

The API Gateway has been designed to be highly resilient to failures in its downstream dependencies, specifically RabbitMQ.

### Key Features:

*   **Automatic Failover:** The gateway is configured with a list of RabbitMQ nodes. If the primary node it's connected to fails, it will automatically detect the disconnection and begin attempting to connect to the next available node in the list.
*   **Persistent Retries:** If all RabbitMQ nodes are unavailable, the gateway enters a persistent retry loop. It will continuously try to reconnect to the list of servers with a backoff period, ensuring that it will automatically recover its connection as soon as a RabbitMQ node becomes available again.
*   **Synchronous Startup:** The service will not start listening for HTTP traffic until it has successfully established an initial connection to RabbitMQ. This prevents the service from accepting requests that it cannot process.
*   **Thread-Safe Connection Management:** A background goroutine manages the connection state, and access to the shared RabbitMQ channel is protected by a `sync.RWMutex` to prevent data races during reconnection events.

This setup ensures that the API Gateway can survive temporary network partitions or RabbitMQ service restarts without requiring a manual restart itself.

## Testing & CI

*   Unit tests with mocks (interfaces for RabbitMQ/Redis)
*   Integration tests via \[Testcontainers-Go]
*   GitHub Actions workflow spins up Redis & RabbitMQ services, then runs `go test ./...`

## Deployment

The project is configured to run using Docker Compose, which builds and manages the Go services alongside their dependencies.

### Containerization Strategy

The `api-gateway` service is fully containerized and managed by `docker-compose`. It is built from its `Dockerfile` and run as a service, connected to the same network as RabbitMQ and Redis. This is the recommended approach for local development and testing.

The `order-processor` and `notifier` services can be containerized following the same pattern. The `docker-compose.yml` would be updated to include build configurations for them:

```yaml
# Example for extending docker-compose.yml
services:
  # ... existing services
  api-gateway:
    build: ./api-gateway
    ports: ['8080:8080']
    depends_on: [rabbitmq1, rabbitmq2, redis]
    environment:
      - RABBITMQ_URLS=amqp://guest:guest@rabbitmq1:5672/,amqp://guest:guest@rabbitmq2:5672/

  order-processor:
    build: ./order-processor
    depends_on: [rabbitmq1, rabbitmq2, redis]
    # No ports needed as it's a background worker

  notifier:
    build: ./notifier
    depends_on: [rabbitmq1, rabbitmq2, redis]
    # No ports needed as it's a background worker
```

For a production environment, this setup can be deployed to a Docker host or adapted for a Kubernetes cluster. For true production-grade high availability, consider using Redis Sentinel/Cluster and a managed RabbitMQ clustering solution.

## Best Practices

* **Go:**

  * Use `context.Context` for cancellation & deadlines
  * Dependency injection for easy testing
  * Graceful shutdown handling (SIGINT/SIGTERM)

* **RabbitMQ:**

  * Declare durable exchanges & queues
  * Publish persistent messages (`DeliveryMode=2`)
  * Use manual ACK/NACK and dead-letter queues
  * Set QoS (prefetch count) for backpressure

* **Redis:**

  * Key naming with clear prefixes and TTLs
  * Use `SETNX` for idempotency
  * Pipeline commands when modifying multiple keys
  * Distinguish between cache and primary data store

## Extensions

* Add observability: Prometheus metrics + OpenTelemetry tracing
* Implement retry/backoff for transient errors
* Swap RabbitMQ for Kafka/SQS to compare messaging systems
* Build a real frontend dashboard with WebSockets and a JS framework
