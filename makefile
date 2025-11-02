dev:
	@go run cmd/main.go
build:
	@go build -o bin/starhane-fm-server cmd/main.go