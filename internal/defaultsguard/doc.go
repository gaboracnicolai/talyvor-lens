// Package defaultsguard holds no production code. It exists so one test can
// import every package that declares an exported Default* scalar and pin the
// value production actually runs at — see defaults_guard_test.go for the
// measurement that produced it.
package defaultsguard
