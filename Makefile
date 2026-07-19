# Etronium-Scdr — Makefile
#
# Генерация proto → Go + swagger.
# Требует: protoc 24+, Go 1.22+, и плагины (см. README.md).

.PHONY: proto proto-go proto-gw proto-swagger all build test clean

PROTO_ROOT := proto
PROTO_FILE := $(PROTO_ROOT)/etronium/v1/etronium.proto
GEN_DIR    := internal/gen
SWAGGER    := docs/openapi/etronium.swagger.json

proto: proto-go proto-gw proto-swagger

proto-go:
	protoc -I $(PROTO_ROOT) -I $(PROTO_ROOT)/third_party \
		--go_out=$(GEN_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(GEN_DIR) --go-grpc_opt=paths=source_relative \
		$(PROTO_FILE)

proto-gw:
	protoc -I $(PROTO_ROOT) -I $(PROTO_ROOT)/third_party \
		--grpc-gateway_out=$(GEN_DIR) --grpc-gateway_opt=paths=source_relative \
		$(PROTO_FILE)

proto-swagger:
	@mkdir -p docs/openapi
	protoc -I $(PROTO_ROOT) -I $(PROTO_ROOT)/third_party \
		--openapiv2_out=docs/openapi \
		$(PROTO_FILE)
	@# protoc-gen-openapiv2 не поддерживает paths=source_relative,
	@# поэтому swagger всегда уходит в docs/openapi/etronium/v1/etronium.swagger.json.
	@# Переносим в удобное место:
	@if [ -f docs/openapi/etronium/v1/etronium.swagger.json ]; then \
		mv docs/openapi/etronium/v1/etronium.swagger.json $(SWAGGER) && \
		rmdir docs/openapi/etronium/v1 docs/openapi/etronium 2>/dev/null || true; \
	fi

build:
	go build ./...

test:
	go test ./...

clean:
	rm -rf $(GEN_DIR) docs/openapi
