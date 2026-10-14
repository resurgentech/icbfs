//go:build tools

// Pins the code generator in go.mod, so go generate is reproducible.
package driveapi

import (
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)
