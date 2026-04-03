.PHONY: test run install dev

run:
	go run .

install:
	go install .

test:
	go test ./...

dev:
	goloop . -- go run .
