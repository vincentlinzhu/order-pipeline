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

// --- Interfaces for Mocking ---

type RedisClient interface {
	DecrBy(ctx context.Context, key string, decrement int64) *redis.IntCmd
	Incr(ctx context.Context, key string) *redis.IntCmd
}

type RabbitChannel interface {
	PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
	Qos(prefetchCount, prefetchSize int, global bool) error
	Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
	ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error
	QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error)
	QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error
}

// --- ConnectionManager ---
type ConnectionManager struct {
	urls         []string
	connection   *amqp.Connection
	channel      *amqp.Channel
	mu           sync.RWMutex
	isConnecting bool
	reconnected  chan struct{} // Channel to signal successful reconnection
	shutdown     chan struct{} // Channel to signal shutdown
}

// --- Global Variables ---
var (
	rdb *redis.Client
	cm  *ConnectionManager
)

// --- Order Struct ---
type Order struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	SKU   string `json:"sku"`
	Qty   int    `json:"qty"`
}

// --- Processor ---
type Processor struct {
	redisClient   RedisClient
	rabbitChannel RabbitChannel
}

// NewConnectionManager creates a new connection manager.
func NewConnectionManager(urls []string) *ConnectionManager {
	return &ConnectionManager{
		urls:        urls,
		reconnected: make(chan struct{}, 1), // Buffered channel of size 1
		shutdown:    make(chan struct{}),
	}
}

// connect attempts to connect to RabbitMQ with a timeout.
func (c *ConnectionManager) connect() error {
	c.mu.Lock()
	if c.isConnecting {
		c.mu.Unlock()
		return nil // Avoid concurrent connection attempts
	}
	c.isConnecting = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isConnecting = false
		c.mu.Unlock()
	}()

	for {
		select {
		case <-c.shutdown:
			return nil
		default:
			for _, url := range c.urls {
				log.Printf("Attempting to connect to RabbitMQ at %s...", url)
				conn, err := amqp.DialConfig(url, amqp.Config{
					Dial: amqp.DefaultDial(5 * time.Second), // 5-second timeout
				})
				if err == nil {
					ch, err := conn.Channel()
					if err == nil {
						c.mu.Lock()
						c.connection = conn
						c.channel = ch
						c.mu.Unlock()
						log.Printf("Successfully connected to RabbitMQ at %s", url)

						// Signal successful connection/reconnection
						select {
						case c.reconnected <- struct{}{}:
						default: // Avoid blocking if the channel is full
						}

						go c.monitorConnection()
						return nil
					}
					log.Printf("Failed to open a channel at %s: %v", url, err)
					conn.Close()
				} else {
					log.Printf("Failed to dial RabbitMQ at %s: %v", url, err)
				}
			}
			log.Println("All RabbitMQ connection attempts failed, retrying in 5 seconds...")
			time.Sleep(5 * time.Second)
		}
	}
}

// monitorConnection runs in the background to handle reconnects.
func (c *ConnectionManager) monitorConnection() {
	closeChan := make(chan *amqp.Error)
	c.GetConnection().NotifyClose(closeChan)

	err := <-closeChan
	if err != nil {
		log.Printf("RabbitMQ connection lost: %v. Starting reconnection process...", err)
		c.connect() // Start reconnection process
	}
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

// Close gracefully shuts down the connection manager.
func (c *ConnectionManager) Close() {
	close(c.shutdown)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channel != nil {
		c.channel.Close()
	}
	if c.connection != nil {
		c.connection.Close()
	}
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
func setupRabbitMQ(ch RabbitChannel) error {
	err := ch.ExchangeDeclare("orders.direct", "direct", true, false, false, false, nil)
	if err != nil {
		return err
	}
	_, err = ch.QueueDeclare("orders.queue", true, false, false, false, nil)
	if err != nil {
		return err
	}
	err = ch.QueueBind("orders.queue", "", "orders.direct", false, nil)
	if err != nil {
		return err
	}
	err = ch.ExchangeDeclare("orders.topic", "topic", true, false, false, false, nil)
	if err != nil {
		return err
	}
	return ch.Qos(10, 0, false)
}

// --- Main Function ---
func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using default environment variables")
	}

	connectToRedis()

	rabbitmqURLs := os.Getenv("RABBITMQ_URLS")
	if rabbitmqURLs == "" {
		rabbitmqURLs = "amqp://guest:guest@localhost:5672/"
	}
	cm = NewConnectionManager(strings.Split(rabbitmqURLs, ","))
	go cm.connect() // Start connection manager in the background

	// Main application loop
	for {
		// Wait for a connection signal before trying to run the consumer
		<-cm.reconnected

		select {
		case <-cm.shutdown:
			log.Println("Shutting down processor.")
			return
		default:
			runConsumer()
		}
	}
}

// runConsumer sets up and runs the message consumer loop.
func runConsumer() {
	ch := cm.GetChannel()
	if ch == nil {
		log.Println("Cannot run consumer, channel is not available.")
		return
	}

	if err := setupRabbitMQ(ch); err != nil {
		log.Printf("Failed to set up RabbitMQ: %v. Retrying...", err)
		return // Exit and let the main loop handle reconnection wait
	}

	msgs, err := ch.Consume("orders.queue", "order-processor", false, false, false, false, nil)
	if err != nil {
		log.Printf("Failed to register a consumer: %v. Retrying...", err)
		return // Exit and let the main loop handle reconnection wait
	}

	processor := &Processor{
		redisClient:   rdb,
		rabbitChannel: ch,
	}

	log.Println("Order Processor started. Waiting for messages...")
	for d := range msgs {
		processor.processOrder(d)
	}
	log.Println("Consumer loop finished.") // This will be logged if the channel is closed
}

// processOrder is the core logic for handling a single order message.
func (p *Processor) processOrder(d amqp.Delivery) {
	var order Order
	if err := json.Unmarshal(d.Body, &order); err != nil {
		log.Printf("Error unmarshalling message: %s", err)
		d.Nack(false, false)
		return
	}

	log.Printf("Received order %s for %s: SKU %s, Qty %d", order.ID, order.Email, order.SKU, order.Qty)

	ctx := context.Background()
	inventoryKey := "inventory:" + order.SKU
	if err := p.redisClient.DecrBy(ctx, inventoryKey, int64(order.Qty)).Err(); err != nil {
		log.Printf("Failed to update inventory for SKU %s: %v", order.SKU, err)
		d.Nack(true, false) // Requeue the message
		return
	}
	log.Printf("Decremented inventory for SKU %s by %d", order.SKU, order.Qty)

	if err := p.redisClient.Incr(ctx, "orders.processed.count").Err(); err != nil {
		log.Printf("Failed to increment processed orders count: %v", err)
	}

	p.publishOrderCreatedEvent(ctx, order)
	d.Ack(false)
}

// publishOrderCreatedEvent publishes a message to the topic exchange.
func (p *Processor) publishOrderCreatedEvent(ctx context.Context, order Order) {
	body, err := json.Marshal(order)
	if err != nil {
		log.Printf("Failed to marshal event for order %s: %v", order.ID, err)
		return
	}

	err = p.rabbitChannel.PublishWithContext(ctx, "orders.topic", "order.created", false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
		})
	if err != nil {
		log.Printf("Failed to publish order.created event for order %s: %v", order.ID, err)
	} else {
		log.Printf("Published 'order.created' event for order %s", order.ID)
	}
}