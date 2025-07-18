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
}

// --- ConnectionManager ---
type ConnectionManager struct {
	urls       []string
	connection *amqp.Connection
	channel    RabbitChannel
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
	SKU   string `json:"sku"`
	Qty   int    `json:"qty"`
}

// --- Processor ---
type Processor struct {
	redisClient   RedisClient
	rabbitChannel RabbitChannel
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
	c.connection.NotifyClose(closeChan)

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
func (c *ConnectionManager) GetChannel() RabbitChannel {
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
func setupRabbitMQ(ch RabbitChannel) error {
	// This is a type assertion. We need the underlying amqp.Channel to declare exchanges and queues.
	// This part of the setup is not easily mockable without more complex interfaces.
	realCh, ok := ch.(*amqp.Channel)
	if !ok {
		log.Fatal("setupRabbitMQ requires a real *amqp.Channel")
	}

	err := realCh.ExchangeDeclare("orders.direct", "direct", true, false, false, false, nil)
	if err != nil {
		return err
	}
	_, err = realCh.QueueDeclare("orders.queue", true, false, false, false, nil)
	if err != nil {
		return err
	}
	err = realCh.QueueBind("orders.queue", "", "orders.direct", false, nil)
	if err != nil {
		return err
	}
	err = realCh.ExchangeDeclare("orders.topic", "topic", true, false, false, false, nil)
	if err != nil {
		return err
	}
	return realCh.Qos(10, 0, false)
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

	realCh, _ := ch.(*amqp.Channel)
	msgs, err := realCh.Consume("orders.queue", "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to register a consumer: %v", err)
	}

	processor := &Processor{
		redisClient:   rdb,
		rabbitChannel: ch,
	}

	forever := make(chan struct{})
	log.Println("Order Processor started. Waiting for messages...")
	go func() {
		for d := range msgs {
			processor.processOrder(d)
		}
	}()
	<-forever
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
		d.Nack(true, false)
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
	}
	log.Printf("Published 'order.created' event for order %s", order.ID)
}