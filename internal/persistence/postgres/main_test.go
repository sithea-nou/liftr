package postgres_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMain(m *testing.M) {
	if os.Getenv("LIFTR_TEST_DATABASE_URL") == "" && os.Getenv("LIFTR_NO_TESTCONTAINERS") == "" {
		ctx := context.Background()
		pgContainer, err := postgres.Run(ctx,
			"postgres:17-alpine",
			postgres.WithDatabase("liftr"),
			postgres.WithUsername("postgres"),
			postgres.WithPassword("liftr"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(30*time.Second)),
		)
		if err == nil {
			url, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
			if err == nil {
				os.Setenv("LIFTR_TEST_DATABASE_URL", url)
				code := m.Run()
				pgContainer.Terminate(ctx)
				os.Exit(code)
			}
			pgContainer.Terminate(ctx)
		} else {
			log.Printf("testcontainers-go failed to start postgres: %v. Tests requiring DB will be skipped.", err)
		}
	}
	
	os.Exit(m.Run())
}
