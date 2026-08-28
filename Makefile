.PHONY: tidy up down run vet test

tidy:
	go mod tidy

up:
	docker compose up -d

down:
	docker compose down

run:
	go run ./cmd/gateway

vet:
	go vet ./...

test:
	go test ./... -count=1
