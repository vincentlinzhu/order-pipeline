package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
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
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.BoolCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
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

// --- API Handler ---
type APIHandler struct {
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
			go c.monitorConnection()
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
				err = ch.ExchangeDeclare("orders.direct", "direct", true, false, false, false, nil)
				if err != nil {
					log.Printf("Failed to declare exchange at %s: %v", url, err)
					continue
				}
				c.connection = conn
				c.channel = ch
				return nil
			}
			conn.Close()
		}
	}
	return amqp.ErrClosed
}

// GetChannel provides a thread-safe way to access the current channel.
func (c *ConnectionManager) GetChannel() RabbitChannel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channel
}

// GetConnection provides a thread-safe way to access the current connection.
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
}

// --- HTTP Handler ---
func (h *APIHandler) handleOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var o Order
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if o.ID == "" || o.Email == "" || o.SKU == "" || o.Qty <= 0 {
		http.Error(w, "Missing or invalid fields", http.StatusBadRequest)
		return
	}

	ctx := context.Background()

	idempotencyKey := "idemp:" + o.ID
	ok, err := h.redisClient.SetNX(ctx, idempotencyKey, "1", 5*time.Minute).Result()
	if err != nil {
		http.Error(w, "Redis error", http.StatusInternalServerError)
		log.Printf("Redis SetNX error for key %s: %v", idempotencyKey, err)
		return
	}
	if !ok {
		log.Printf("Duplicate order received: %s", o.ID)
		http.Error(w, "Duplicate order", http.StatusConflict)
		return
	}

	if err := h.publishOrder(ctx, o); err != nil {
		http.Error(w, "Failed to publish order", http.StatusInternalServerError)
		h.redisClient.Del(ctx, idempotencyKey)
		return
	}

	log.Printf("Order %s accepted and published", o.ID)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("Order received: " + o.ID))
}

// --- Publishing Logic ---
func (h *APIHandler) publishOrder(ctx context.Context, order Order) error {
	body, err := json.Marshal(order)
	if err != nil {
		log.Printf("Failed to marshal order %s: %v", order.ID, err)
		return err
	}

	if h.rabbitChannel == nil {
		log.Printf("Cannot publish order %s: RabbitMQ channel is not available", order.ID)
		return amqp.ErrClosed
	}

	err = h.rabbitChannel.PublishWithContext(ctx, "orders.direct", "", false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			Body:         body,
		})
	if err != nil {
		log.Printf("Failed to publish message for order %s: %v", order.ID, err)
		return err
	}
	return nil
}

// --- Main Function ---
func main() {
	handler := &APIHandler{
		redisClient:   rdb,
		rabbitChannel: cm.GetChannel(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/orders", handler.handleOrders)

	addr := ":8080"
	log.Println("API Gateway listening on", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
