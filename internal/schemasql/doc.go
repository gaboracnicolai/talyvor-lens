// Package schemasql holds no production code. It exists so the schema/SQL parity
// guard can live in a package of its own rather than inside one of the packages it
// sweeps — the guard walks every non-test .go file in the repository, and putting it
// under (say) internal/dbmigrate would make one swept package special.
//
// See schema_sql_parity_test.go.
package schemasql
