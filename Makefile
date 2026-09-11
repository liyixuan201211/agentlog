BINARY := agentlog

.PHONY: build test vet fmt clean install

build:
	go build -o $(BINARY) ./cmd/agentlog

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

install:
	go install ./cmd/agentlog

clean:
	rm -f $(BINARY)
