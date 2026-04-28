# Set IMAGE to your Docker Hub repo (e.g. IMAGE=dockeruser/workflows-benchmark).
# TAG defaults to latest; override for versioned pushes.
IMAGE ?= workflows-benchmark
TAG   ?= latest

.PHONY: build test vet image push

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

image:
	docker build -t $(IMAGE):$(TAG) .

push: image
	docker push $(IMAGE):$(TAG)
