.PHONY: test e2e run install dev build

build:
	CGO_ENABLED=0 go build -o mygnoscan .

run:
	CGO_ENABLED=0 go run .

install:
	CGO_ENABLED=0 go install .

test:
	go test ./...

# Browser tests. Separate from `test` because they need Node and a browser,
# which the binary never does — see e2e/README.md.
e2e:
	cd e2e && npm ci && npx playwright install --with-deps chromium && npx playwright test

dev:
	goloop . -- go run .
