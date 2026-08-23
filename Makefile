GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

BINARY := magneto
ifeq ($(GOOS),windows)
BINARY := magneto.exe
endif

IMAGE := magneto:latest
DOCKER_PLATFORM := linux/$(GOARCH)

.PHONY: build install docker clean

build:
	go build -o bin/$(BINARY) .

install:
	go install .

docker:
	docker build --platform $(DOCKER_PLATFORM) -t $(IMAGE) .

clean:
	rm -rf bin/
