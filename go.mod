module github.com/BegoniaHe/wokbuddy-demon

go 1.27

require (
	github.com/redis/go-redis/v9 v9.23.0
	github.com/refraction-networking/utls v1.8.3-0.20261006222701-ff1b50fbbe9a
	go.uber.org/goleak v1.3.0
)

// TLS ClientHello 指纹复刻使用本地 vendored uTLS（非 GitHub fork），
// 维护与本地补丁见 internal/vendor/utls/README.md。
replace github.com/refraction-networking/utls => ./internal/vendor/utls

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/klauspost/compress v1.20.4 // indirect
	github.com/molecule-man/go-brrr v1.3.1 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/tools v0.52.0 // indirect
)
