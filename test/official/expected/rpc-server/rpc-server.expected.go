// Package "rpcServer" provides primitives to interact with the AsyncAPI specification.
//
// Code generated by github.com/lerenn/asyncapi-codegen version (devel) DO NOT EDIT.
package rpcServer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiContext "github.com/lerenn/asyncapi-codegen/pkg/context"
	"github.com/lerenn/asyncapi-codegen/pkg/log"
	"github.com/lerenn/asyncapi-codegen/pkg/middleware"

	"github.com/google/uuid"
)

// AppSubscriber represents all handlers that are expecting messages for App
type AppSubscriber interface {
	// RpcQueue
	RpcQueue(ctx context.Context, msg RpcQueueMessage, done bool)
}

// AppController is the structure that provides publishing capabilities to the
// developer and and connect the broker with the App
type AppController struct {
	brokerController BrokerController
	stopSubscribers  map[string]chan interface{}
	logger           log.Interface
	middlewares      []middleware.Middleware
}

// NewAppController links the App to the broker
func NewAppController(bs BrokerController) (*AppController, error) {
	if bs == nil {
		return nil, ErrNilBrokerController
	}

	return &AppController{
		brokerController: bs,
		stopSubscribers:  make(map[string]chan interface{}),
		logger:           log.Silent{},
		middlewares:      make([]middleware.Middleware, 0),
	}, nil
}

// SetLogger attaches a logger that will log operations on controller
func (c *AppController) SetLogger(logger log.Interface) {
	c.logger = logger
	c.brokerController.SetLogger(logger)
}

// AddMiddlewares attaches middlewares that will be executed when sending or
// receiving messages
func (c *AppController) AddMiddlewares(middleware ...middleware.Middleware) {
	c.middlewares = append(c.middlewares, middleware...)
}

func (c AppController) wrapMiddlewares(middlewares []middleware.Middleware, last middleware.Next) func(ctx context.Context) {
	var called bool

	// If there is no more middleware
	if len(middlewares) == 0 {
		return func(ctx context.Context) {
			if !called {
				called = true
				last(ctx)
			}
		}
	}

	// Wrap middleware into a check function that will call execute the middleware
	// and call the next wrapped middleware if the returned function has not been
	// called already
	next := c.wrapMiddlewares(middlewares[1:], last)
	return func(ctx context.Context) {
		// Call the middleware and the following if it has not been done already
		if !called {
			called = true
			ctx = middlewares[0](ctx, next)

			// If next has already been called in middleware, it should not be
			// executed again
			next(ctx)
		}
	}
}

func (c AppController) executeMiddlewares(ctx context.Context, callback func(ctx context.Context)) {
	// Wrap middleware to have 'next' function when calling them
	wrapped := c.wrapMiddlewares(c.middlewares, callback)

	// Execute wrapped middlewares
	wrapped(ctx)
}

func addAppContextValues(ctx context.Context, path string) context.Context {
	ctx = context.WithValue(ctx, apiContext.KeyIsProvider, "app")
	return context.WithValue(ctx, apiContext.KeyIsChannel, path)
}

// Close will clean up any existing resources on the controller
func (c *AppController) Close(ctx context.Context) {
	// Unsubscribing remaining channels
	c.UnsubscribeAll(ctx)
	c.logger.Info(ctx, "Closed app controller")
}

// SubscribeAll will subscribe to channels without parameters on which the app is expecting messages.
// For channels with parameters, they should be subscribed independently.
func (c *AppController) SubscribeAll(ctx context.Context, as AppSubscriber) error {
	if as == nil {
		return ErrNilAppSubscriber
	}

	if err := c.SubscribeRpcQueue(ctx, as.RpcQueue); err != nil {
		return err
	}

	return nil
}

// UnsubscribeAll will unsubscribe all remaining subscribed channels
func (c *AppController) UnsubscribeAll(ctx context.Context) {
	// Unsubscribe channels with no parameters (if any)
	c.UnsubscribeRpcQueue(ctx)

	// Unsubscribe remaining channels
	for n, stopChan := range c.stopSubscribers {
		stopChan <- true
		delete(c.stopSubscribers, n)
	}
}

// SubscribeRpcQueue will subscribe to new messages from 'rpc_queue' channel.
//
// Callback function 'fn' will be called each time a new message is received.
// The 'done' argument indicates when the subscription is canceled and can be
// used to clean up resources.
func (c *AppController) SubscribeRpcQueue(ctx context.Context, fn func(ctx context.Context, msg RpcQueueMessage, done bool)) error {
	// Get channel path
	path := "rpc_queue"

	// Set context
	ctx = addAppContextValues(ctx, path)

	// Check if there is already a subscription
	_, exists := c.stopSubscribers[path]
	if exists {
		err := fmt.Errorf("%w: %q channel is already subscribed", ErrAlreadySubscribedChannel, path)
		c.logger.Error(ctx, err.Error())
		return err
	}

	// Subscribe to broker channel
	msgs, stop, err := c.brokerController.Subscribe(ctx, path)
	if err != nil {
		c.logger.Error(ctx, err.Error())
		return err
	}
	c.logger.Info(ctx, "Subscribed to channel")

	// Asynchronously listen to new messages and pass them to app subscriber
	go func() {
		for {
			// Wait for next message
			um, open := <-msgs

			// Add correlation ID to context if it exists
			if um.CorrelationID != nil {
				ctx = context.WithValue(ctx, apiContext.KeyIsCorrelationID, *um.CorrelationID)
			}

			// Process message
			msg, err := newRpcQueueMessageFromUniversalMessage(um)
			if err != nil {
				ctx = context.WithValue(ctx, apiContext.KeyIsMessage, um)
				c.logger.Error(ctx, err.Error())
			}

			// Add context
			msgCtx := context.WithValue(ctx, apiContext.KeyIsMessage, msg)
			msgCtx = context.WithValue(msgCtx, apiContext.KeyIsMessageDirection, "reception")

			// Process message if no error and still open
			if err == nil && open {
				// Execute middlewares with the callback
				c.executeMiddlewares(msgCtx, func(ctx context.Context) {
					fn(ctx, msg, !open)
				})
			}

			// If subscription is closed, then exit the function
			if !open {
				return
			}
		}
	}()

	// Add the stop channel to the inside map
	c.stopSubscribers[path] = stop

	return nil
}

// UnsubscribeRpcQueue will unsubscribe messages from 'rpc_queue' channel
func (c *AppController) UnsubscribeRpcQueue(ctx context.Context) {
	// Get channel path
	path := "rpc_queue"

	// Set context
	ctx = addAppContextValues(ctx, path)

	// Get stop channel
	stopChan, exists := c.stopSubscribers[path]
	if !exists {
		return
	}

	// Stop the channel and remove the entry
	stopChan <- true
	delete(c.stopSubscribers, path)

	c.logger.Info(ctx, "Unsubscribed from channel")
}

// PublishQueue will publish messages to '{queue}' channel
func (c *AppController) PublishQueue(ctx context.Context, params QueueParameters, msg QueueMessage) error {
	// Get channel path
	path := fmt.Sprintf("%v", params.Queue)

	// Set context
	ctx = addAppContextValues(ctx, path)
	ctx = context.WithValue(ctx, apiContext.KeyIsMessage, msg)
	ctx = context.WithValue(ctx, apiContext.KeyIsMessageDirection, "publication")

	// Convert to UniversalMessage
	um, err := msg.toUniversalMessage()
	if err != nil {
		return err
	}

	// Add correlation ID to context if it exists
	if um.CorrelationID != nil {
		ctx = context.WithValue(ctx, apiContext.KeyIsCorrelationID, *um.CorrelationID)
	}

	// Publish the message on event-broker through middlewares
	c.executeMiddlewares(ctx, func(ctx context.Context) {
		err = c.brokerController.Publish(ctx, path, um)
	})

	// Return error from publication on broker
	return err
}

// ClientSubscriber represents all handlers that are expecting messages for Client
type ClientSubscriber interface {
	// Queue
	Queue(ctx context.Context, msg QueueMessage, done bool)
}

// ClientController is the structure that provides publishing capabilities to the
// developer and and connect the broker with the Client
type ClientController struct {
	brokerController BrokerController
	stopSubscribers  map[string]chan interface{}
	logger           log.Interface
	middlewares      []middleware.Middleware
}

// NewClientController links the Client to the broker
func NewClientController(bs BrokerController) (*ClientController, error) {
	if bs == nil {
		return nil, ErrNilBrokerController
	}

	return &ClientController{
		brokerController: bs,
		stopSubscribers:  make(map[string]chan interface{}),
		logger:           log.Silent{},
		middlewares:      make([]middleware.Middleware, 0),
	}, nil
}

// SetLogger attaches a logger that will log operations on controller
func (c *ClientController) SetLogger(logger log.Interface) {
	c.logger = logger
	c.brokerController.SetLogger(logger)
}

// AddMiddlewares attaches middlewares that will be executed when sending or
// receiving messages
func (c *ClientController) AddMiddlewares(middleware ...middleware.Middleware) {
	c.middlewares = append(c.middlewares, middleware...)
}

func (c ClientController) wrapMiddlewares(middlewares []middleware.Middleware, last middleware.Next) func(ctx context.Context) {
	var called bool

	// If there is no more middleware
	if len(middlewares) == 0 {
		return func(ctx context.Context) {
			if !called {
				called = true
				last(ctx)
			}
		}
	}

	// Wrap middleware into a check function that will call execute the middleware
	// and call the next wrapped middleware if the returned function has not been
	// called already
	next := c.wrapMiddlewares(middlewares[1:], last)
	return func(ctx context.Context) {
		// Call the middleware and the following if it has not been done already
		if !called {
			called = true
			ctx = middlewares[0](ctx, next)

			// If next has already been called in middleware, it should not be
			// executed again
			next(ctx)
		}
	}
}

func (c ClientController) executeMiddlewares(ctx context.Context, callback func(ctx context.Context)) {
	// Wrap middleware to have 'next' function when calling them
	wrapped := c.wrapMiddlewares(c.middlewares, callback)

	// Execute wrapped middlewares
	wrapped(ctx)
}

func addClientContextValues(ctx context.Context, path string) context.Context {
	ctx = context.WithValue(ctx, apiContext.KeyIsProvider, "client")
	return context.WithValue(ctx, apiContext.KeyIsChannel, path)
}

// Close will clean up any existing resources on the controller
func (c *ClientController) Close(ctx context.Context) {
	// Unsubscribing remaining channels
	c.UnsubscribeAll(ctx)
	c.logger.Info(ctx, "Closed client controller")
}

// SubscribeAll will subscribe to channels without parameters on which the app is expecting messages.
// For channels with parameters, they should be subscribed independently.
func (c *ClientController) SubscribeAll(ctx context.Context, as ClientSubscriber) error {
	if as == nil {
		return ErrNilClientSubscriber
	}

	return nil
}

// UnsubscribeAll will unsubscribe all remaining subscribed channels
func (c *ClientController) UnsubscribeAll(ctx context.Context) {
	// Unsubscribe channels with no parameters (if any)

	// Unsubscribe remaining channels
	for n, stopChan := range c.stopSubscribers {
		stopChan <- true
		delete(c.stopSubscribers, n)
	}
}

// SubscribeQueue will subscribe to new messages from '{queue}' channel.
//
// Callback function 'fn' will be called each time a new message is received.
// The 'done' argument indicates when the subscription is canceled and can be
// used to clean up resources.
func (c *ClientController) SubscribeQueue(ctx context.Context, params QueueParameters, fn func(ctx context.Context, msg QueueMessage, done bool)) error {
	// Get channel path
	path := fmt.Sprintf("%v", params.Queue)

	// Set context
	ctx = addClientContextValues(ctx, path)

	// Check if there is already a subscription
	_, exists := c.stopSubscribers[path]
	if exists {
		err := fmt.Errorf("%w: %q channel is already subscribed", ErrAlreadySubscribedChannel, path)
		c.logger.Error(ctx, err.Error())
		return err
	}

	// Subscribe to broker channel
	msgs, stop, err := c.brokerController.Subscribe(ctx, path)
	if err != nil {
		c.logger.Error(ctx, err.Error())
		return err
	}
	c.logger.Info(ctx, "Subscribed to channel")

	// Asynchronously listen to new messages and pass them to app subscriber
	go func() {
		for {
			// Wait for next message
			um, open := <-msgs

			// Add correlation ID to context if it exists
			if um.CorrelationID != nil {
				ctx = context.WithValue(ctx, apiContext.KeyIsCorrelationID, *um.CorrelationID)
			}

			// Process message
			msg, err := newQueueMessageFromUniversalMessage(um)
			if err != nil {
				ctx = context.WithValue(ctx, apiContext.KeyIsMessage, um)
				c.logger.Error(ctx, err.Error())
			}

			// Add context
			msgCtx := context.WithValue(ctx, apiContext.KeyIsMessage, msg)
			msgCtx = context.WithValue(msgCtx, apiContext.KeyIsMessageDirection, "reception")

			// Process message if no error and still open
			if err == nil && open {
				// Execute middlewares with the callback
				c.executeMiddlewares(msgCtx, func(ctx context.Context) {
					fn(ctx, msg, !open)
				})
			}

			// If subscription is closed, then exit the function
			if !open {
				return
			}
		}
	}()

	// Add the stop channel to the inside map
	c.stopSubscribers[path] = stop

	return nil
}

// UnsubscribeQueue will unsubscribe messages from '{queue}' channel
func (c *ClientController) UnsubscribeQueue(ctx context.Context, params QueueParameters) {
	// Get channel path
	path := fmt.Sprintf("%v", params.Queue)

	// Set context
	ctx = addClientContextValues(ctx, path)

	// Get stop channel
	stopChan, exists := c.stopSubscribers[path]
	if !exists {
		return
	}

	// Stop the channel and remove the entry
	stopChan <- true
	delete(c.stopSubscribers, path)

	c.logger.Info(ctx, "Unsubscribed from channel")
}

// PublishRpcQueue will publish messages to 'rpc_queue' channel
func (c *ClientController) PublishRpcQueue(ctx context.Context, msg RpcQueueMessage) error {
	// Get channel path
	path := "rpc_queue"

	// Set context
	ctx = addClientContextValues(ctx, path)
	ctx = context.WithValue(ctx, apiContext.KeyIsMessage, msg)
	ctx = context.WithValue(ctx, apiContext.KeyIsMessageDirection, "publication")

	// Convert to UniversalMessage
	um, err := msg.toUniversalMessage()
	if err != nil {
		return err
	}

	// Add correlation ID to context if it exists
	if um.CorrelationID != nil {
		ctx = context.WithValue(ctx, apiContext.KeyIsCorrelationID, *um.CorrelationID)
	}

	// Publish the message on event-broker through middlewares
	c.executeMiddlewares(ctx, func(ctx context.Context) {
		err = c.brokerController.Publish(ctx, path, um)
	})

	// Return error from publication on broker
	return err
}

// WaitForQueue will wait for a specific message by its correlation ID
//
// The pub function is the publication function that should be used to send the message
// It will be called after subscribing to the channel to avoid race condition, and potentially loose the message
func (cc *ClientController) WaitForQueue(ctx context.Context, params QueueParameters, publishMsg MessageWithCorrelationID, pub func(ctx context.Context) error) (QueueMessage, error) {
	// Get channel path
	path := fmt.Sprintf("%v", params.Queue)

	// Set context
	ctx = addClientContextValues(ctx, path)

	// Subscribe to broker channel
	msgs, stop, err := cc.brokerController.Subscribe(ctx, path)
	if err != nil {
		cc.logger.Error(ctx, err.Error())
		return QueueMessage{}, err
	}
	cc.logger.Info(ctx, "Subscribed to channel")

	// Close subscriber on leave
	defer func() {
		// Unsubscribe
		stop <- true

		// Logging unsubscribing
		cc.logger.Info(ctx, "Unsubscribed from channel")
	}()

	// Execute callback for publication
	if err = pub(ctx); err != nil {
		return QueueMessage{}, err
	}

	// Wait for corresponding response
	for {
		select {
		case um, open := <-msgs:
			// Get new message
			msg, err := newQueueMessageFromUniversalMessage(um)
			if err != nil {
				cc.logger.Error(ctx, err.Error())
			}

			// If valid message with corresponding correlation ID, return message
			if err == nil && publishMsg.CorrelationID() == msg.CorrelationID() {
				// Set context with received values
				msgCtx := context.WithValue(ctx, apiContext.KeyIsMessage, msg)
				msgCtx = context.WithValue(msgCtx, apiContext.KeyIsMessageDirection, "reception")
				msgCtx = context.WithValue(msgCtx, apiContext.KeyIsCorrelationID, publishMsg.CorrelationID())

				// Execute middlewares before returning
				cc.executeMiddlewares(msgCtx, func(_ context.Context) {
					/* Nothing to do more */
				})

				return msg, nil
			} else if !open { // If message is invalid or not corresponding and the subscription is closed, then set corresponding error
				cc.logger.Error(ctx, "Channel closed before getting message")
				return QueueMessage{}, ErrSubscriptionCanceled
			}
		case <-ctx.Done(): // Set corrsponding error if context is done
			cc.logger.Error(ctx, "Context done before getting message")
			return QueueMessage{}, ErrContextCanceled
		}
	}
}

const (
	// CorrelationIDField is the name of the field that will contain the correlation ID
	CorrelationIDField = "correlation_id"
)

// UniversalMessage is a wrapper that will contain all information regarding a message
type UniversalMessage struct {
	CorrelationID *string
	Payload       []byte
}

// BrokerController represents the functions that should be implemented to connect
// the broker to the application or the client
type BrokerController interface {
	// SetLogger set a logger that will log operations on broker controller
	SetLogger(logger log.Interface)

	// Publish a message to the broker
	Publish(ctx context.Context, channel string, mw UniversalMessage) error

	// Subscribe to messages from the broker
	Subscribe(ctx context.Context, channel string) (msgs chan UniversalMessage, stop chan interface{}, err error)

	// SetQueueName sets the name of the queue that will be used by the broker
	SetQueueName(name string)
}

var (
	// Generic error for AsyncAPI generated code
	ErrAsyncAPI = errors.New("error when using AsyncAPI")

	// ErrContextCanceled is given when a given context is canceled
	ErrContextCanceled = fmt.Errorf("%w: context canceled", ErrAsyncAPI)

	// ErrNilBrokerController is raised when a nil broker controller is user
	ErrNilBrokerController = fmt.Errorf("%w: nil broker controller has been used", ErrAsyncAPI)

	// ErrNilAppSubscriber is raised when a nil app subscriber is user
	ErrNilAppSubscriber = fmt.Errorf("%w: nil app subscriber has been used", ErrAsyncAPI)

	// ErrNilClientSubscriber is raised when a nil client subscriber is user
	ErrNilClientSubscriber = fmt.Errorf("%w: nil client subscriber has been used", ErrAsyncAPI)

	// ErrAlreadySubscribedChannel is raised when a subscription is done twice
	// or more without unsubscribing
	ErrAlreadySubscribedChannel = fmt.Errorf("%w: the channel has already been subscribed", ErrAsyncAPI)

	// ErrSubscriptionCanceled is raised when expecting something and the subscription has been canceled before it happens
	ErrSubscriptionCanceled = fmt.Errorf("%w: the subscription has been canceled", ErrAsyncAPI)
)

type MessageWithCorrelationID interface {
	CorrelationID() string
}

type Error struct {
	Channel string
	Err     error
}

func (e *Error) Error() string {
	return fmt.Sprintf("channel %q: err %v", e.Channel, e.Err)
}

// RpcQueueMessage is the message expected for 'RpcQueue' channel
type RpcQueueMessage struct {
	// Headers will be used to fill the message headers
	Headers struct {
		CorrelationID *string `json:"correlation_id"`
	}

	// Payload will be inserted in the message payload
	Payload struct {
		Numbers []float64 `json:"numbers"`
	}
}

func NewRpcQueueMessage() RpcQueueMessage {
	var msg RpcQueueMessage

	// Set correlation ID
	u := uuid.New().String()
	msg.Headers.CorrelationID = &u

	return msg
}

// newRpcQueueMessageFromUniversalMessage will fill a new RpcQueueMessage with data from UniversalMessage
func newRpcQueueMessageFromUniversalMessage(um UniversalMessage) (RpcQueueMessage, error) {
	var msg RpcQueueMessage

	// Unmarshal payload to expected message payload format
	err := json.Unmarshal(um.Payload, &msg.Payload)
	if err != nil {
		return msg, err
	}

	// Get correlation ID
	msg.Headers.CorrelationID = um.CorrelationID

	// TODO: run checks on msg type

	return msg, nil
}

// toUniversalMessage will generate an UniversalMessage from RpcQueueMessage data
func (msg RpcQueueMessage) toUniversalMessage() (UniversalMessage, error) {
	// TODO: implement checks on message

	// Marshal payload to JSON
	payload, err := json.Marshal(msg.Payload)
	if err != nil {
		return UniversalMessage{}, err
	}

	// Set correlation ID if it does not exist
	var correlationID *string
	if msg.Headers.CorrelationID != nil {
		correlationID = msg.Headers.CorrelationID
	} else {
		u := uuid.New().String()
		correlationID = &u
	}

	return UniversalMessage{
		Payload:       payload,
		CorrelationID: correlationID,
	}, nil
}

// CorrelationID will give the correlation ID of the message, based on AsyncAPI spec
func (msg RpcQueueMessage) CorrelationID() string {
	if msg.Headers.CorrelationID != nil {
		return *msg.Headers.CorrelationID
	}

	return ""
}

// SetAsResponseFrom will correlate the message with the one passed in parameter.
// It will assign the 'req' message correlation ID to the message correlation ID,
// both specified in AsyncAPI spec.
func (msg *RpcQueueMessage) SetAsResponseFrom(req MessageWithCorrelationID) {
	id := req.CorrelationID()
	msg.Headers.CorrelationID = &id
}

// QueueParameters represents Queue channel parameters
type QueueParameters struct {
	Queue string
}

// QueueMessage is the message expected for 'Queue' channel
type QueueMessage struct {
	// Headers will be used to fill the message headers
	Headers struct {
		CorrelationID *string `json:"correlation_id"`
	}

	// Payload will be inserted in the message payload
	Payload struct {
		Result *float64 `json:"result"`
	}
}

func NewQueueMessage() QueueMessage {
	var msg QueueMessage

	// Set correlation ID
	u := uuid.New().String()
	msg.Headers.CorrelationID = &u

	return msg
}

// newQueueMessageFromUniversalMessage will fill a new QueueMessage with data from UniversalMessage
func newQueueMessageFromUniversalMessage(um UniversalMessage) (QueueMessage, error) {
	var msg QueueMessage

	// Unmarshal payload to expected message payload format
	err := json.Unmarshal(um.Payload, &msg.Payload)
	if err != nil {
		return msg, err
	}

	// Get correlation ID
	msg.Headers.CorrelationID = um.CorrelationID

	// TODO: run checks on msg type

	return msg, nil
}

// toUniversalMessage will generate an UniversalMessage from QueueMessage data
func (msg QueueMessage) toUniversalMessage() (UniversalMessage, error) {
	// TODO: implement checks on message

	// Marshal payload to JSON
	payload, err := json.Marshal(msg.Payload)
	if err != nil {
		return UniversalMessage{}, err
	}

	// Set correlation ID if it does not exist
	var correlationID *string
	if msg.Headers.CorrelationID != nil {
		correlationID = msg.Headers.CorrelationID
	} else {
		u := uuid.New().String()
		correlationID = &u
	}

	return UniversalMessage{
		Payload:       payload,
		CorrelationID: correlationID,
	}, nil
}

// CorrelationID will give the correlation ID of the message, based on AsyncAPI spec
func (msg QueueMessage) CorrelationID() string {
	if msg.Headers.CorrelationID != nil {
		return *msg.Headers.CorrelationID
	}

	return ""
}

// SetAsResponseFrom will correlate the message with the one passed in parameter.
// It will assign the 'req' message correlation ID to the message correlation ID,
// both specified in AsyncAPI spec.
func (msg *QueueMessage) SetAsResponseFrom(req MessageWithCorrelationID) {
	id := req.CorrelationID()
	msg.Headers.CorrelationID = &id
}
