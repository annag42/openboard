// +build tools

package tools

//go:generate go install github.com/codemodus/withdraw
//go:generate go install github.com/go-bindata/go-bindata/go-bindata
//go:generate go install google.golang.org/protobuf/cmd/protoc-gen-go

import (
	_ "github.com/codemodus/withdraw"
	_ "github.com/go-bindata/go-bindata/go-bindata"
	_ "google.golang.org/protobuf/cmd/protoc-gen-go"
)

//go:generate go mod tidy
