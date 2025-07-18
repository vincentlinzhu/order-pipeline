package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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

func (m *MockRabbitChannel) PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp091.Publishing) error {
	args := m.Called(ctx, exchange, key, mandatory, immediate, msg)
	return args.Error(0)
}

// --- Tests ---

func TestHandleOrders_Success(t *testing.T) {
	// --- Setup ---
	mockRedis := new(MockRedisClient)
	mockChannel := new(MockRabbitChannel)
	handler := &APIHandler{
		redisClient:   mockRedis,
		rabbitChannel: mockChannel,
	}

	// Define expectations
	mockRedis.On("SetNX", mock.Anything, "idemp:123", "1", 5*time.Minute).Return(true, nil)
	mockChannel.On("PublishWithContext", mock.Anything, "orders.direct", "", false, false, mock.Anything).Return(nil)

	// Create request
	body := `{"id":"123", "email":"test@test.com", "sku":"test-sku", "qty":10}`
	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()

	// --- Execute ---
	handler.handleOrders(rr, req)

	// --- Assert ---
	assert.Equal(t, http.StatusAccepted, rr.Code)
	mockRedis.AssertExpectations(t)
	mockChannel.AssertExpectations(t)
}

func TestHandleOrders_DuplicateOrder(t *testing.T) {
	// --- Setup ---
	mockRedis := new(MockRedisClient)
	handler := &APIHandler{redisClient: mockRedis}

	// Define expectations: SetNX returns false, indicating a duplicate
	mockRedis.On("SetNX", mock.Anything, "idemp:123", "1", 5*time.Minute).Return(false, nil)

	// Create request
	body := `{"id":"123", "email":"test@test.com", "sku":"test-sku", "qty":10}`
	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()

	// --- Execute ---
	handler.handleOrders(rr, req)

	// --- Assert ---
	assert.Equal(t, http.StatusConflict, rr.Code)
	mockRedis.AssertExpectations(t)
}

func TestHandleOrders_InvalidJSON(t *testing.T) {
	handler := &APIHandler{}
	body := `{"id":123}` // Invalid JSON, id should be a string
	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()

	handler.handleOrders(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
