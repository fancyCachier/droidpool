.PHONY: dist test deploy
dist:
	mkdir -p dist
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/droidpoold-linux-amd64 ./cmd/droidpoold
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/droidpool-linux-amd64  ./cmd/droidpool
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/droidpool-linux-arm64  ./cmd/droidpool
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/droidpool-darwin-arm64 ./cmd/droidpool
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/droidpool-mcp-linux-amd64  ./cmd/droidpool-mcp
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/droidpool-mcp-linux-arm64  ./cmd/droidpool-mcp
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/droidpool-mcp-darwin-arm64 ./cmd/droidpool-mcp
test:
	go vet ./... && go test ./... -race -count=1
deploy: dist
	deploy/deploy.sh
