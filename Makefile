.PHONY: all build test dev web clean

# Point this at a Go >= 1.23 toolchain if it is not on PATH.
GO ?= go

all: build

web:
	cd web && npm install && npm run build

build: web
	$(GO) build -o accesssim .

test:
	$(GO) test -race -count=1 ./...

webtest:
	cd web && npm install && npm test

dev-backend:
	$(GO) run .

dev-web:
	cd web && npm run dev

clean:
	rm -f accesssim
