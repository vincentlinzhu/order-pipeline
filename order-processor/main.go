package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

// --- ConnectionManager ---
type ConnectionManager struct {
	urls       []string
	connection *amqp.Connection
	channel    *amqp.Channel
	mu         sync.RWMutex
}

// --- Global Variables ---
var (
	rdb *redis.Client
	cm  *ConnectionManager
)

// --- Order Struct ---
type Order struct {
	ID  string `json:"id"`
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// --- Initialization ---
func init() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using default environment variables")
	}

	connectToRedis()

	rabbitmqURLs := os.Getenv("RABBITMQ_URLS")
	if rabbitmqURLs == "" {
		rabbitmqURLs = "amqp://guest:guest@localhost:5672/"
	}
	cm = NewConnectionManager(strings.Split(rabbitmqURLs, ","))

	log.Println("Attempting initial connection to RabbitMQ...")
	for {
		if err := cm.connect(); err == nil {
			log.Println("Initial RabbitMQ connection established.")
			break
		}
		log.Println("Initial RabbitMQ connection failed, retrying in 5 seconds...")
		time.Sleep(5 * time.Second)
	}

	go cm.monitorConnection()
}

// NewConnectionManager creates a new connection manager.
func NewConnectionManager(urls []string) *ConnectionManager {
	return &ConnectionManager{
		urls: urls,
	}
}

// monitorConnection runs in the background to handle reconnects.
func (c *ConnectionManager) monitorConnection() {
	closeChan := make(chan *amqp.Error)
	c.GetConnection().NotifyClose(closeChan)

	err := <-closeChan
	log.Printf("RabbitMQ connection lost: %v. Starting reconnection process...", err)

	for {
		if err := c.connect(); err == nil {
			log.Println("Successfully reconnected to RabbitMQ.")
			// After reconnecting, we must restart the consumer.
			// A simple approach for a worker is to log a fatal error
			// and let the container orchestrator (Docker/Kubernetes) restart it.
			log.Fatal("Reconnected to RabbitMQ, but consumer needs to be restarted. Shutting down.")
			return
		}
		log.Println("RabbitMQ reconnection failed, retrying in 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

// connect attempts to connect to RabbitMQ.
func (c *ConnectionManager) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, url := range c.urls {
		conn, err := amqp.Dial(url)
		if err == nil {
			ch, err := conn.Channel()
			if err == nil {
				c.connection = conn
				c.channel = ch
				return nil // Success
			}
			conn.Close()
		}
	}
	return amqp.ErrClosed
}

// GetChannel provides thread-safe access to the channel.
func (c *ConnectionManager) GetChannel() *amqp.Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channel
}

// GetConnection provides thread-safe access to the connection.
func (c *ConnectionManager) GetConnection() *amqp.Connection {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connection
}

// --- Redis Connection ---
func connectToRedis() {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb = redis.NewClient(&redis.Options{Addr: redisAddr})
	_, err := rdb.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
	log.Println("Successfully connected to Redis.")
}

// setupRabbitMQ declares exchanges, queues, and bindings.
func setupRabbitMQ(ch *amqp.Channel) error {
	// Declare the exchange we receive messages from
	err := ch.ExchangeDeclare("orders.direct", "direct", true, false, false, false, nil)
	if err != nil {
		return err
	}

	// Declare the queue for orders
	_, err = ch.QueueDeclare("orders.queue", true, false, false, false, nil)
	if err != nil {
		return err
	}

	// Bind the queue to the exchange
	err = ch.QueueBind("orders.queue", "", "orders.direct", false, nil)
	if err != nil {
		return err
	}

	// Declare the topic exchange we publish events to
	err = ch.ExchangeDeclare("orders.topic", "topic", true, false, false, false, nil)
	if err != nil {
		return err
	}

	// Set a Quality of Service: process up to 10 messages at a time.
	// This prevents the worker from being overwhelmed.
	return ch.Qos(10, 0, false)
}

// --- Main Function ---
func main() {
	ch := cm.GetChannel()
	if ch == nil {
		log.Fatal("Failed to get RabbitMQ channel.")
	}

	if err := setupRabbitMQ(ch); err != nil {
		log.Fatalf("Failed to set up RabbitMQ: %v", err)
	}

	// Start consuming messages from the queue
	msgs, err := ch.Consume(
		"orders.queue", // queue
		"",             // consumer
		false,          // auto-ack
		false,          // exclusive
		false,          // no-local
		false,          // no-wait
		nil,            // args
	)
	if err != nil {
		log.Fatalf("Failed to register a consumer: %v", err)
	}

	forever := make(chan struct{})

	log.Println("Order Processor started. Waiting for messages...")

	go func() {
		for d := range msgs {
			processOrder(d)
		}
	}()

	<-forever // Block forever
}

// processOrder is the core logic for handling a single order message.
func processOrder(d amqp.Delivery) {
	var order Order
	if err := json.Unmarshal(d.Body, &order); err != nil {
		log.Printf("Error unmarshalling message: %s", err)
		d.Nack(false, false) // Nack without requeue for bad messages
		return
	}

	log.Printf("Received order %s: SKU %s, Qty %d", order.ID, order.SKU, order.Qty)

	// --- Update Inventory in Redis ---
	ctx := context.Background()
	inventoryKey := "inventory:" + order.SKU
	err := rdb.DecrBy(ctx, inventoryKey, int64(order.Qty)).Err()
	if err != nil {
		log.Printf("Failed to update inventory for SKU %s: %v", order.SKU, err)
		d.Nack(true, false) // Nack and requeue on processing failure
		return
	}
	log.Printf("Decremented inventory for SKU %s by %d", order.SKU, order.Qty)

	// --- Publish Downstream Event ---
	publishOrderCreatedEvent(ctx, order)

	// Acknowledge the message to remove it from the queue
	d.Ack(false)
}

// publishOrderCreatedEvent publishes a message to the topic exchange.
func publishOrderCreatedEvent(ctx context.Context, order Order) {
	ch := cm.GetChannel()
	if ch == nil {
		log.Printf("Cannot publish event for order %s: channel is not available", order.ID)
		return
	}

	body, err := json.Marshal(order)
	if err != nil {
		log.Printf("Failed to marshal event for order %s: %v", order.ID, err)
		return
	}

	err = ch.PublishWithContext(ctx,
		"orders.topic",    // exchange
		"order.created",   // routing key
		false,             // mandatory
		false,             // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
		})

	if err != nil {
		log.Printf("Failed to publish order.created event for order %s: %v", order.ID, err)
		// Note: In a real system, you'd need a robust outbox pattern here
		// to handle failures in publishing downstream events.
		return
	}

	log.Printf("Published 'order.created' event for order %s", order.ID)
}
