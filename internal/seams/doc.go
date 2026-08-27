// Package seams holds no production code. It exists so the wiring-seam census can
// live outside every package it sweeps — the guard walks all of internal/*, and
// putting it inside one of those packages would make that package special.
//
// See wiring_seam_census_test.go.
package seams
