package main

import "database/sql"

// The reference directory is compiled as a standalone package by `go test
// ./...`; the real db variable is supplied by workspace/app/main.go when the
// reference handler is copied into a running session.
var db *sql.DB
