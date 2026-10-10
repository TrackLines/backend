// Package testdb starts an isolated PostgreSQL instance for a Go test package.
package testdb

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Image is the PostgreSQL the tests run against: the supported major version, same as
// docker-compose.yml and production (Railway postgres-ssl:18). Bump them together.
const Image = "postgres:18-alpine"

// Run starts PostgreSQL, exposes its URL to the package tests, and terminates
// the container after the package's tests finish.
func Run(m *testing.M) int {
	startCtx, cancelStart := context.WithTimeout(context.Background(), 2*time.Minute)
	container, err := postgres.Run(startCtx, Image,
		postgres.WithDatabase("tracklines_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		cancelStart()
		_, _ = fmt.Fprintf(os.Stderr, "start test PostgreSQL container: %v\n", err)
		return 1
	}
	defer cancelStart()
	defer func() {
		terminateCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := container.Terminate(terminateCtx); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "terminate test PostgreSQL container: %v\n", err)
		}
	}()

	url, err := container.ConnectionString(startCtx, "sslmode=disable")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "get test PostgreSQL connection string: %v\n", err)
		return 1
	}
	if err := os.Setenv("TEST_DATABASE_URL", url); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "set TEST_DATABASE_URL: %v\n", err)
		return 1
	}
	return m.Run()
}
