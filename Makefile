lint-fix: 
	gofmt -d -w .

lint: 
	gofmt -d .

test:
	rm -rf ./core/tmp/
	go test ./...
	go test ./... -bench=.

benchmark:
	drill -b kv_benchmark.yml --stats

start-http-server:
	ENV=dev go run main.go

build:
	go build -race -o ./bin/kv_http_server
