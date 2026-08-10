module github.com/bridgecore/bridgecore

go 1.22.2

replace (
	go.uber.org/multierr => github.com/uber-go/multierr v1.11.0
	go.uber.org/zap => github.com/uber-go/zap v1.27.0
	golang.org/x/crypto => github.com/golang/crypto v0.23.0
	golang.org/x/net => github.com/golang/net v0.25.0
	golang.org/x/sys => github.com/golang/sys v0.20.0
	golang.org/x/text => github.com/golang/text v0.15.0
)

require (
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/joho/godotenv v1.5.1
	github.com/lib/pq v1.12.3
	github.com/redis/go-redis/v9 v9.5.1
	go.uber.org/zap v1.27.0
	golang.org/x/crypto v0.23.0
)

require (
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	go.uber.org/multierr v1.10.0 // indirect
)
