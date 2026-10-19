// Package domain holds the entities of the service and their invariants: types, rules
// and errors of the subject area that hold whatever the storage or transport. It imports
// nothing from internal/adapter, pgx, gRPC or generated contract code; adapters convert
// between domain types and rows or messages.
package domain
