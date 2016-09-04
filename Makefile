CWD=$(shell pwd)
export GOPATH:=$(CWD)/_vendor:$(CWD)

BIN=httpsocket

.PHONY: clean depends bin/$(BIN)

all: bin/$(BIN)

bin/$(BIN):
	go install $(BIN)


depends:
	go get github.com/gorilla/websocket


clean:
	rm -rf bin/
	rm -rf pkg/
	rm -rf _vendor/bin/
	rm -rf _vendor/pkg/
