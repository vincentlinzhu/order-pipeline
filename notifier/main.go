package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"
	amqp "github.com/rabbitmq/amqp091-go"
)

// --- Interfaces for Mocking ---

type RedisClient interface {
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.BoolCmd
	HSet(ctx context.Context, key string, values ...interface{}) *redis.IntCmd
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
}

// --- Notifier ---
type Notifier struct {
	redisClient RedisClient
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

// setupRabbitMQ declares exchanges, queues, and bindings for the notifier.
func setupRabbitMQ(ch *amqp.Channel) error {
	err := ch.ExchangeDeclare("orders.topic", "topic", true, false, false, false, nil)
	if err != nil {
		return err
	}
	q, err := ch.QueueDeclare("notifier.queue", true, false, false, false, nil)
	if err != nil {
		return err
	}
	err = ch.QueueBind(q.Name, "order.created", "orders.topic", false, nil)
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
			log.Println("Shutting down notifier.")
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
		return
	}

	msgs, err := ch.Consume("notifier.queue", "notifier-service", false, false, false, false, nil)
	if err != nil {
		log.Printf("Failed to register a consumer: %v. Retrying...", err)
		return
	}

	notifier := &Notifier{redisClient: rdb}

	log.Println("Notifier service started. Waiting for messages...")
	for d := range msgs {
		notifier.processNotification(d)
	}
	log.Println("Consumer loop finished.")
}

// processNotification handles a single notification message.
func (n *Notifier) processNotification(d amqp.Delivery) {
	var order Order
	if err := json.Unmarshal(d.Body, &order); err != nil {
		log.Printf("Error decoding message: %s", err)
		d.Nack(false, false)
		return
	}

	ctx := context.Background()
	key := "notif:" + order.ID
	ok, err := n.redisClient.SetNX(ctx, key, "1", 24*time.Hour).Result()
	if err != nil {
		log.Printf("Redis error: %s", err)
		d.Nack(false, true) // Requeue
		return
	}

	if !ok {
		log.Printf("Notification for order %s already processed.", order.ID)
		d.Ack(false)
		return
	}

	log.Printf("Sending notification for order %s to %s", order.ID, order.Email)
	time.Sleep(1 * time.Second) // Simulate work

	err = n.redisClient.HSet(ctx, "notifications:"+order.ID, "status", "sent").Err()
	if err != nil {
		log.Printf("Failed to update notification status in Redis: %s", err)
	}

	log.Printf("Notification sent for order %s", order.ID)
	d.Ack(false)
}