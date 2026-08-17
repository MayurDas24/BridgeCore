# GraphQL schema (SDL reference)

These `.graphqls` files are the human-readable contract for BridgeCore's
GraphQL API, and are what you hand to a client team.

They are **documentation, not codegen input**. The executable schema is built
programmatically in `graph/schema.go` and `graph/resolver.go`, which means it
is compiled and type-checked alongside the rest of the service: there is no
generated file to keep in sync, no codegen step in the build or CI, and a
resolver that stops matching its field is a compile error rather than a
runtime surprise.

`make graphql-check` verifies the SDL here still matches the live schema by
diffing it against an introspection dump.
