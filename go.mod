module github.com/llm-d/llm-d-batch-gateway

go 1.25.0

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/aws/aws-sdk-go-v2 v1.42.1
	github.com/aws/aws-sdk-go-v2/config v1.32.29
	github.com/aws/aws-sdk-go-v2/credentials v1.19.28
	github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.22.32
	github.com/aws/aws-sdk-go-v2/service/s3 v1.105.0
	github.com/cenkalti/backoff/v5 v5.0.3
	github.com/exaring/otelpgx v0.11.1
	github.com/go-resty/resty/v2 v2.17.2
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/llm-d-incubation/llm-d-async/api v0.7.2
	github.com/llm-d-incubation/llm-d-async/producer v0.7.2
	github.com/pashagolub/pgxmock/v4 v4.9.0
	github.com/prometheus/client_golang v1.23.2
	github.com/quasilyte/go-ruleguard/dsl v0.3.23
	github.com/redis/go-redis/extra/redisotel/v9 v9.21.0
	github.com/redis/go-redis/v9 v9.21.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.44.0
	go.opentelemetry.io/otel/sdk v1.44.0
	go.opentelemetry.io/otel/trace v1.44.0
	golang.org/x/sync v0.22.0
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/klog/v2 v2.140.0
)

require (
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/redis/go-redis/extra/rediscmd/v9 v9.21.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260630182238-925bb5da69e7 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260630182238-925bb5da69e7 // indirect
	google.golang.org/grpc v1.82.0 // indirect
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.14 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.30 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.30 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.31 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.4.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.32.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.37.0 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.44.0 // indirect
	github.com/aws/smithy-go v1.27.3 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.2
	github.com/prometheus/common v0.69.0 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
