module github.com/dev-sssss/tango_backend/server

go 1.24

replace (
	golang.org/x/crypto => github.com/golang/crypto v0.31.0
	golang.org/x/mod => github.com/golang/mod v0.22.0
	golang.org/x/net => github.com/golang/net v0.33.0
	golang.org/x/sync => github.com/golang/sync v0.10.0
	golang.org/x/sys => github.com/golang/sys v0.28.0
	golang.org/x/term => github.com/golang/term v0.27.0
	golang.org/x/text => github.com/golang/text v0.21.0
	golang.org/x/tools => github.com/golang/tools v0.28.0
	golang.org/x/xerrors => github.com/golang/xerrors v0.0.0-20240903120638-7835f813f4da
	google.golang.org/genproto => github.com/googleapis/go-genproto v0.0.0-20241223144023-3abc09e42ca8
	google.golang.org/genproto/googleapis/api => github.com/googleapis/go-genproto/googleapis/api v0.0.0-20241223144023-3abc09e42ca8
	google.golang.org/genproto/googleapis/rpc => github.com/googleapis/go-genproto/googleapis/rpc v0.0.0-20241223144023-3abc09e42ca8
	google.golang.org/grpc => github.com/grpc/grpc-go v1.69.2
	google.golang.org/protobuf => github.com/protocolbuffers/protobuf-go v1.36.1
)

replace gopkg.in/yaml.v3 => github.com/go-yaml/yaml/v3 v3.0.1

replace gopkg.in/check.v1 => github.com/go-check/check v0.0.0-20201130134442-10cb98267c6c

require (
	github.com/gofrs/uuid/v5 v5.3.0
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/gorilla/websocket v1.5.3
	github.com/heroiclabs/nakama-common v1.34.0
	github.com/jackc/pgx/v5 v5.7.2
	github.com/yuin/gopher-lua v1.1.1
	golang.org/x/crypto v0.31.0
	google.golang.org/protobuf v1.36.1
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)
