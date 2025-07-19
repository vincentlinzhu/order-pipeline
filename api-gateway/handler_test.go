package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"golang.org/x/crypto/bcrypt"
)

// --- Mocks ---

type MockRedisClient struct {
	mock.Mock
}

func (m *MockRedisClient) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.BoolCmd {
	args := m.Called(ctx, key, value, expiration)
	cmd := redis.NewBoolCmd(ctx)
	cmd.SetVal(args.Bool(0))
	if args.Error(1) != nil {
		cmd.SetErr(args.Error(1))
	}
	return cmd
}

func (m *MockRedisClient) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	args := m.Called(ctx, keys)
	cmd := redis.NewIntCmd(ctx)
	if args.Error(0) != nil {
		cmd.SetErr(args.Error(0))
	}
	return cmd
}

type MockRabbitChannel struct {
	mock.Mock
}

func (m *MockRabbitChannel) PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error {
	args := m.Called(ctx, exchange, key, mandatory, immediate, msg)
	return args.Error(0)
}

type MockUserRepository struct {
	mock.Mock
}

func (m *MockUserRepository) CreateUser(ctx context.Context, user *User) error {
	args := m.Called(ctx, user)
	return args.Error(0)
}

func (m *MockUserRepository) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	args := m.Called(ctx, email)
	user, _ := args.Get(0).(*User)
	return user, args.Error(1)
}

func (m *MockUserRepository) DeleteUser(ctx context.Context, email string) error {
	args := m.Called(ctx, email)
	return args.Error(0)
}

// Helper to generate a valid JWT for testing
func generateTestToken(email string) (string, error) {
	// In tests, jwtSecret might not be initialized by setupDependencies.
	// Initialize it here if it's nil.
	if jwtSecret == nil {
		jwtSecret = []byte("test-secret")
	}
	expirationTime := time.Now().Add(1 * time.Hour)
	claims := jwt.MapClaims{
		"email": email,
		"exp":   expirationTime.Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

// --- Tests ---

func TestHandleOrders_Success(t *testing.T) {
	// --- Setup ---
	mockRedis := new(MockRedisClient)
	mockChannel := new(MockRabbitChannel)
	mockUserRepo := new(MockUserRepository)

	// Create a mock ConnectionManager that returns our mock channel
	mockCM := &ConnectionManager{}
	mockCM.channel = mockChannel

	handler := &APIHandler{
		redisClient: mockRedis,
		cm:          mockCM,
		userRepo:    mockUserRepo,
	}

	testEmail := "authenticated@example.com"
	token, err := generateTestToken(testEmail)
	assert.NoError(t, err)

	// Define expectations
	mockRedis.On("SetNX", mock.Anything, "idemp:123", "1", 5*time.Minute).Return(true, nil)
	mockChannel.On("PublishWithContext", mock.Anything, "orders.direct", "", false, false, mock.Anything).Return(nil)

	body := `{"id":"123", "sku":"test-sku", "qty":10}`
	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	// --- Execute ---
	mux := http.NewServeMux()
	mux.Handle("/orders", authMiddleware(http.HandlerFunc(handler.handleOrders)))
	mux.ServeHTTP(rr, req)

	// --- Assert ---
	assert.Equal(t, http.StatusAccepted, rr.Code)
	mockRedis.AssertExpectations(t)
	mockChannel.AssertExpectations(t)

	publishedMsg := mockChannel.Calls[0].Arguments.Get(5).(amqp.Publishing)
	var publishedOrder Order
	json.Unmarshal(publishedMsg.Body, &publishedOrder)
	assert.Equal(t, testEmail, publishedOrder.Email)
	assert.Equal(t, testEmail, publishedOrder.UserEmail)
}

func TestHandleOrders_DuplicateOrder(t *testing.T) {
	mockRedis := new(MockRedisClient)
	mockChannel := new(MockRabbitChannel)
	mockUserRepo := new(MockUserRepository)
	mockCM := &ConnectionManager{channel: mockChannel}

	handler := &APIHandler{
		redisClient: mockRedis,
		cm:          mockCM,
		userRepo:    mockUserRepo,
	}

	testEmail := "authenticated@example.com"
	token, err := generateTestToken(testEmail)
	assert.NoError(t, err)

	mockRedis.On("SetNX", mock.Anything, "idemp:123", "1", 5*time.Minute).Return(false, nil)

	body := `{"id":"123", "sku":"test-sku", "qty":10}`
	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.Handle("/orders", authMiddleware(http.HandlerFunc(handler.handleOrders)))
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code)
	mockRedis.AssertExpectations(t)
	mockChannel.AssertNotCalled(t, "PublishWithContext", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestRegister_Success(t *testing.T) {
	mockUserRepo := new(MockUserRepository)
	handler := &APIHandler{userRepo: mockUserRepo}

	mockUserRepo.On("CreateUser", mock.Anything, mock.AnythingOfType("*main.User")).Return(nil)

	body := `{"email":"newuser@example.com","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/register", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.registerHandler(rr, req)

	assert.Equal(t, http.StatusCreated, rr.Code)
	mockUserRepo.AssertExpectations(t)
}

func TestLogin_Success(t *testing.T) {
	mockUserRepo := new(MockUserRepository)
	handler := &APIHandler{userRepo: mockUserRepo}

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte("correctpassword"), bcrypt.DefaultCost)
	mockUserRepo.On("GetUserByEmail", mock.Anything, "loginuser@example.com").Return(&User{
		Email:        "loginuser@example.com",
		PasswordHash: string(hashedPassword),
	}, nil)

	body := `{"email":"loginuser@example.com","password":"correctpassword"}`
	req := httptest.NewRequest(http.MethodPost, "/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.loginHandler(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var response map[string]string
	json.Unmarshal(rr.Body.Bytes(), &response)
	assert.Contains(t, response, "token")
}

func TestDeleteUser_Success(t *testing.T) {
	mockUserRepo := new(MockUserRepository)
	handler := &APIHandler{userRepo: mockUserRepo}

	testEmail := "user-to-delete@example.com"
	token, err := generateTestToken(testEmail)
	assert.NoError(t, err)

	mockUserRepo.On("DeleteUser", mock.Anything, testEmail).Return(nil)

	req := httptest.NewRequest(http.MethodDelete, "/delete-user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.Handle("/delete-user", authMiddleware(http.HandlerFunc(handler.deleteUserHandler)))
	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	mockUserRepo.AssertExpectations(t)
}
