# Order-Pipeline

A simple event-driven microservices project built with Go, Redis, and RabbitMQ. This pipeline demonstrates how to handle HTTP requests, perform idempotency checks, process messages asynchronously, and maintain cache state—all with best practices for reliability and scalability.

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

Each service can be run independently. For example, to start the API Gateway:

```bash
cd api-gateway
go run main.go
```

By default:

* API Gateway listens on port **8080**
* Order Processor and Notifier connect to RabbitMQ at **amqp\://guest\:guest\@localhost:5672** and Redis at **localhost:6379**

## Service Overview

### API Gateway

* **Endpoint:** `POST /orders`
* **Responsibilities:**

  * Parse & validate JSON payload
  * Enforce idempotency via Redis `SETNX`
  * Publish order messages to RabbitMQ exchange `orders.direct`

### Order Processor

* **Consumes:** `orders.queue` bound to `orders.direct`
* **Responsibilities:**

  * Manual ACK/NACK for reliability
  * Decrement inventory in Redis (`DECRBY`)
  * Publish `order.created` events to `orders.topic`

### Notification Service

* **Consumes:** `notifier.queue` bound to `orders.topic` routing key `order.created`
* **Responsibilities:**

  * Exactly-once delivery via Redis `SETNX`
  * Simulate sending notification (log/email stub)
  * Record status in Redis hash

### Dashboard (Optional)

* **Endpoint:** `GET /metrics`
* **Responsibilities:**

  * Fetch cached metrics from Redis (order count, inventory levels, notification status)
  * Serve JSON response for monitoring

## Testing & CI

* Unit tests with mocks (interfaces for RabbitMQ/Redis)
* Integration tests via \[Testcontainers-Go]
* GitHub Actions workflow spins up Redis & RabbitMQ services, then runs `go test ./...`

## Deployment

1. Add Dockerfiles to each service:

   ```dockerfile
   FROM golang:1.20-alpine AS build
   WORKDIR /app
   COPY . .
   RUN go build -o service

   FROM alpine:latest
   COPY --from=build /app/service /usr/local/bin/service
   ENTRYPOINT ["/usr/local/bin/service"]
   ```
2. Update `docker-compose.yml` to build each service:

   ```yaml
   services:
     api-gateway:
       build: ./api-gateway
       ports: ['8080:8080']
       depends_on: [rabbitmq, redis]
     order-processor:
       build: ./order-processor
       depends_on: [rabbitmq, redis]
     notifier:
       build: ./notifier
       depends_on: [rabbitmq, redis]
   ```
3. For production, deploy to a Docker host or Kubernetes cluster. Consider using Redis Sentinel/Cluster and RabbitMQ clustering for HA.

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
