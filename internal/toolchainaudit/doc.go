// Package toolchainaudit holds no product code. It exists so that the Go
// version this repository SHIPS and the Go version its supply-chain gate
// GRADES cannot drift apart silently.
//
// # Why this needs an instrument rather than a comment
//
// `go.mod`'s `toolchain` directive is a FLOOR, not a pin. Measured on this
// module, with GOTOOLCHAIN=auto and the directive reading go1.26.5:
//
//	base go1.26.4 (below the floor) -> selects go1.26.5
//	base go1.26.5 (at the floor)    -> selects go1.26.5
//	base go1.26.6 (above the floor) -> selects go1.26.6   <- go.mod does NOT hold it down
//
// Every job in ci.yaml gets its Go from `actions/setup-go`, and the version it
// finally runs is max(setup-go's pin, go.mod's toolchain). That gives the three
// version strings in this repo three different jobs:
//
//   - go.mod `toolchain` is the ONLY one that decides what is SHIPPED. The
//     build job pins setup-go at "1.25", which is below the floor, so the
//     released binary is always built with exactly go.mod's toolchain.
//   - ci.yaml's `vuln` pin decides which stdlib govulncheck GRADES.
//   - ci.yaml's `lint` pin decides which Go golangci-lint is built with.
//
// The hazard is one-directional and it is not hypothetical. Raise the `vuln`
// job's pin ABOVE go.mod's toolchain and govulncheck grades a standard library
// that no job ever builds with: the gate reports green while the released
// binary is still linked against the vulnerable one. That is a supply-chain
// gate that is green precisely when it should be red — the same shape as a
// health probe that reports healthy when the credential is missing.
//
// Both ci.yaml pins already carry comments asserting this coupling ("keep in
// lockstep with go.mod toolchain so stdlib vulns are assessed against the
// shipped runtime", "Must be >= go.mod's toolchain"). A comment cannot fail.
// The test beside this file is those two sentences made mechanical.
//
// # What it does NOT claim
//
// It does not check that the pinned version is current, or free of advisories
// — that is govulncheck's job and govulncheck already runs. It checks only
// that the grader and the shipped runtime are the same version, so that when
// govulncheck says green, the thing it said that about is the thing that ships.
package toolchainaudit
