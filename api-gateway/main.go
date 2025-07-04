package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

type Order struct {
	ID  string `json:"id"`
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

var (
	rdb *redis.Client
	ch  *amqp.Channel
)

func init() {
	// Load .env file. This will not override existing environment variables.
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using default environment variables")
	}

	// Connect to Redis
	rdb = redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	// if _, err := rdb.Ping(context.Background()).Result(); err != nil {
	_, err := rdb.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	// Connect to RabbitMQ
	// Use environment variable to switch between RabbitMQ instances
	rabbitmqHost := "localhost:5672" // Default to primary instance
	if os.Getenv("RABBITMQ_ENV") == "test" {
		rabbitmqHost = "localhost:5673" // Use secondary instance for testing
		log.Println("Using TEST RabbitMQ instance")
	}

	conn, err := amqp.Dial("amqp://guest:guest@" + rabbitmqHost + "/")
	if err != nil {
		log.Fatalf("Failed to connect to RabbitMQ: %v", err)
	}

	ch, err = conn.Channel()
	if err != nil {
		log.Fatalf("Failed to open a channel: %v", err)
	}

	err = ch.ExchangeDeclare("orders.direct", "direct", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to declare an exchange: %v", err)
	}
}

func handleOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var o Order
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if o.ID == "" || o.SKU == "" || o.Qty <= 0 {
		http.Error(w, "Missing or invalid fields", http.StatusBadRequest)
		return
	}

	ctx := context.Background()

	// Idempotency Check
	idempotencyKey := "idemp:" + o.ID
	ok, err := rdb.SetNX(ctx, idempotencyKey, "1", 5*time.Minute).Result()
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

	// Publish Order
	if err := publishOrder(ctx, o); err != nil {
		http.Error(w, "Failed to publish order", http.StatusInternalServerError)
		// Optional: Consider removing the idempotency key if publishing fails
		rdb.Del(ctx, idempotencyKey)
		return
	}

	log.Printf("Order %s accepted and published", o.ID)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("Order received: " + o.ID))
}

func publishOrder(ctx context.Context, order Order) error {
	body, err := json.Marshal(order)
	if err != nil {
		log.Printf("Failed to marshal order %s: %v", order.ID, err)
		return err
	}

	err = ch.PublishWithContext(ctx,
		"orders.direct", // exchange
		"",              // routing key
		false,           // mandatory
		false,           // immediate
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

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/orders", handleOrders)

	addr := ":8080"
	log.Println("API Gateway listening on", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}