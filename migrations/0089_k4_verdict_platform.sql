-- 0089_k4_verdict_platform.sql
-- STEP 3 (attested slash). Record the PLATFORM a verdict was produced on. `//go:build` means the same source
-- can compile on one GOOS/GOARCH and fail on another, so a compile verdict is only meaningful with its
-- platform attached. An ATTESTED (talyvor_verified) verdict is emitted by internal/buildverify ONLY when ALL
-- target platforms AGREE (a platform-independent result); the set it agreed across is recorded here, e.g.
-- "go1.25.11 linux/amd64,linux/arm64". Self-reported rows leave it '' (the workspace does not attest a
-- platform). Existing rows default to '' and satisfy the column.
ALTER TABLE k4_mechanical_verdicts
    ADD COLUMN platform TEXT NOT NULL DEFAULT '';
