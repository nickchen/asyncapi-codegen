//go:generate go run ../../../../../cmd/asyncapi-codegen -g application,types -p main -i ../../asyncapi.yaml -o ./app.gen.go

package main

import (
	"context"
	"time"

	"github.com/lerenn/asyncapi-codegen/examples"
	"github.com/lerenn/asyncapi-codegen/pkg/extensions/brokers/kafka"
	"github.com/lerenn/asyncapi-codegen/pkg/extensions/loggers"
	"github.com/lerenn/asyncapi-codegen/pkg/extensions/middlewares"
	testutil "github.com/lerenn/asyncapi-codegen/pkg/utils/test"
)

var _ AppSubscriber = (*Subscriber)(nil)

type Subscriber struct {
	Controller *AppController
}

func (s Subscriber) Ping(ctx context.Context, req PingMessage) error {
	// Generate a pong message, set as a response of the request
	resp := NewPongMessage()
	resp.SetAsResponseFrom(&req)
	resp.Payload.Message = "pong"
	resp.Payload.Time = time.Now()

	// Publish the pong message
	// Note: it will indefinitely wait to publish as context has no timeout
	err := s.Controller.PublishPong(ctx, resp)
	if err != nil {
		panic(err)
	}

	return nil
}

func main() {
	// Get broker address based on the environment, it will returns an address like "kafka:9092"
	// Note: this is not needed in your application, you can directly use the address
	addr := testutil.BrokerAddress(testutil.BrokerAddressParams{
		DockerizedAddr: "kafka",
		Port:           "9092",
	})

	// Instantiate a Kafka controller with a logger
	logger := loggers.NewText()
	broker, err := kafka.NewController(
		[]string{addr},                 // List of hosts
		kafka.WithLogger(logger),       // Attach an internal logger
		kafka.WithGroupID("ping-apps"), // Change group id
	)
	if err != nil {
		panic(err)
	}

	// Create a new app controller
	ctrl, err := NewAppController(
		broker,             // Attach the kafka controller
		WithLogger(logger), // Attach an internal logger
		WithMiddlewares(middlewares.Logging(logger))) // Attach a middleware to log messages
	if err != nil {
		panic(err)
	}
	defer ctrl.Close(context.Background())

	// Subscribe to all (we could also have just listened on the ping request channel)
	sub := Subscriber{Controller: ctrl}
	if err := ctrl.SubscribeAll(context.Background(), sub); err != nil {
		panic(err)
	}

	// Listen on port to let know that app is ready
	examples.ListenLocalPort(1234)
}
