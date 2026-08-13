// Package pointeraudit holds one guard and no product code: the census of
// SOURCE POINTERS this tree's comments carry, and the rule that a pointer must
// be resolvable.
//
// ⚠ WHY IT EXISTS, MEASURED RATHER THAN ARGUED. A comment citing line 1883 of
// internal/proxy/proxy.go for the `forward` method was exactly right when
// written and is now 620 lines wrong — `forward` sits at 2503 — because
// unrelated edits moved the file. Nobody was careless: the pointer decays with
// NO commit touching the comment, no review catches it, and no test can fail for
// it. A nine-pointer sample of this tree's citations into that one file found
// SIX landing somewhere else, two of them in the table that documents where the
// prompt rewriter does and does not move a customer's bill.
//
// The fix is not better numbers — a correct line number rots identically. It is
// the SYMBOL, which survives edits and cannot come to point at a different
// declaration. Rule A enforces that any symbol citation resolves. Rule B is the
// census of the line citations that remain, pinned per citing file, so a NEW one
// fails and a converted one has to lower the pin in a reviewable diff.
//
// ⚠ THE EXAMPLES ABOVE ARE DELIBERATELY NOT WRITTEN IN CITATION FORM, and that is
// this guard's own first finding about itself: an illustrative pointer in the
// documentation of the scanner is indistinguishable from a real one, and its
// first run reddened on its own prose. The alternative — excluding this package
// from the walk — is a hole in the only instrument that would see one.
package pointeraudit
