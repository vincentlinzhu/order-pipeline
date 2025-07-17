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
	ID    string `json:"id"`
	Email string `json:"email"`
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
				// Declare the topic exchange we consume from
				err = ch.ExchangeDeclare("orders.topic", "topic", true, false, false, false, nil)
				if err != nil {
					log.Printf("Failed to declare exchange at %s: %v", url, err)
					continue
				}
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

// setupRabbitMQ declares queues and bindings for the notifier.
func setupRabbitMQ(ch *amqp.Channel) error {
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
	ch := cm.GetChannel()
	if ch == nil {
		log.Fatal("Failed to get RabbitMQ channel.")
	}

	if err := setupRabbitMQ(ch); err != nil {
		log.Fatalf("Failed to set up RabbitMQ: %v", err)
	}

	msgs, err := ch.Consume(
		"notifier.queue",
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		log.Fatalf("Failed to register a consumer: %v", err)
	}

	forever := make(chan struct{})

	log.Println("Notifier service started. Waiting for messages...")

	go func() {
		for d := range msgs {
			processNotification(d)
		}
	}()

	<-forever
}

// processNotification handles a single notification message.
func processNotification(d amqp.Delivery) {
	var order Order
	if err := json.Unmarshal(d.Body, &order); err != nil {
		log.Printf("Error decoding message: %s", err)
		d.Nack(false, false)
		return
	}

	ctx := context.Background()
	key := "notif:" + order.ID
	ok, err := rdb.SetNX(ctx, key, "1", 24*time.Hour).Result()
	if err != nil {
		log.Printf("Redis error: %s", err)
		d.Nack(false, true) // Requeue on Redis error
		return
	}

	if !ok {
		log.Printf("Notification for order %s already processed.", order.ID)
		d.Ack(false)
		return
	}

	log.Printf("Sending notification for order %s to %s", order.ID, order.Email)
	// Simulate sending email
	time.Sleep(1 * time.Second)

	err = rdb.HSet(ctx, "notifications:"+order.ID, "status", "sent").Err()
	if err != nil {
		log.Printf("Failed to update notification status in Redis: %s", err)
		// Don't requeue, as the notification was already sent
	}

	log.Printf("Notification sent for order %s", order.ID)
	d.Ack(false)
}
