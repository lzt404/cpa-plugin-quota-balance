NAME := quota-balance
BIN_DIR := bin
EXT := so

.PHONY: build test clean package

build: $(BIN_DIR)/$(NAME).$(EXT)

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

$(BIN_DIR)/$(NAME).$(EXT): | $(BIN_DIR)
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $@ .
	rm -f $(BIN_DIR)/$(NAME).h

test:
	go test -race -v ./...

package:
	bash build.sh

clean:
	rm -rf $(BIN_DIR) dist .build *.so *.dylib *.dll *.h
