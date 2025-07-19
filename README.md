# Order-Pipeline

A simple event-driven microservices project built with Go, Redis, and RabbitMQ. This pipeline demonstrates how to handle HTTP requests, perform idempotency checks, process messages asynchronously, and maintain cache state—all with best practices for reliability and scalability.

Next Steps:
- Add a proper database like PostgreSQL or Cassandra DB and use a cache scheme (aside, read back, write around, write through, etc) to interact with the redis cache.
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
   git clone git@github.com:vincentlinzhu/order-pipeline.git
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


### Interacting with the Redis Datastore

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
docker exec <container-name> redis-cli TYPE <key>

# Delete a key
docker exec <container-name> redis-cli DEL <key>
```

**Inventory Commands**
```bash
# Get the current inventory for a specific SKU
docker exec <container-name> redis-cli GET <key>

# Manually set the inventory for a SKU
docker exec <container-name> redis-cli SET <key> 100

# Increase the inventory for a SKU by 10
docker exec <container-name> redis-cli INCRBY <key> 10
```

**Idempotency Key Commands**
```bash
# Find all idempotency keys
docker exec <container-name> redis-cli KEYS "idemp:*"

# Check the remaining time-to-live (in seconds) of an idempotency key
docker exec <container-name> redis-cli TTL idemp:order123
```

### Fill in the inventory of a product:

Let's say we have an item "mag-safe-phone-case". We want the item to be in out inventory, so we can use these commands to set up the inventory for our test item.

```bash
docker exec order-pipeline-redis-1 redis-cli SET inventory:mag-safe-phone-case 100
```
```bash
docker exec order-pipeline-redis-1 redis-cli GET inventory:mag-safe-phone-case
```

### Placing an Order (with Authentication)

With the addition of user authentication, placing an order is now a three-step process:
1.  **Register** a new user.
2.  **Login** with the user's credentials to receive a JWT authentication token.
3.  **Place the order** by sending the request with the JWT in the `Authorization` header.

**1. Register a New User**

Send a `POST` request to the `/register` endpoint with an email and password.

```bash
curl -X POST http://localhost:8080/register \
     -H "Content-Type: application/json" \
     -d '{"email":"customer@example.com","password":"securepassword123"}'
```

Expected Output:
```
User registered successfully
```

**2. Log In to Get a Token**

Send a `POST` request to the `/login` endpoint with the same credentials.

```bash
curl -X POST http://localhost:8080/login \
     -H "Content-Type: application/json" \
     -d '{"email":"customer@example.com","password":"securepassword123"}'
```

Expected Output (the token will be different each time):
```json
{"token":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJlbWFpbCI6ImN1c3RvbWVyQGV4YW1wbGUuY29tIiwiZXhwIjoxNzIxMzg5MjQxfQ.SomeTokenSignature"}
```

**3. Place the Order with the Token**

Copy the token from the login response and use it in the `Authorization` header of your order request.

> **Note:** Replace `<your_jwt_token>` with the actual token you received.

```bash
TOKEN="<your_jwt_token>"

curl -X POST http://localhost:8080/orders \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer $TOKEN" \
     -d '{"id":"order123","sku":"mag-safe-phone-case","qty":2}'
```

Expected Output:
```
Order received: order123
```

**4. Send the Same Order Again (Idempotency Check)**

If you run the exact same `curl` command from Step 3 again, the idempotency check will prevent a duplicate order.

Expected Output:
```
Duplicate order
```

This confirms that the system correctly authenticates the user and processes the order while still preventing duplicates.


### Monitoring with the Dashboard

The dashboard service provides metrics on port `8081`.

**Get total processed orders:**
```bash
curl http://localhost:8081/metrics
```

**Check inventory for a specific SKU:**
```bash
curl http://localhost:8081/metrics?sku=mag-safe-phone-case
```

**Check the notification status for an order:**
```bash
curl http://localhost:8081/metrics?order_id=order123
```

### Shutdown the application


To shut down the application, press `Ctrl+C` in the terminal where `docker-compose up` is running. This will stop the containers. After that, run the following command to remove the containers and networks created by `docker-compose`:

```bash
docker-compose down
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

### Dashboard Service

*   **Endpoint:** `GET /metrics` on port `:8081`
*   **Responsibilities:**
    *   Provides a read-only view into the system's state.
    *   Fetches metrics directly from Redis, such as total processed orders, inventory levels, and notification statuses.


## Authentication

The application uses a robust, token-based authentication system to secure endpoints and manage users.

### Key Technologies & Methodologies

*   **JWT (JSON Web Tokens):** Authentication is handled using JWTs. After a user successfully logs in, the API Gateway generates a signed JWT (using HS256) that contains the user's email and an expiration time. This token must be included in the `Authorization` header for all subsequent requests to protected endpoints. This approach ensures stateless authentication, as the server does not need to store session information.

*   **Cassandra for User Storage:** User credentials, including their email and a securely hashed password, are stored in a Cassandra database. The `users_keyspace.users` table is used for this purpose.

*   **bcrypt for Password Hashing:** To ensure password security, all user passwords are hashed using the `bcrypt` library before being stored in the database. This one-way hashing algorithm is resistant to rainbow table and brute-force attacks.

*   **Authentication Middleware:** A dedicated middleware is applied to sensitive routes (like `/orders` and `/delete-user`). This middleware inspects incoming requests for a valid JWT, verifies its signature and expiration, and extracts the user's identity. If the token is valid, the user's email is added to the request context for use by the handler. If the token is missing or invalid, the middleware rejects the request with an appropriate HTTP error (401 Unauthorized or 403 Forbidden).

### Authentication Endpoints

*   `POST /register`: Creates a new user in the database.
*   `POST /login`: Authenticates a user and returns a JWT.
*   `DELETE /delete-user`: Deletes the authenticated user (requires a valid JWT).


## High Availability and Resilience

The API Gateway has been designed to be highly resilient to failures in its downstream dependencies, specifically RabbitMQ.

### Key Features:

*   **Automatic Failover:** The gateway is configured with a list of RabbitMQ nodes. If the primary node it's connected to fails, it will automatically detect the disconnection and begin attempting to connect to the next available node in the list.
*   **Persistent Retries:** If all RabbitMQ nodes are unavailable, the gateway enters a persistent retry loop. It will continuously try to reconnect to the list of servers with a backoff period, ensuring that it will automatically recover its connection as soon as a RabbitMQ node becomes available again.
*   **Synchronous Startup:** The service will not start listening for HTTP traffic until it has successfully established an initial connection to RabbitMQ. This prevents the service from accepting requests that it cannot process.
*   **Thread-Safe Connection Management:** A background goroutine manages the connection state, and access to the shared RabbitMQ channel is protected by a `sync.RWMutex` to prevent data races during reconnection events.

This setup ensures that the API Gateway can survive temporary network partitions or RabbitMQ service restarts without requiring a manual restart itself.

## Testing & CI

This project is configured with a full suite of unit tests and a Continuous Integration (CI) pipeline using GitHub Actions.

### Running Tests Locally

The services have been refactored using interfaces and dependency injection to allow for robust unit testing with mocks. To run all tests for all services, execute the following command from the project root:

```bash
go test ./api-gateway/... ./order-processor/... ./notifier/...
```
This command will discover and run all `*_test.go` files in the project. It will also download any necessary test dependencies like `testify`.

### CI Pipeline

The CI pipeline is defined in `.github/workflows/ci.yml`. It automatically triggers on every `push` and `pull_request` to the repository.

The pipeline performs the following steps:
1.  Spins up a clean Ubuntu environment.
2.  Starts background service containers for Redis and RabbitMQ.
3.  Checks out the repository code.
4.  Sets up the correct Go version.
5.  Runs the `go test ./api-gateway/... ./order-processor/... ./notifier/...` command.

If any test fails, the pipeline will fail, providing immediate feedback on the code changes.

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
