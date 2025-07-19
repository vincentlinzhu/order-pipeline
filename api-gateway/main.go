package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/golang-jwt/jwt/v5"
	"github.com/joho/godotenv"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

// --- JWT Secret Key ---
var jwtSecret []byte

// userContextKey is a custom type for context keys to avoid collisions.
type userContextKey string

const authenticatedUserKey userContextKey = "authenticatedUserEmail"

// --- Interfaces for Mocking ---

type RedisClient interface {
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.BoolCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

type RabbitChannel interface {
	PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error
}

type CassandraClient interface {
	Query(stmt string, args ...interface{}) *gocql.Query
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
	rdb              *redis.Client
	cm               *ConnectionManager
	cassandraSession *gocql.Session
)

// --- User & Order Structs ---
type User struct {
	Email        string `json:"email"`
	PasswordHash string `json:"-"`
}

type Order struct {
	ID        string `json:"id"`
	Email     string
	SKU       string `json:"sku"`
	Qty       int    `json:"qty"`
	UserEmail string `json:"user_email"`
}

// --- APIHandler ---
// Holds a reference to the ConnectionManager to get the latest channel.
type APIHandler struct {
	redisClient RedisClient
	cm          *ConnectionManager
	userRepo    UserRepository
}

// --- HTTP Handlers (register, login, delete) ---

func (h *APIHandler) registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var creds struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if creds.Email == "" || creds.Password == "" {
		http.Error(w, "Email and password are required", http.StatusBadRequest)
		return
	}
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(creds.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Failed to hash password", http.StatusInternalServerError)
		return
	}
	user := &User{Email: creds.Email, PasswordHash: string(hashedPassword)}
	if err := h.userRepo.CreateUser(context.Background(), user); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			http.Error(w, "User with this email already exists", http.StatusConflict)
		} else {
			http.Error(w, "Failed to register user", http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("User registered successfully"))
}

func (h *APIHandler) loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var creds struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	user, err := h.userRepo.GetUserByEmail(context.Background(), creds.Email)
	if err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(creds.Password)); err != nil {
		http.Error(w, "Invalid credentials", http.StatusUnauthorized)
		return
	}
	expirationTime := time.Now().Add(1 * time.Hour)
	claims := jwt.MapClaims{"email": user.Email, "exp": expirationTime.Unix()}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(jwtSecret)
	if err != nil {
		http.Error(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenString})
}

func (h *APIHandler) deleteUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userEmail, ok := r.Context().Value(authenticatedUserKey).(string)
	if !ok || userEmail == "" {
		http.Error(w, "User not authenticated", http.StatusInternalServerError)
		return
	}
	if err := h.userRepo.DeleteUser(context.Background(), userEmail); err != nil {
		http.Error(w, "Failed to delete user", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("User deleted successfully"))
}

// --- Authentication Middleware ---
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}
		tokenString := strings.TrimPrefix(authHeader, "Bearer ")
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			http.Error(w, "Invalid token", http.StatusForbidden)
			return
		}
		if claims, ok := token.Claims.(jwt.MapClaims); ok {
			if userEmail, ok := claims["email"].(string); ok {
				ctx := context.WithValue(r.Context(), authenticatedUserKey, userEmail)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		http.Error(w, "Invalid token claims", http.StatusForbidden)
	})
}

// --- User Repository ---
type UserRepository interface {
	CreateUser(ctx context.Context, user *User) error
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	DeleteUser(ctx context.Context, email string) error
}

type CassandraUserRepository struct{ session CassandraClient }

func NewCassandraUserRepository(session CassandraClient) *CassandraUserRepository {
	return &CassandraUserRepository{session: session}
}
func (r *CassandraUserRepository) CreateUser(ctx context.Context, user *User) error {
	var existingEmail string
	if err := r.session.Query("SELECT email FROM users_keyspace.users WHERE email = ?", user.Email).WithContext(ctx).Scan(&existingEmail); err == nil && existingEmail != "" {
		return fmt.Errorf("user with email %s already exists", user.Email)
	}
	return r.session.Query(`INSERT INTO users_keyspace.users (email, password_hash) VALUES (?, ?)`, user.Email, user.PasswordHash).WithContext(ctx).Exec()
}
func (r *CassandraUserRepository) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var user User
	err := r.session.Query(`SELECT email, password_hash FROM users_keyspace.users WHERE email = ?`, email).WithContext(ctx).Scan(&user.Email, &user.PasswordHash)
	return &user, err
}
func (r *CassandraUserRepository) DeleteUser(ctx context.Context, email string) error {
	return r.session.Query(`DELETE FROM users_keyspace.users WHERE email = ?`, email).WithContext(ctx).Exec()
}

// --- Connection Setup ---

func setupDependencies() {
	godotenv.Load()
	connectToRedis()
	connectToCassandra()

	jwtSecret = []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		log.Fatal("JWT_SECRET environment variable not set.")
	}

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

func NewConnectionManager(urls []string) *ConnectionManager {
	return &ConnectionManager{urls: urls}
}

func (c *ConnectionManager) connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, url := range c.urls {
		conn, err := amqp.DialConfig(url, amqp.Config{Dial: amqp.DefaultDial(5 * time.Second)})
		if err == nil {
			ch, err := conn.Channel()
			if err == nil {
				if err := ch.ExchangeDeclare("orders.direct", "direct", true, false, false, false, nil); err != nil {
					conn.Close()
					continue
				}
				c.connection = conn
				c.channel = ch
				log.Printf("Successfully connected to RabbitMQ at %s", url)
				return nil
			}
			conn.Close()
		}
	}
	return amqp.ErrClosed
}

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

func (c *ConnectionManager) GetChannel() RabbitChannel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channel
}

func connectToRedis() {
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb = redis.NewClient(&redis.Options{Addr: redisAddr})
	if _, err := rdb.Ping(context.Background()).Result(); err != nil {
		log.Fatalf("Failed to connect to Redis: %v", err)
	}
}

func connectToCassandra() {
	cassandraAddr := os.Getenv("CASSANDRA_ADDR")
	if cassandraAddr == "" {
		cassandraAddr = "localhost:9042"
	}

	cluster := gocql.NewCluster(cassandraAddr)
	cluster.Keyspace = "system"
	cluster.Consistency = gocql.Quorum
	cluster.Port = 9042
	cluster.ConnectTimeout = 10 * time.Second
	cluster.Authenticator = gocql.PasswordAuthenticator{
		Username: "cassandra",
		Password: "cassandra",
	}

	var err error
	for i := 0; i < 5; i++ { // Retry connection a few times
		cassandraSession, err = cluster.CreateSession()
		if err == nil {
			log.Println("Connected to Cassandra.")
			break
		}
		log.Printf("Failed to connect to Cassandra (attempt %d): %v. Retrying in 5 seconds...", i+1, err)
		time.Sleep(5 * time.Second)
	}

	if err != nil {
		log.Fatalf("Failed to connect to Cassandra after multiple retries: %v", err)
	}

	// Create keyspace and table if they don't exist
	createKeyspaceCql := `CREATE KEYSPACE IF NOT EXISTS users_keyspace WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}`
	if err := cassandraSession.Query(createKeyspaceCql).Exec(); err != nil {
		log.Fatalf("Failed to create keyspace: %v", err)
	}
	log.Println("Keyspace 'users_keyspace' ensured.")

	cassandraSession.Close() // Close the session to 'system' keyspace
	cluster.Keyspace = "users_keyspace"
	cassandraSession, err = cluster.CreateSession() // Reconnect to the new keyspace
	if err != nil {
		log.Fatalf("Failed to connect to users_keyspace: %v", err)
	}

	createTableCql := `CREATE TABLE IF NOT EXISTS users (
		email text PRIMARY KEY,
		password_hash text
	)`
	if err := cassandraSession.Query(createTableCql).Exec(); err != nil {
		log.Fatalf("Failed to create users table: %v", err)
	}
	log.Println("Table 'users' ensured.")
}

// --- Order Handling Logic ---

func (h *APIHandler) handleOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userEmail, _ := r.Context().Value(authenticatedUserKey).(string)
	var o Order
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	o.Email = userEmail
	o.UserEmail = userEmail
	if o.ID == "" || o.SKU == "" || o.Qty <= 0 {
		http.Error(w, "Missing or invalid fields", http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	idempotencyKey := "idemp:" + o.ID
	ok, err := h.redisClient.SetNX(ctx, idempotencyKey, "1", 5*time.Minute).Result()
	if err != nil || !ok {
		http.Error(w, "Duplicate order or Redis error", http.StatusConflict)
		return
	}
	if err := h.publishOrder(ctx, o); err != nil {
		http.Error(w, "Failed to publish order", http.StatusInternalServerError)
		h.redisClient.Del(ctx, idempotencyKey) // Clean up idempotency key on failure
		return
	}
	log.Printf("Order %s accepted and published for user %s", o.ID, o.UserEmail)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("Order received: " + o.ID))
}

func (h *APIHandler) publishOrder(ctx context.Context, order Order) error {
	body, err := json.Marshal(order)
	if err != nil {
		return fmt.Errorf("failed to marshal order: %w", err)
	}

	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		ch := h.cm.GetChannel()
		if ch == nil {
			log.Printf("Attempt %d/%d: RabbitMQ channel is nil. Retrying in 2 seconds...", i+1, maxRetries)
			time.Sleep(2 * time.Second)
			continue
		}

		err = ch.PublishWithContext(ctx, "orders.direct", "", false, false,
			amqp.Publishing{
				ContentType:  "application/json",
				DeliveryMode: amqp.Persistent,
				Body:         body,
			})

		if err == nil {
			return nil // Success
		}

		if amqpErr, ok := err.(*amqp.Error); ok && (amqpErr.Code == amqp.ChannelError || amqpErr.Code == amqp.ConnectionForced) {
			log.Printf("Attempt %d/%d: Connection error publishing: %v. Retrying...", i+1, maxRetries, err)
			time.Sleep(2 * time.Second)
			continue
		}

		return fmt.Errorf("non-retriable error publishing: %w", err)
	}
	return fmt.Errorf("failed to publish message after %d retries", maxRetries)
}

// --- Main Function ---
func main() {
	setupDependencies()

	userRepo := NewCassandraUserRepository(cassandraSession)
	handler := &APIHandler{
		redisClient: rdb,
		cm:          cm, // Pass the connection manager
		userRepo:    userRepo,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register", handler.registerHandler)
	mux.HandleFunc("/login", handler.loginHandler)
	mux.Handle("/orders", authMiddleware(http.HandlerFunc(handler.handleOrders)))
	mux.Handle("/delete-user", authMiddleware(http.HandlerFunc(handler.deleteUserHandler)))

	addr := ":8080"
	log.Println("API Gateway listening on", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}