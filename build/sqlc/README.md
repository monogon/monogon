Bazel rules for sqlc
===

This is a set of rules which uses [sqlc](https://github.com/kyleconroy/sqlc) to generate Go code (types and functions) based on a SQL schema/migrations and a list of queries to be turned into functions.

Usage
---

In an empty directory (eg. monogon/foo/bar/model), create:

 - Migration files, eg. `1662395623_foo.up.sql` and `1662395623_foo.down.sql` containing CREATE TABLE and DROP TABLE statements respectively.
 - A query file, containing SQL queries annotated with function names and return values (see [official docs](https://docs.sqlc.dev/en/stable/tutorials/getting-started-postgresql.html) for a sample `query.sql` file).
 - A `BUILD.bazel` file containing:

```
load("@rules_go//go:def.bzl", "go_library")
load("//build/sqlc:sqlc.bzl", "sqlc_go_library")

sqlc_go_library(
    name = "sqlc_model",
    importpath = "source.monogon.dev/foo/bar/model",
    migrations = [
        "1662395623_foo.up.sql",
        "1662395623_foo.down.sql",
        # More migrations can be created by provising larger timestamp values.
    ],
    queries = [
        "queries.sql",
    ],
    dialect = "cockroachdb",
)

go_library(
    name = "model",
    importpath = "source.monogon.dev/foo/bar/model",
    embed = [":sqlc_model"],
    deps = [
        # Might need this for CockroachDB UUID types.
        "@com_github_google_uuid//:uuid",
    ],
)
```

The built `go_library ` will contain sqlc functions corresponding to queries defined in `queries.sql` and structures corresponding to database tables (and query parameters/results).

To list the generated files for inspection/debugging, `bazel aquery //foo/bar:sqlc_model` and find files named `db.go`, `model.go` and `queries.sql.go` (or similar, depending on how your query file(s) are named).

Migrations
---

Currently, you need to manually embed the same migrations files using standard //go:embed directives, then you can used them with golang-migrate via en embed.FS/io.FS/iofs-source. See //cloud/apigw/model for an example.
