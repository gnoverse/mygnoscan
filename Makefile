.PHONY: test e2e screenshots run install dev build

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

# Reproducible screenshots of every page, rendered against the e2e fixture into
# docs/images/review/. For reviewing a frontend change without checking the
# branch out — CI uploads them as artifacts on any PR touching frontend/.
#
# Not the README images: those come from scripts/screenshots.sh against a real
# database snapshot, because a README wants a chain with enough history to look
# like something and this wants to be identical run to run.
screenshots:
	cd e2e && npm ci && npx playwright install --with-deps chromium && npm run screenshots

dev:
	goloop . -- go run .
