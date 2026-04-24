BIN_DIR := dist

GO_BUILD := CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"

.PHONY: build listener wrapper clean

build: listener wrapper

listener:
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/zed-remote-listener ./cmd/listener

wrapper:
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/zed-remote-wrapper ./cmd/wrapper

clean:
	rm -rf $(BIN_DIR)
