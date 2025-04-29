#!/bin/bash
set -e

# Build the plugin
go build -o protoc-gen-openapi

# Create a test directory
mkdir -p test-proto3-optional/proto
cd test-proto3-optional

# Create a test proto file that uses proto3 optional fields
cat > proto/test.proto << EOF
syntax = "proto3";
package test;

option go_package = "github.com/test/proto3optional";

message TestMessage {
  string required_field = 1;
  optional string optional_field = 2;
}

service TestService {
  rpc TestMethod(TestMessage) returns (TestMessage) {}
}
EOF

# Run protoc with our plugin without support_proto3_optional
echo "Testing without proto3 optional support:"
PATH=$PATH:$(pwd)/.. protoc --plugin=protoc-gen-openapi=$(pwd)/../protoc-gen-openapi \
  --openapi_out=. proto/test.proto || echo "Failed as expected without proto3 optional support"

# Run protoc with our plugin with support_proto3_optional
echo -e "\nTesting with proto3 optional support:"
PATH=$PATH:$(pwd)/.. protoc --plugin=protoc-gen-openapi=$(pwd)/../protoc-gen-openapi \
  --openapi_out=support_proto3_optional=true:. proto/test.proto

# Show the output
echo -e "\nGenerated OpenAPI spec with proto3 optional support:"
cat test.json

cd ..
rm -rf test-proto3-optional 