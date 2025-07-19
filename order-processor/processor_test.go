package main

import (
	"context"
	"encoding/json"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/mock"
)

// --- Mocks ---

type MockRedisClient struct {
	mock.Mock
}

func (m *MockRedisClient) DecrBy(ctx context.Context, key string, decrement int64) *redis.IntCmd {
	args := m.Called(ctx, key, decrement)
	cmd := redis.NewIntCmd(ctx)
	if args.Error(0) != nil {
		cmd.SetErr(args.Error(0))
	}
	return cmd
}

func (m *MockRedisClient) Incr(ctx context.Context, key string) *redis.IntCmd {
	args := m.Called(ctx, key)
	cmd := redis.NewIntCmd(ctx)
	if args.Error(0) != nil {
		cmd.SetErr(args.Error(0))
	}
	return cmd
}

// MockRabbitChannel now implements the full RabbitChannel interface.
type MockRabbitChannel struct {
	mock.Mock
}

func (m *MockRabbitChannel) PublishWithContext(ctx context.Context, exchange, key string, mandatory, immediate bool, msg amqp.Publishing) error {
	args := m.Called(ctx, exchange, key, mandatory, immediate, msg)
	return args.Error(0)
}

func (m *MockRabbitChannel) Qos(prefetchCount, prefetchSize int, global bool) error {
	args := m.Called(prefetchCount, prefetchSize, global)
	return args.Error(0)
}

func (m *MockRabbitChannel) Consume(queue, consumer string, autoAck, exclusive, noLocal, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error) {
	callArgs := m.Called(queue, consumer, autoAck, exclusive, noLocal, noWait, args)
	var deliveryChan chan amqp.Delivery
	if c, ok := callArgs.Get(0).(chan amqp.Delivery); ok {
		deliveryChan = c
	}
	return deliveryChan, callArgs.Error(1)
}

func (m *MockRabbitChannel) ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error {
	callArgs := m.Called(name, kind, durable, autoDelete, internal, noWait, args)
	return callArgs.Error(0)
}

func (m *MockRabbitChannel) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	callArgs := m.Called(name, durable, autoDelete, exclusive, noWait, args)
	// Ensure we return a valid amqp.Queue struct even if it's empty
	var queue amqp.Queue
	if q, ok := callArgs.Get(0).(amqp.Queue); ok {
		queue = q
	}
	return queue, callArgs.Error(1)
}

func (m *MockRabbitChannel) QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error {
	callArgs := m.Called(name, key, exchange, noWait, args)
	return callArgs.Error(0)
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

func TestProcessOrder_Success(t *testing.T) {
	// --- Setup ---
	mockRedis := new(MockRedisClient)
	mockChannel := new(MockRabbitChannel)
	processor := &Processor{
		redisClient:   mockRedis,
		rabbitChannel: mockChannel,
	}

	// Define expectations
	mockRedis.On("DecrBy", mock.Anything, "inventory:test-sku", int64(10)).Return(nil)
	mockRedis.On("Incr", mock.Anything, "orders.processed.count").Return(nil)
	mockChannel.On("PublishWithContext", mock.Anything, "orders.topic", "order.created", false, false, mock.Anything).Return(nil)

	order := Order{ID: "123", Email: "test@test.com", SKU: "test-sku", Qty: 10}
	body, _ := json.Marshal(order)
	mockAcker := &MockAcker{}
	mockAcker.On("Ack", mock.AnythingOfType("uint64"), false).Return(nil)
	delivery := amqp.Delivery{Body: body, Acknowledger: mockAcker}

	// --- Execute ---
	processor.processOrder(delivery)

	// --- Assert ---
	mockRedis.AssertExpectations(t)
	mockChannel.AssertExpectations(t)
	mockAcker.AssertExpectations(t)
}

func TestProcessOrder_UnmarshalError(t *testing.T) {
	processor := &Processor{}
	mockAcker := &MockAcker{}
	mockAcker.On("Nack", mock.AnythingOfType("uint64"), false, false).Return(nil)
	delivery := amqp.Delivery{Body: []byte("invalid json"), Acknowledger: mockAcker}

	processor.processOrder(delivery)

	mockAcker.AssertExpectations(t)
}