module codeberg.org/example/snapvault

go 1.26.0

// Run `go mod tidy` once after cloning to resolve transitive dependencies and
// generate go.sum. The versions below are the direct dependencies; tidy will
// pin them and fill in the rest.

require (
	cloud.google.com/go/storage v1.50.0
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace v1.27.0
	github.com/jackc/pgx/v5 v5.7.2
	github.com/nats-io/nats.go v1.39.1
	github.com/prometheus/client_golang v1.20.5
	go.mongodb.org/mongo-driver/v2 v2.1.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.59.0
	go.opentelemetry.io/otel v1.34.0
	go.opentelemetry.io/otel/sdk v1.34.0
	golang.org/x/image v0.24.0
)

// Test-only dependencies (Testcontainers spins up real Postgres/NATS in Docker).
require (
	github.com/testcontainers/testcontainers-go v0.34.0
	github.com/testcontainers/testcontainers-go/modules/nats v0.34.0
	github.com/testcontainers/testcontainers-go/modules/postgres v0.34.0
)
