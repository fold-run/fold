package config

import _ "embed"

// schemaJSON is the machine-readable contract for the configuration
// document, shipped with the binary (`fold --schema`) and kept in lockstep
// with the structs in this package by TestSchemaMatchesStructs.
//
//go:embed fold.config.schema.json
var schemaJSON []byte

// Schema returns the JSON Schema (draft-07) for the fold configuration
// document. It describes the structural contract — field names, types,
// enums, required fields; cross-field rules remain the job of Validate.
func Schema() []byte { return schemaJSON }
