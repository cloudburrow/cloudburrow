module github.com/cloudburrow/cloudburrow

go 1.26.7

require (
	cel.dev/cel-go v0.33.4
	cloud.google.com/go v0.123.0
	cloud.google.com/go/aiplatform v1.127.0
	cloud.google.com/go/apphub v1.4.1
	cloud.google.com/go/artifactregistry v1.27.0
	cloud.google.com/go/asset v1.30.1
	cloud.google.com/go/auth v0.24.1
	cloud.google.com/go/backupdr v1.18.0
	cloud.google.com/go/bigquery v1.85.0
	cloud.google.com/go/bigtable v1.58.0
	cloud.google.com/go/billing v1.28.1
	cloud.google.com/go/certificatemanager v1.17.1
	cloud.google.com/go/cloudbuild v1.35.0
	cloud.google.com/go/cloudquotas v1.14.1
	cloud.google.com/go/cloudsqlconn v1.24.0
	cloud.google.com/go/cloudtasks v1.20.0
	cloud.google.com/go/compute v1.62.0
	cloud.google.com/go/compute/metadata v0.10.0
	cloud.google.com/go/container v1.56.1
	cloud.google.com/go/containeranalysis v0.21.1
	cloud.google.com/go/dataform v1.9.0
	cloud.google.com/go/datastore v1.27.0
	cloud.google.com/go/developerconnect v1.2.0
	cloud.google.com/go/documentai v1.49.0
	cloud.google.com/go/eventarc v1.27.1
	cloud.google.com/go/firestore v1.26.0
	cloud.google.com/go/functions v1.26.0
	cloud.google.com/go/grafeas v0.6.1
	cloud.google.com/go/iam v1.14.0
	cloud.google.com/go/iap v1.19.1
	cloud.google.com/go/kms v1.35.0
	cloud.google.com/go/logging v1.20.0
	cloud.google.com/go/longrunning v1.5.0
	cloud.google.com/go/memorystore v1.7.0
	cloud.google.com/go/networksecurity v0.21.1
	cloud.google.com/go/observability v0.1.1
	cloud.google.com/go/orgpolicy v1.22.1
	cloud.google.com/go/parametermanager v1.2.1
	cloud.google.com/go/policytroubleshooter v1.18.1
	cloud.google.com/go/pubsub/v2 v2.7.0
	cloud.google.com/go/resourcemanager v1.17.0
	cloud.google.com/go/run v1.25.0
	cloud.google.com/go/scheduler v1.17.0
	cloud.google.com/go/secretmanager v1.22.0
	cloud.google.com/go/securesourcemanager v1.12.0
	cloud.google.com/go/security v1.29.1
	cloud.google.com/go/spanner v1.96.1
	cloud.google.com/go/speech v1.36.0
	cloud.google.com/go/storage v1.70.0
	cloud.google.com/go/storagebatchoperations v1.3.1
	cloud.google.com/go/storageinsights v1.10.1
	cloud.google.com/go/storagetransfer v1.21.1
	cloud.google.com/go/trace v1.18.1
	cloud.google.com/go/vpcaccess v1.16.1
	cloud.google.com/go/workflows v1.22.1
	github.com/Eyevinn/mp4ff v0.63.0
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace v1.39.0
	github.com/apache/arrow-go/v18 v18.8.0
	github.com/apache/iceberg-go v0.7.0
	github.com/beevik/etree v1.7.0
	github.com/chromedp/cdproto v0.158.4
	github.com/chromedp/chromedp v0.21.0
	github.com/cloudflare/circl v1.6.5
	github.com/creack/pty v1.1.24
	github.com/darui3018823/opus v1.8.0
	github.com/daulet/tokenizers v1.27.0
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1
	github.com/gen2brain/gav1d v0.2.7
	github.com/gen2brain/h265 v0.2.5
	github.com/go-sql-driver/mysql v1.10.1
	github.com/golang/geo v0.0.0-20261029132150-f51d36688936
	github.com/golang/protobuf v1.5.4
	github.com/google/differential-privacy/go/v4 v4.1.0
	github.com/google/go-containerregistry v0.22.1
	github.com/google/licensecheck v0.3.1
	github.com/google/uuid v1.6.0
	github.com/googleapis/gax-go/v2 v2.27.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/klippa-app/go-pdfium v1.21.4
	github.com/linkedin/goavro/v2 v2.15.0
	github.com/microsoft/go-mssqldb v1.10.0
	github.com/prometheus/client_golang v1.25.0
	github.com/prometheus/client_model v0.6.3
	github.com/prometheus/common v0.72.1
	github.com/prometheus/prometheus v0.316.0
	github.com/redis/go-redis/v9 v9.23.0
	github.com/robfig/cron/v3 v3.0.1
	github.com/russellhaering/goxmldsig v1.6.1
	github.com/scritchley/orc v0.0.0-20210513144143-06dddf1ad665
	github.com/tetratelabs/wazero v1.12.0
	github.com/tink-crypto/tink-go-gcpkms/v2 v2.4.0
	github.com/tink-crypto/tink-go/v2 v2.9.0
	github.com/yalue/onnxruntime_go v1.36.0
	go.mongodb.org/mongo-driver/v2 v2.10.0
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.72.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	go.opentelemetry.io/proto/otlp v1.11.0
	golang.org/x/image v0.47.0
	golang.org/x/mod v0.43.0
	golang.org/x/net v0.62.0
	golang.org/x/oauth2 v0.38.0
	golang.org/x/sys v0.49.0
	google.golang.org/api v0.314.0
	google.golang.org/genai v1.77.0
	google.golang.org/genproto v0.0.0-20261019180522-ce908d29299b
	google.golang.org/genproto/googleapis/api v0.0.0-20261019180522-ce908d29299b
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261104224025-d1e00e9f33ab
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cloud.google.com/go/accesscontextmanager v1.15.0 // indirect
	cloud.google.com/go/networkservices v1.8.1 // indirect
	cloud.google.com/go/osconfig v1.22.0 // indirect
	github.com/RoaringBitmap/roaring/v2 v2.26.0 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/apache/arrow/go/v15 v15.0.2 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/cockroachdb/apd/v3 v3.2.1 // indirect
	github.com/creasty/defaults v1.8.0 // indirect
	github.com/dennwc/varint v1.0.0 // indirect
	github.com/docker/cli v29.7.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.8 // indirect
	github.com/edsrzf/mmap-go v1.2.1-0.20241212181136-fad1cd13edbd // indirect
	github.com/facette/natsort v0.0.0-20181210072756-2cd4dd1e2dcb // indirect
	github.com/geoarrow/geoarrow-go v0.0.0-20260403143023-f54751c3e3a1 // indirect
	github.com/goccy/go-yaml v1.17.1 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/golang/glog v1.2.5 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/grafana/regexp v0.0.0-20250905093917-f7b3be9d1853 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jolestar/go-commons-pool/v2 v2.1.2 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/stretchr/testify v1.12.1 // indirect
	github.com/substrait-io/substrait v0.87.0 // indirect
	github.com/substrait-io/substrait-go/v8 v8.1.1 // indirect
	github.com/substrait-io/substrait-protobuf/go v0.85.0 // indirect
	github.com/twmb/avro v1.8.0 // indirect
	github.com/twmb/murmur3 v1.1.8 // indirect
	github.com/twpayne/go-geom v1.6.1 // indirect
	github.com/xdg-go/scram v1.4.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	gonum.org/v1/gonum v0.17.0 // indirect
)

require (
	cel.dev/expr v0.25.3 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.3.1 // indirect
	cloud.google.com/go/monitoring v1.31.0
	cloud.google.com/go/redis v1.27.0
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/GoogleCloudPlatform/grpc-gcp-go/grpcgcp v1.6.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/detectors/gcp v1.35.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric v0.57.0 // indirect
	github.com/GoogleCloudPlatform/opentelemetry-operations-go/internal/resourcemapping v0.63.0 // indirect
	github.com/andybalholm/brotli v1.2.7 // indirect
	github.com/apache/thrift v0.24.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cncf/xds/go v0.0.0-20260202195803-dba9d589def2
	github.com/envoyproxy/go-control-plane/envoy v1.40.0
	github.com/envoyproxy/protoc-gen-validate v1.3.3 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.9 // indirect
	github.com/go-json-experiment/json v0.0.0-20260623181947-01eb4420fa68 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/goccy/go-json v0.10.6 // indirect
	github.com/golang/snappy v1.0.0
	github.com/google/btree v1.1.3 // indirect
	github.com/google/flatbuffers v25.12.19+incompatible
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/s2a-go v0.1.11 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.25 // indirect
	github.com/gorilla/websocket v1.5.4-0.20250319132907-e064f32e3674
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.32.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/klauspost/compress v1.20.3
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pierrec/lz4/v4 v4.1.29
	github.com/planetscale/vtprotobuf v0.6.1-0.20240319094008-0393e58bdf10 // indirect
	github.com/spiffe/go-spiffe/v2 v2.8.1 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	go.opencensus.io v0.24.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/detectors/gcp v1.45.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.47.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/crypto v0.58.0
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/telemetry v0.0.0-20261009151841-97e8ea77aa2a // indirect
	golang.org/x/text v0.43.0
	golang.org/x/time v0.16.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
	golang.org/x/xerrors v0.0.0-20240903120638-7835f813f4da // indirect
	rsc.io/binaryregexp v0.2.0 // indirect
)

// genproto's googleapis/cloud/kms/v1, which tink-go-gcpkms v2.4.0's tests
// import (test/compat/kmstink_test.go), is gone from genproto after
// 2026-07-23, and gax-go v2.27.0 (#1788) and cloud.google.com/go/memorystore
// v1.7.0 (the first with Memorystore for Valkey's ACL policies, #1578)
// require a later genproto: the root module, whose packages this repo uses
// are all in this version, stays at the last that has it, so `go mod tidy`
// resolves every test. googleapis/api and googleapis/rpc are their own
// modules and stay current.
replace google.golang.org/genproto => google.golang.org/genproto v0.0.0-20260723164925-7274b71286bd
