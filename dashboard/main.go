package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

var rdb *redis.Client

func init() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using default environment variables")
	}
	connectToRedis()
}

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
	log.Println("Dashboard successfully connected to Redis.")
}

type Metrics struct {
	ProcessedOrdersCount int64   `json:"processed_orders_count"`
	InventoryLevel       *int64  `json:"inventory_level,omitempty"`
	NotificationStatus   *string `json:"notification_status,omitempty"`
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := context.Background()
	metrics := Metrics{}

	// 1. Get total processed orders count
	count, err := rdb.Get(ctx, "orders.processed.count").Int64()
	if err != nil && err != redis.Nil {
		http.Error(w, "Failed to get processed orders count", http.StatusInternalServerError)
		log.Printf("Redis GET error for orders.processed.count: %v", err)
		return
	}
	metrics.ProcessedOrdersCount = count

	// 2. Get inventory for a specific SKU, if requested
	sku := r.URL.Query().Get("sku")
	if sku != "" {
		level, err := rdb.Get(ctx, "inventory:"+sku).Int64()
		if err != nil && err != redis.Nil {
			http.Error(w, "Failed to get inventory level", http.StatusInternalServerError)
			log.Printf("Redis GET error for inventory:%s: %v", sku, err)
			return
		}
		if err == nil {
			metrics.InventoryLevel = &level
		}
	}

	// 3. Get notification status for a specific order ID, if requested
	orderID := r.URL.Query().Get("order_id")
	if orderID != "" {
		status, err := rdb.HGet(ctx, "notifications:"+orderID, "status").Result()
		if err != nil && err != redis.Nil {
			http.Error(w, "Failed to get notification status", http.StatusInternalServerError)
			log.Printf("Redis HGET error for notifications:%s: %v", orderID, err)
			return
		}
		if err == nil {
			metrics.NotificationStatus = &status
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)

	addr := ":8081"
	log.Println("Dashboard server listening on", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
