.PHONY: test run install dev build

build:
	CGO_ENABLED=0 go build -o mygnoscan .

run:
	CGO_ENABLED=0 go run .

install:
	CGO_ENABLED=0 go install .

test:
	go test ./...

dev:
	goloop . -- go run .
