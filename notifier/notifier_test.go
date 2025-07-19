package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	amqp "github.com/rabbitmq/amqp091-go"
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

func (m *MockRedisClient) HSet(ctx context.Context, key string, values ...interface{}) *redis.IntCmd {
	args := m.Called(ctx, key, values)
	cmd := redis.NewIntCmd(ctx)
	if args.Error(0) != nil {
		cmd.SetErr(args.Error(0))
	}
	return cmd
}

// MockAcker correctly implements the amqp.Acknowledger interface for testing.
type MockAcker struct {
	mock.Mock
}

func (m *MockAcker) Ack(tag uint64, multiple bool) error {
	args := m.Called(tag, multiple)
	return args.Error(0)
}

func (m *MockAcker) Nack(tag uint64, multiple, requeue bool) error {
	args := m.Called(tag, multiple, requeue)
	return args.Error(0)
}

func (m *MockAcker) Reject(tag uint64, requeue bool) error {
	args := m.Called(tag, requeue)
	return args.Error(0)
}

// --- Tests ---

func TestProcessNotification_Success(t *testing.T) {
	// --- Setup ---
	mockRedis := new(MockRedisClient)
	notifier := &Notifier{redisClient: mockRedis}

	// Define expectations
	mockRedis.On("SetNX", mock.Anything, "notif:123", "1", 24*time.Hour).Return(true, nil)
	// The HSet method receives a variadic ...interface{}, which the mock framework
	// treats as a slice. We must match that slice explicitly.
	mockRedis.On("HSet", mock.Anything, "notifications:123", []interface{}{"status", "sent"}).Return(nil)

	order := Order{ID: "123", Email: "test@test.com"}
	body, _ := json.Marshal(order)
	mockAcker := &MockAcker{}
	mockAcker.On("Ack", mock.AnythingOfType("uint64"), false).Return(nil)
	delivery := amqp.Delivery{Body: body, Acknowledger: mockAcker}

	// --- Execute ---
	notifier.processNotification(delivery)

	// --- Assert ---
	mockRedis.AssertExpectations(t)
	mockAcker.AssertExpectations(t)
}

func TestProcessNotification_Duplicate(t *testing.T) {
	// --- Setup ---
	mockRedis := new(MockRedisClient)
	notifier := &Notifier{redisClient: mockRedis}

	// Define expectations
	mockRedis.On("SetNX", mock.Anything, "notif:123", "1", 24*time.Hour).Return(false, nil)

	order := Order{ID: "123", Email: "test@test.com"}
	body, _ := json.Marshal(order)
	mockAcker := &MockAcker{}
	mockAcker.On("Ack", mock.AnythingOfType("uint64"), false).Return(nil)
	delivery := amqp.Delivery{Body: body, Acknowledger: mockAcker}

	// --- Execute ---
	notifier.processNotification(delivery)

	// --- Assert ---
	mockRedis.AssertExpectations(t)
	mockAcker.AssertExpectations(t)
	mockRedis.AssertNotCalled(t, "HSet", mock.Anything, mock.Anything, mock.Anything)
}
